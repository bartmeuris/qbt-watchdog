package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnvironmentStringSyntax(t *testing.T) {
	t.Parallel()
	environment := map[string]string{"NAME": "value", "EMPTY": "", "OPAQUE": "${UNSET}$$$HOME$(exit 1)`exit 1`", "Mixed_1": "case-sensitive"}
	for _, tc := range []struct{ input, want string }{
		{"literal: \"quoted\"\n#value", "literal: \"quoted\"\n#value"},
		{"$HOME $ $(exit 1) `exit 1`", "$HOME $ $(exit 1) `exit 1`"},
		{"${NAME}", "value"}, {"a${NAME}/${Mixed_1}z", "avalue/case-sensitivez"},
		{"${EMPTY}", ""}, {"$$", "$"}, {"$$$$", "$$"},
		{"$${UNSET}", "${UNSET}"}, {"$$${NAME}", "$value"},
		{"${OPAQUE}", environment["OPAQUE"]},
	} {
		got, err := expandString(tc.input, environment)
		if err != nil || got != tc.want {
			t.Fatal("unexpected expansion", err)
		}
	}
	for _, input := range []string{
		"${SECRET_MISSING}", "${name}", "${", "${NAME", "${}", "${1NAME}",
		"${NAME:-SECRET_DEFAULT}", "${NAME+SECRET_DEFAULT}", "${NAME?SECRET_ERROR}",
		"${NAME SECRET}", "${NAME\nSECRET}", "${${NAME}}", "${NAME/SECRET/x}", "${é}",
	} {
		if _, err := expandString(input, environment); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("malformed/unset reference accepted or leaked", err)
		}
	}
}

func TestEnvironmentDecodeBothFormats(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"ENDPOINT": "https://host/qbt", "USER": "user", "PASSWORD": "SECRET: 'quoted'\n\"\ndry_run: false\npolicies: {} #$value",
		"CATEGORY": "one, two: \"three\"", "ACTION": "delete", "THRESHOLD": "9m", "INTERVAL": "11s",
		"QBTW_DRY_RUN": "false", "dry_run": "false", "qbt_url": "https://ignored",
	}
	for _, tc := range []struct {
		format Format
		body   string
	}{
		{YAML, "qbt_url: '${ENDPOINT}'\nqbt_username: '${USER}'\nqbt_password: '${PASSWORD}'\npoll_interval: '${INTERVAL}'\ninclude_categories: ['${CATEGORY}', 'literal']\npolicies:\n  metadata:\n    action: '${ACTION}'\n    threshold: '${THRESHOLD}'\n"},
		{TOML, "qbt_url = '${ENDPOINT}'\nqbt_username = '${USER}'\nqbt_password = '${PASSWORD}'\npoll_interval = '${INTERVAL}'\ninclude_categories = ['${CATEGORY}', 'literal']\n[policies.metadata]\naction = '${ACTION}'\nthreshold = '${THRESHOLD}'\n"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			c, err := DecodeWithEnvironment(tc.format, []byte(tc.body), environment)
			if err != nil {
				t.Fatal(err)
			}
			if c.Password != environment["PASSWORD"] || c.Username != "user" || c.URL.String() != "https://host/qbt/" || !c.DryRun || c.PollInterval != 11*time.Second {
				t.Fatal("resolved string changed configuration structure or literal types")
			}
			if !reflect.DeepEqual(c.IncludeCategories, []string{environment["CATEGORY"], "literal"}) || !reflect.DeepEqual(c.Policies[Metadata], Policy{Action: Delete, Threshold: 9 * time.Minute, ArrMode: InheritArrMode}) {
				t.Fatal("nested strings or string list did not expand uniformly")
			}
			if _, err := Decode(tc.format, []byte(tc.body)); err == nil {
				t.Fatal("pure Decode accepted unresolved placeholders")
			}
		})
	}
	plain := minimal + "qbt_username: user\nqbt_password: '$value#literal'\n"
	before := decode(t, plain)
	after, err := DecodeWithEnvironment(YAML, []byte(plain), environment)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("environment overrode literal configuration", err)
	}
}

