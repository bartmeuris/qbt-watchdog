package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = "qbt_url: 'http://host'\n"

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decode(t *testing.T, body string) Config {
	t.Helper()
	c, err := Decode(YAML, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultsAndMinimalCommandLine(t *testing.T) {
	path := writeFile(t, "config.yaml", minimal)
	c, err := Parse([]string{"--config", path, "--once"}, func(string) (string, bool) { return "INVALID_SECRET", true }, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.DryRun || !c.Once || !c.MetricsPublic {
		t.Fatal("unsafe defaults")
	}
	if c.PollInterval != 30*time.Second || c.MaxObservationGap != 90*time.Second || c.HTTPTimeout != 10*time.Second {
		t.Fatal("incorrect interval defaults")
	}
	if c.DeleteConfirmationTimeout != 2*time.Minute || c.UIRefreshInterval != 5*time.Second || c.ReadinessMaxAge != 2*time.Minute {
		t.Fatal("incorrect timeout defaults")
	}
	if c.MaxDeletions != 10 || c.HistoryLimit != 100 || c.Listen != ":8080" || c.StateFile != "/data/state.json" {
		t.Fatal("incorrect scalar defaults")
	}
	if c.LogLevel != "info" || c.LogFormat != "json" || len(c.ExcludeTags) != 2 {
		t.Fatal("incorrect presentation defaults")
	}
	for _, id := range PolicyIDs() {
		if c.Policies[id] != (Policy{Warn, 30 * time.Minute}) {
			t.Fatal("policy default is not warn/30m", id)
		}
	}
	if c.ConfigFile != path {
		t.Fatal("configuration path not absolute", c.ConfigFile)
	}
}

func TestConfigurationPathPrecedence(t *testing.T) {
	fromFlag := writeFile(t, "flag.yaml", minimal+"poll_interval: '11s'")
	fromEnv := writeFile(t, "env.yaml", minimal+"poll_interval: '12s'")
	environment := func(key string) (string, bool) {
		if key == "QBTW_CONFIG" {
			return fromEnv, true
		}
		return "unused", true
	}
	c, err := Parse([]string{"--config", fromFlag}, environment, &bytes.Buffer{})
	if err != nil || c.PollInterval != 11*time.Second {
		t.Fatal("explicit --config must win over QBTW_CONFIG", err)
	}
	c, err = Parse(nil, environment, &bytes.Buffer{})
	if err != nil || c.PollInterval != 12*time.Second {
		t.Fatal("QBTW_CONFIG ignored", err)
	}
}

func TestRemovedFlagAndEnvironmentSurface(t *testing.T) {
	path := writeFile(t, "config.yaml", minimal)
	for _, args := range [][]string{
		{"--dry-run=false"}, {"--qbt-password=SECRET"}, {"--poll-interval=1s"},
		{"--listen=:9090"}, {"--config", path, "extra"},
	} {
		_, err := Parse(args, os.LookupEnv, &bytes.Buffer{})
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("legacy flag accepted or leaked", args, err)
		}
	}
	// Per-setting environment variables must not override the file either.
	c, err := Parse([]string{"--config", path}, func(string) (string, bool) { return "false", true }, &bytes.Buffer{})
	if err != nil || !c.DryRun {
		t.Fatal("per-setting environment applied", err)
	}
}

func TestHelpNeverReadsEnvironmentOrShowsSecrets(t *testing.T) {
	var out bytes.Buffer
	_, err := Parse([]string{"--help"}, func(string) (string, bool) {
		t.Fatal("help read the environment")
		return "SECRET", true
	}, &out)
	if !errors.Is(err, flag.ErrHelp) || strings.Contains(out.String(), "SECRET") || !strings.Contains(out.String(), "config") {
		t.Fatal(err, out.String())
	}
}

func TestFormatDetectionAndEquivalence(t *testing.T) {
	for _, name := range []string{"config.json", "config", "config.ini"} {
		if _, err := FormatFor(name); err == nil {
			t.Fatal("unsupported extension accepted", name)
		}
	}
	yamlPath := writeFile(t, "config.yml", minimal+"poll_interval: '45s'\npolicies:\n  metadata:\n    action: 'delete'\n")
	tomlPath := writeFile(t, "config.toml", "qbt_url = 'http://host'\npoll_interval = '45s'\n[policies.metadata]\naction = 'delete'\n")
	fromYAML, err := Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	fromTOML, err := Load(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	fromYAML.ConfigFile, fromTOML.ConfigFile = "", ""
	if fromYAML.PollInterval != 45*time.Second || fromYAML.Policies[Metadata].Action != Delete {
		t.Fatal("YAML not applied")
	}
	if fromYAML.URL.String() != fromTOML.URL.String() || fromYAML.PollInterval != fromTOML.PollInterval || fromYAML.Policies[Metadata] != fromTOML.Policies[Metadata] {
		t.Fatal("YAML and TOML disagree")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestStrictValidationRejectsEveryInvalidSetting(t *testing.T) {
	for _, body := range []string{
		// endpoint
		"qbt_url: 'ftp://host'", "qbt_url: 'http://user:SECRET@host'", "qbt_url: 'http://host?SECRET=x'",
		"qbt_url: 'http://host#SECRET'", "qbt_url: 'http://host:99999'", "qbt_url: ''", "qbt_url: 'http:///path'",
		// credentials
		"qbt_username: 'u'", "web_password: 'SECRET'", "web_username: 'u'",
		"qbt_password: 'SECRET'\nqbt_password_file: '/SECRET'",
		"web_password: 'SECRET'\nweb_password_file: '/SECRET'",
		"qbt_password_file: '/not/a/SECRET/file'",
		"qbt_username: 'u'\nqbt_password_file: '/not/a/SECRET/file'",
		// caps
		"history_limit: 0", "history_limit: 10001", "max_actions_per_poll: -1", "max_actions_per_poll: 10001",
		"max_actions_per_poll: 1.5",
		// durations
		"poll_interval: '0s'", "poll_interval: '-1s'", "poll_interval: 30", "poll_interval: ''",
		"max_observation_gap: ''", "max_observation_gap: '-1s'", "http_timeout: '0s'",
		"delete_confirmation_timeout: '0s'", "ui_refresh_interval: '0s'", "readiness_max_age: '0s'",
		// types
		"dry_run: 'false'", "dry_run: 0", "metrics_public: 'true'", "tls_insecure_skip_verify: 1",
		"include_categories: 'a,b'", "exclude_tags: [1]", "history_limit: 'many'",
		// presentation and binding
		"log_level: 'trace'", "log_format: 'xml'", "listen: 'invalid'", "listen: ':99999'", "state_file: ''",
		// files
		"tls_ca_file: '/no/such/SECRET/bundle.pem'",
		// unknown keys
		"unknown: 'SECRET'", "api_key: 'SECRET'", "qbt_urls: 'SECRET'",
		// policies
		"policies:\n  unknown:\n    action: 'warn'",
		"policies:\n  metadata:\n    action: 'Warn'",
		"policies:\n  metadata:\n    action: 'remove'",
		"policies:\n  metadata:\n    action: ''",
		"policies:\n  metadata:\n    threshold: ''",
		"policies:\n  metadata:\n    threshold: '0s'",
		"policies:\n  metadata:\n    threshold: 30",
		"policies:\n  metadata:\n    unknown: 'SECRET'",
		"policies: 'metadata'",
	} {
		t.Run(body, func(t *testing.T) {
			if !strings.HasPrefix(body, "qbt_url:") {
				body = minimal + body
			}
			_, err := Decode(YAML, []byte(body))
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("error leaked file content", err)
			}
		})
	}
}

func TestValidEdgeCases(t *testing.T) {
	if c := decode(t, minimal+"max_actions_per_poll: 0"); c.MaxDeletions != 0 {
		t.Fatal("zero action cap must be accepted as take-no-action")
	}
	if c := decode(t, minimal+"listen: ':0'"); c.Listen != ":0" {
		t.Fatal("ephemeral port rejected")
	}
	if c := decode(t, minimal+"poll_interval: '7s'"); c.MaxObservationGap != 21*time.Second {
		t.Fatal("observation gap must default to three poll intervals")
	}
	c := decode(t, minimal+"include_categories: ['  films ', '', 'TV']\nexclude_tags: [' keep ']")
	if len(c.IncludeCategories) != 2 || c.IncludeCategories[0] != "films" || c.IncludeCategories[1] != "TV" {
		t.Fatal("category list not trimmed case sensitively", c.IncludeCategories)
	}
	if len(c.ExcludeTags) != 1 || c.ExcludeTags[0] != "keep" {
		t.Fatal("tag list not trimmed", c.ExcludeTags)
	}
	if c := decode(t, "qbt_url: 'https://host/prefix'"); c.URL.String() != "https://host/prefix/" {
		t.Fatal("base URL not normalised", c.URL)
	}
}

func TestSecretsAreReadFromFilesAndNeverEchoed(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "password")
	if err := os.WriteFile(secret, []byte(" s3cret \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := decode(t, fmt.Sprintf("qbt_url: 'http://host'\nqbt_username: 'u'\nqbt_password_file: %q\n", secret))
	if c.Password != " s3cret " {
		t.Fatalf("only the trailing newline may be stripped: %q", c.Password)
	}
	if err := os.WriteFile(secret, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	c = decode(t, fmt.Sprintf("qbt_url: 'http://host'\nqbt_username: 'u'\nqbt_password_file: %q\n", secret))
	if c.Password != "rotated" {
		t.Fatal("password file not re-read on reload")
	}
	// A secret must never reach an error message, however it fails.
	if err := os.WriteFile(secret, bytes.Repeat([]byte("s3cret"), 20000), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Decode(YAML, []byte(fmt.Sprintf("qbt_url: 'http://host'\nqbt_password_file: %q\n", secret)))
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatal("oversized secret accepted or echoed", err)
	}
}

func TestCloneIsolatesEveryReference(t *testing.T) {
	c := decode(t, minimal+"include_categories: ['a']\nexclude_categories: ['b']\nexclude_tags: ['c']\ntls_ca_file: ''")
	c.TLSCAPEM = []byte("pem")
	clone := c.Clone()
	clone.URL.Host = "other"
	clone.Policies[Metadata] = Policy{DeleteFile, time.Second}
	clone.IncludeCategories[0] = "mutated"
	clone.ExcludeCategories[0] = "mutated"
	clone.ExcludeTags[0] = "mutated"
	clone.TLSCAPEM[0] = 'X'
	if c.URL.Host != "host" || c.Policies[Metadata].Action != Warn {
		t.Fatal("clone shares the URL or policy map")
	}
	if c.IncludeCategories[0] != "a" || c.ExcludeCategories[0] != "b" || c.ExcludeTags[0] != "c" || c.TLSCAPEM[0] != 'p' {
		t.Fatal("clone shares a slice")
	}
}

func TestDryRunOverridesEveryAction(t *testing.T) {
	body := minimal + "policies:\n  metadata:\n    action: 'delete'\n  stalled_no_seeders:\n    action: 'delete_file'\n  stalled_seeders_seen:\n    action: 'delete'\n  stalled_partial:\n    action: 'delete_file'\n"
	c := decode(t, body)
	for _, id := range PolicyIDs() {
		if !c.Policies[id].Action.Destructive() {
			t.Fatal("test fixture is not destructive", id)
		}
		if c.EffectiveAction(id) != Warn {
			t.Fatal("dry_run did not downgrade to warn", id)
		}
	}
	live := decode(t, body+"dry_run: false\n")
	for _, id := range PolicyIDs() {
		if live.EffectiveAction(id) != live.Policies[id].Action {
			t.Fatal("configured action not effective once dry_run is off", id)
		}
	}
}

func TestWarningsCoverEveryUnsafeSetting(t *testing.T) {
	messages := func(c Config) string {
		var b strings.Builder
		for _, w := range c.Warnings() {
			b.WriteString(w.Message)
			b.WriteString(string(w.Policy))
			b.WriteString("|")
		}
		return b.String()
	}
	if got := messages(decode(t, minimal+"listen: '127.0.0.1:8080'")); got != "" {
		t.Fatal("safe defaults warned", got)
	}
	unsafe := decode(t, minimal+"dry_run: false\ntls_insecure_skip_verify: true\npolicies:\n  stalled_partial:\n    action: 'delete_file'\n")
	got := messages(unsafe)
	for _, want := range []string{"DELETION ENABLED", "DELETE FILES ENABLED", string(StalledPartial), "TLS certificate verification disabled", "web UI exposed without authentication"} {
		if !strings.Contains(got, want) {
			t.Fatal("missing warning", want, got)
		}
	}
	loopback := decode(t, minimal+"listen: '127.0.0.1:8080'")
	if strings.Contains(messages(loopback), "web UI") {
		t.Fatal("loopback UI warned")
	}
	for _, tc := range []struct {
		addr   string
		public bool
	}{{":8080", true}, {"0.0.0.0:8080", true}, {"127.0.0.1:8080", false}, {"[::1]:8080", false}, {"localhost:8080", false}} {
		if (Config{Listen: tc.addr}).PublicListen() != tc.public {
			t.Fatal("wrong exposure verdict", tc.addr)
		}
	}
}

func TestRestartRequiredSettings(t *testing.T) {
	current := decode(t, minimal)
	if err := RestartRequiredError(current, current.Clone()); err != nil {
		t.Fatal("unchanged configuration rejected", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"listen", func(c *Config) { c.Listen = ":9090" }, "listen"},
		{"state_file", func(c *Config) { c.StateFile = "/other.json" }, "state_file"},
	} {
		next := current.Clone()
		tc.mutate(&next)
		err := RestartRequiredError(current, next)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "restart") {
			t.Fatal("restart-only change accepted", tc.name, err)
		}
	}
	live := current.Clone()
	live.LogLevel, live.LogFormat, live.Password, live.TLSInsecure = "debug", "text", "new", true
	live.PollInterval, live.UIRefreshInterval, live.ReadinessMaxAge = time.Minute, time.Minute, time.Minute
	live.Policies[Metadata] = Policy{Delete, time.Hour}
	live.ExcludeTags = append(live.ExcludeTags, "extra")
	if err := RestartRequiredError(current, live); err != nil {
		t.Fatal("live-applicable change rejected", err)
	}
}

func TestExampleConfigurationIsSafeAndComplete(t *testing.T) {
	body, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Never consult a developer's adjacent .env while checking the example.
	c, err := Decode(YAML, body)
	if err != nil {
		t.Fatal(err)
	}
	if !c.DryRun {
		t.Fatal("example disables dry_run")
	}
	for _, id := range PolicyIDs() {
		if c.Policies[id] != (Policy{Warn, 30 * time.Minute}) || c.EffectiveAction(id) != Warn {
			t.Fatal("example configures an unsafe policy", id)
		}
	}
	// The example binds every interface, which is the only warning it may
	// produce; nothing destructive may be enabled.
	for _, warning := range c.Warnings() {
		if !strings.Contains(warning.Message, "web UI") {
			t.Fatal("example is unsafe", warning)
		}
	}
	// Every documented key must actually be present in the example.
	for _, key := range []string{
		"qbt_url", "qbt_username", "qbt_password", "qbt_password_file", "poll_interval",
		"max_observation_gap", "http_timeout", "max_actions_per_poll", "delete_confirmation_timeout",
		"include_categories", "exclude_categories", "exclude_tags", "state_file", "history_limit",
		"listen", "ui_refresh_interval", "readiness_max_age", "web_username", "web_password",
		"web_password_file", "metrics_public", "tls_ca_file", "tls_insecure_skip_verify",
		"log_level", "log_format", "dry_run", "policies",
	} {
		if !bytes.Contains(body, []byte("\n"+key+":")) {
			t.Fatal("example omits a setting", key)
		}
	}
	for _, id := range PolicyIDs() {
		if !bytes.Contains(body, []byte("  "+string(id)+":")) {
			t.Fatal("example omits a policy", id)
		}
	}
	if !bytes.Contains(body, []byte("RESTART REQUIRED")) || !bytes.Contains(body, []byte("APPLIED LIVE")) {
		t.Fatal("example does not document reload behaviour")
	}
}
