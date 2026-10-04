package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editorFile(t *testing.T, body string) (*Editor, string) {
	t.Helper()
	path := writeFile(t, "config.yaml", body)
	return NewEditor(path), path
}

func TestEditorRoundTripPreservesCommentsAndRefs(t *testing.T) {
	t.Setenv("QBTW_TEST_API_KEY", "testkey123")
	body := "# top comment\n" +
		"qbt_url: \"http://host\"\n" +
		"qbt_api_key: \"${QBTW_TEST_API_KEY}\"  # secret stays literal\n" +
		"policies:\n" +
		"  # policy comment\n" +
		"  stalled_no_seeders:\n" +
		"    action: \"warn\"\n" +
		"    threshold: \"30m\"\n"
	e, path := editorFile(t, body)
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	action := "delete"
	if _, err := e.Save(raw, stamp, Patch{Policies: map[string]PolicyPatch{
		"stalled_no_seeders": {Action: &action},
	}}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "# top comment") {
		t.Fatal("top comment lost", s)
	}
	if !strings.Contains(s, "# policy comment") {
		t.Fatal("policy comment lost", s)
	}
	if !strings.Contains(s, "${QBTW_TEST_API_KEY}") {
		t.Fatal("env reference lost or resolved", s)
	}
	if strings.Contains(s, "testkey123") {
		t.Fatal("env reference was resolved into the file", s)
	}
	if !strings.Contains(s, "delete") {
		t.Fatal("policy action not changed", s)
	}
	if !(strings.Index(s, "qbt_url") < strings.Index(s, "qbt_api_key") &&
		strings.Index(s, "qbt_api_key") < strings.Index(s, "policies")) {
		t.Fatal("key order not preserved", s)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Policies[StalledNoSeeders].Action != Delete {
		t.Fatal("action not applied", c.Policies[StalledNoSeeders].Action)
	}
}

func TestEditorConflict(t *testing.T) {
	e, path := editorFile(t, minimal)
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(minimal+"log_level: 'debug'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	level := "info"
	if _, err := e.Save(raw, stamp, Patch{LogLevel: &level}); !errors.Is(err, ErrConflict) {
		t.Fatal("expected ErrConflict, got", err)
	}
}

func TestEditorAtomicWritePreservesModeAndCleansUp(t *testing.T) {
	e, path := editorFile(t, minimal)
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	level := "debug"
	if _, err := e.Save(raw, stamp, Patch{LogLevel: &level}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("file mode not preserved", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".qbt-watchdog-edit-") {
			t.Fatal("temp file left behind", entry.Name())
		}
	}
}

func TestEditorValidationRejectsInvalidValue(t *testing.T) {
	e, path := editorFile(t, minimal)
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	format := "bogus"
	if _, err := e.Save(raw, stamp, Patch{LogFormat: &format}); err == nil {
		t.Fatal("expected validation error")
	} else if !strings.Contains(err.Error(), "log_format") {
		t.Fatal("expected log_format validation error, got", err)
	}
	// Validation happens before the write, so the rejected value never lands
	// on disk and the previous document is untouched.
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "bogus") {
		t.Fatal("rejected value was written", string(out))
	}
	if string(out) != minimal {
		t.Fatal("document changed on a rejected save", string(out))
	}
	if _, err := Load(path); err != nil {
		t.Fatal("valid document was damaged by a rejected save", err)
	}
}

func TestEditorCreatesMissingKey(t *testing.T) {
	e, path := editorFile(t, minimal)
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	level := "debug"
	if _, err := e.Save(raw, stamp, Patch{LogLevel: &level}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "log_level") {
		t.Fatal("log_level not created", string(out))
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "debug" {
		t.Fatal("log_level not applied", c.LogLevel)
	}
}

func TestEditorSaveRaw(t *testing.T) {
	t.Setenv("QBTW_TEST_API_KEY", "testkey123")
	body := "# top comment\nqbt_url: 'http://host'\nqbt_api_key: '${QBTW_TEST_API_KEY}'\n"
	e, path := editorFile(t, body)
	_, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}

	// Writes the edited text verbatim: refs stay literal, comments survive.
	edited := "# top comment\nqbt_url: 'http://host'\nqbt_api_key: '${QBTW_TEST_API_KEY}'\nlog_level: 'debug'\n"
	if _, err := e.SaveRaw([]byte(edited), stamp); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != edited {
		t.Fatal("edited text not written verbatim", string(out))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("file mode not preserved", info.Mode().Perm())
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "debug" {
		t.Fatal("edited setting not applied", c.LogLevel)
	}

	// A stale stamp is refused without writing.
	if _, err := e.SaveRaw([]byte("qbt_url: 'http://other'\n"), stamp); !errors.Is(err, ErrConflict) {
		t.Fatal("expected ErrConflict, got", err)
	}

	// Invalid YAML is rejected before the write, so the valid document stays.
	_, freshStamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SaveRaw([]byte("qbt_url: [unclosed\n"), freshStamp); err == nil {
		t.Fatal("expected validation error for invalid YAML")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "[unclosed") {
		t.Fatal("invalid yaml was written", string(written))
	}
	if string(written) != edited {
		t.Fatal("valid document changed on a rejected raw save", string(written))
	}
	if _, err := Load(path); err != nil {
		t.Fatal("valid document was damaged by a rejected raw save", err)
	}
}

func TestEditorCreatesNestedPolicyAndIntegration(t *testing.T) {
	e, path := editorFile(t, minimal)
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	action := "delete"
	seconds := 1800
	mode := "search_only"
	if _, err := e.Save(raw, stamp, Patch{
		Policies:     map[string]PolicyPatch{"stalled_no_seeders": {Action: &action, ThresholdSeconds: &seconds}},
		Integrations: map[string]IntegrationPatch{"sonarr": {Mode: &mode}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "policies") || !strings.Contains(s, "stalled_no_seeders") {
		t.Fatal("policies block not created", s)
	}
	if !strings.Contains(s, "integrations") || !strings.Contains(s, "sonarr") {
		t.Fatal("integrations block not created", s)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Policies[StalledNoSeeders].Action != Delete || c.Policies[StalledNoSeeders].Threshold != 1800e9 {
		t.Fatal("policy not applied", c.Policies[StalledNoSeeders])
	}
	if c.Integrations.Sonarr.Mode != SearchOnly {
		t.Fatal("integration mode not applied", c.Integrations.Sonarr.Mode)
	}
}