func TestEnvironmentExpansionPreservesStrictBoundary(t *testing.T) {
	t.Parallel()
	environment := map[string]string{"VALUE": "false", "NUMBER": "3", "KEY": "dry_run", "POLICY": "metadata", "SECRET": "SECRET_INVALID"}
	for _, tc := range []struct{ yaml, toml string }{
		{"dry_run: '${VALUE}'", "dry_run = '${VALUE}'"},
		{"history_limit: '${NUMBER}'", "history_limit = '${NUMBER}'"},
		{"qbt_password: 42", "qbt_password = 42"},
		{"exclude_tags: [1]", "exclude_tags = [1]"},
		{"exclude_tags: '${VALUE}'", "exclude_tags = '${VALUE}'"},
		{"unknown: '${SECRET}'", "unknown = '${SECRET}'"},
		{"'${KEY}': false", "'${KEY}' = false"},
		{"policies:\n  '${POLICY}':\n    action: warn", "[policies.'${POLICY}']\naction = 'warn'"},
		{"policies:\n  metadata:\n    unknown: '${SECRET}'", "[policies.metadata]\nunknown = '${SECRET}'"},
	} {
		for format, body := range map[Format]string{YAML: minimal + tc.yaml, TOML: "qbt_url = 'http://host'\n" + tc.toml} {
			if _, err := DecodeWithEnvironment(format, []byte(body), environment); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("type/unknown-key validation weakened or leaked", err)
			}
		}
	}
	if _, err := DecodeWithEnvironment(YAML, []byte("qbt_url: [${SECRET}"), environment); err == nil || err.Error() != "invalid yaml syntax" {
		t.Fatal("expansion ran before parsing", err)
	}
}

func TestEnvironmentEmptyCredentialsCannotEnableBypass(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"qbt_username", "qbt_password", "qbt_password_file", "qbt_api_key", "qbt_api_key_file"} {
		if _, err := DecodeWithEnvironment(YAML, []byte(minimal+key+": '${EMPTY}'\n"), map[string]string{"EMPTY": ""}); err == nil {
			t.Fatal("empty referenced credential silently enabled bypass", key)
		}
	}
	if _, err := DecodeWithEnvironment(YAML, []byte(minimal+"qbt_username: '${EMPTY}'\nqbt_password: '${EMPTY}'\n"), map[string]string{"EMPTY": ""}); err == nil {
		t.Fatal("empty credential pair silently enabled bypass")
	}
	if _, err := DecodeWithEnvironment(YAML, []byte(minimal+"include_categories: ['${EMPTY}']\n"), map[string]string{"EMPTY": ""}); err != nil {
		t.Fatal("empty non-credential value rejected", err)
	}
	if _, err := DecodeWithEnvironment(YAML, []byte(minimal+"qbt_api_key: '${KEY}'\n"), map[string]string{"KEY": "SECRET\nHEADER"}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("invalid API header value accepted or leaked", err)
	}
}

func TestEnvironmentSecretFilesAndAuthenticationConflicts(t *testing.T) {
	t.Parallel()
	keyPath := writeFile(t, "key", "SECRET_${LITERAL}$$#value\n")
	for _, source := range []string{"qbt_api_key_file", "qbt_password_file"} {
		body := minimal + source + ": '${FILE}'\n"
		if source == "qbt_password_file" {
			body += "qbt_username: user\n"
		}
		c, err := DecodeWithEnvironment(YAML, []byte(body), map[string]string{"FILE": keyPath})
		if err != nil || c.APIKey+c.Password != "SECRET_${LITERAL}$$#value" {
			t.Fatal("secret-file mode changed or contents expanded", err)
		}
	}
	for _, extra := range []string{"qbt_api_key_file: '${FILE}'", "qbt_username: user", "qbt_password: '${KEY}'", "qbt_password_file: '${FILE}'"} {
		_, err := DecodeWithEnvironment(YAML, []byte(minimal+"qbt_api_key: '${KEY}'\n"+extra), map[string]string{"KEY": "SECRET_KEY", "FILE": keyPath})
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), keyPath) {
			t.Fatal("authentication conflict accepted or leaked", err)
		}
	}
}

func TestEnvironmentNestedHookIsPure(t *testing.T) {
	t.Parallel()
	input := map[string]any{"future": []any{map[string]any{"${KEY}": "${VALUE}", "number": json.Number("9007199254740993"), "enabled": true}}}
	want := map[string]any{"future": []any{map[string]any{"${KEY}": "SECRET", "number": json.Number("9007199254740993"), "enabled": true}}}
	before, _ := json.Marshal(input)
	got, err := expandValues(input, map[string]string{"VALUE": "SECRET"})
	after, _ := json.Marshal(input)
	if err != nil || !reflect.DeepEqual(got, want) || string(before) != string(after) {
		t.Fatal("generic expansion changed keys, number precision, types or input", err)
	}
}

func TestLoadEnvironmentPrecedenceAndOptionalDotEnv(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "config.yaml", minimal+"qbt_api_key: '${KEY}'\n")
	dotenv := filepath.Join(filepath.Dir(path), ".env")
	load := func(environment map[string]string, want string, wantError bool) {
		t.Helper()
		c, err := LoadWithEnvironment(path, environment)
		if (err != nil) != wantError || !wantError && (c.APIKey != want || c.ConfigFile != path) {
			t.Fatal("unexpected load outcome", err)
		}
	}
	load(nil, "", true)
	load(map[string]string{"KEY": "process"}, "process", false)
	putFixture(t, dotenv, "")
	load(map[string]string{"KEY": "process"}, "process", false)
	putFixture(t, dotenv, "KEY=file\nQBTW_CONFIG=/ignored\nQBTW_DRY_RUN=false\n")
	load(nil, "file", false)
	load(map[string]string{"KEY": "process"}, "process", false)
	load(map[string]string{"KEY": ""}, "", true)
	putFixture(t, dotenv, "KEY=rotated\n")
	load(nil, "rotated", false)
	load(map[string]string{"KEY": "process"}, "process", false)
	if err := os.Remove(dotenv); err != nil {
		t.Fatal(err)
	}
	load(nil, "", true)
	load(map[string]string{"KEY": "process"}, "process", false)
}

func TestLoadUsesConfigDirectoryNotWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	putFixture(t, filepath.Join(cwd, ".env"), "KEY=wrong-directory\n")
	t.Chdir(cwd)
	path := writeFile(t, "config.yaml", minimal+"qbt_api_key: '${KEY}'\n")
	putFixture(t, filepath.Join(filepath.Dir(path), ".env"), "KEY=adjacent\n")
	relative, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithEnvironment(relative, nil)
	if err != nil || c.APIKey != "adjacent" || c.ConfigFile != path {
		t.Fatal("dotenv was not relative to resolved config location", err)
	}
}

func TestEnvironmentSnapshotAndConcurrentLoads(t *testing.T) {
	t.Parallel()
	const fixtureName = "QBTW_TEST_DOTENV_NO_GLOBAL_MUTATION_92753"
	before, existed := os.LookupEnv(fixtureName)
	entries := []string{"KEY=process=with=equals", "EMPTY=", "INVALID"}
	process := environmentSnapshot(entries)
	entries[0] = "KEY=changed"
	if process["KEY"] != "process=with=equals" || len(process) != 2 {
		t.Fatal("process snapshot was not detached")
	}
	path := writeFile(t, "config.yaml", minimal+"qbt_api_key: '${KEY}'\n")
	putFixture(t, filepath.Join(filepath.Dir(path), ".env"), fmt.Sprintf("KEY=file\n%s=SECRET\n", fixtureName))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				c, err := LoadWithEnvironment(path, process)
				if err != nil || c.APIKey != "process=with=equals" {
					t.Error("concurrent environment resolution failed", err)
				}
			}
		}()
	}
	wg.Wait()
	if after, exists := os.LookupEnv(fixtureName); after != before || exists != existed || len(process) != 2 || process["KEY"] != "process=with=equals" {
		t.Fatal("dotenv mutated process environment or injected snapshot")
	}
}

func putFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
