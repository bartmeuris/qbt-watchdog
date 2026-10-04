package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editorWithEnv writes a config document and an adjacent .env into one temp
// directory so the loader's environment precedence can be exercised.
func editorWithEnv(t *testing.T, name, body, dotenv string) (*Editor, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return NewEditor(path), path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSettingsReadModelReportsSourcesAndSecrets(t *testing.T) {
	body := "dry_run: false\n" +
		"qbt_url: 'http://host'\n" +
		"qbt_api_key: '${MY_KEY}'\n" +
		"policies:\n" +
		"  stalled_no_seeders:\n" +
		"    action: delete\n" +
		"    threshold: 45m\n"
	e, _ := editorWithEnv(t, "config.yaml", body, "MY_KEY=secret\n")
	settings, err := e.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.General.DryRun.Source != SourceExplicit || settings.General.DryRun.Value != false {
		t.Fatal("explicit dry_run not reported", settings.General.DryRun)
	}
	if settings.General.PollInterval.Source != SourceDefault || settings.General.PollInterval.Value != "30s" {
		t.Fatal("defaulted poll_interval not reported", settings.General.PollInterval)
	}
	policy := settings.Policies["stalled_no_seeders"]
	if policy.Action.Source != SourceExplicit || policy.Action.Value != "delete" {
		t.Fatal("explicit policy action not reported", policy.Action)
	}
	if policy.ThresholdSeconds.Value != "45m" || policy.ThresholdSeconds.Effective != 2700 {
		t.Fatal("threshold source/effective mismatch", policy.ThresholdSeconds)
	}
	if policy.ArrMode.Source != SourceInherited {
		t.Fatal("absent arr_mode should be inherited", policy.ArrMode)
	}
	apiKey := secretByKey(t, settings.Secrets, "qbt_api_key")
	if apiKey.Source != SecretEnv || apiKey.EnvName != "MY_KEY" || !apiKey.Configured {
		t.Fatal("env secret source not inferred", apiKey)
	}
	if strings.Contains(apiKey.Path, "secret") || apiKey.Error != "" {
		t.Fatal("secret metadata leaked or errored", apiKey)
	}
}

func TestSettingsSurvivesMissingEnvironmentReference(t *testing.T) {
	body := "qbt_url: 'http://host'\nqbt_api_key: '${DEFINITELY_MISSING}'\n"
	e, _ := editorWithEnv(t, "config.yaml", body, "")
	settings, err := e.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Errors) == 0 {
		t.Fatal("expected a decode error to be reported")
	}
	apiKey := secretByKey(t, settings.Secrets, "qbt_api_key")
	if apiKey.Source != SecretEnv || apiKey.EnvName != "DEFINITELY_MISSING" {
		t.Fatal("broken reference not visible", apiKey)
	}
	if apiKey.Error == "" {
		t.Fatal("missing environment variable not flagged")
	}
	// The source value is still readable so the editor can repair it.
	if settings.Connections.QBTURL.Value != "http://host" {
		t.Fatal("source values unavailable when decode fails", settings.Connections.QBTURL)
	}
}

func TestPatchPreservesUntouchedSettingsAndComments(t *testing.T) {
	body := "# top comment\n" +
		"qbt_url: 'http://host'  # endpoint\n" +
		"qbt_api_key: '${MY_KEY}'\n" +
		"policies:\n" +
		"  # policy comment\n" +
		"  stalled_no_seeders:\n" +
		"    action: warn\n"
	e, path := editorWithEnv(t, "config.yaml", body, "MY_KEY=secret\n")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	level := "debug"
	if _, err := e.Save(raw, stamp, Patch{LogLevel: &level}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	for _, want := range []string{"# top comment", "# endpoint", "# policy comment", "${MY_KEY}", "action: warn"} {
		if !strings.Contains(out, want) {
			t.Fatalf("untouched content lost: %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "log_level: debug") {
		t.Fatal("patched value missing", out)
	}
}

func TestPatchExplicitZeroFalseAndEmptyList(t *testing.T) {
	body := "qbt_url: 'http://host'\ndry_run: true\nmax_actions_per_poll: 5\nexclude_tags:\n  - keep\n"
	e, path := editorWithEnv(t, "config.yaml", body, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	off := false
	empty := []string{}
	if _, err := e.Save(raw, stamp, Patch{DryRun: &off, MaxActionsPerPoll: &zero, ExcludeTags: &empty}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DryRun || c.MaxDeletions != 0 || len(c.ExcludeTags) != 0 {
		t.Fatal("explicit zero/false/empty not applied", c.DryRun, c.MaxDeletions, c.ExcludeTags)
	}
}

func TestPatchSecretsKeepReplaceClearAndSwitch(t *testing.T) {
	t.Setenv("MY_KEY", "secret")
	e, path := editorWithEnv(t, "config.yaml", "qbt_url: 'http://host'\n", "")
	secretPath := filepath.Join(filepath.Dir(path), "qbt.key")
	if err := os.WriteFile(secretPath, []byte("filekey\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	// Start from a path source.
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_api_key": {Mode: "replace", Source: "path", Path: &secretPath},
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "qbt_api_key_file") {
		t.Fatal("path source not written")
	}
	// keep: an omitted secret is untouched.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Save(raw, stamp, Patch{LogLevel: strPtr("debug")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "qbt_api_key_file") {
		t.Fatal("omitted secret was cleared")
	}
	// switch path -> env removes the file key atomically.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_api_key": {Mode: "replace", Source: "env", Env: strPtr("MY_KEY")},
	}}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	if strings.Contains(out, "qbt_api_key_file") {
		t.Fatal("conflicting file source not removed", out)
	}
	if !strings.Contains(out, "${MY_KEY}") {
		t.Fatal("env source not written", out)
	}
	// clear removes both.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_api_key": {Mode: "clear"},
	}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, path), "qbt_api_key") {
		t.Fatal("cleared secret still present")
	}
}

func TestPatchSecretValueEscapesLiteralDollar(t *testing.T) {
	body := "qbt_url: 'http://host'\nqbt_username: 'operator'\n"
	e, path := editorWithEnv(t, "config.yaml", body, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	literal := "pa$$word${NAME}"
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_password": {Mode: "replace", Source: "value", Value: &literal},
	}}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != literal {
		t.Fatalf("literal dollar not preserved: %q", c.Password)
	}
}

func TestInvalidCandidateDoesNotOverwriteValidConfig(t *testing.T) {
	e, path := editorWithEnv(t, "config.yaml", minimal, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	bad := "bogus"
	if _, err := e.Save(raw, stamp, Patch{LogFormat: &bad}); err == nil {
		t.Fatal("expected validation error")
	}
	if readFile(t, path) != minimal {
		t.Fatal("invalid candidate overwrote the valid document")
	}
}

func TestStaleStampConflicts(t *testing.T) {
	e, path := editorWithEnv(t, "config.yaml", minimal, "")
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

func TestRepeatedSavesGetUpdatedStamps(t *testing.T) {
	e, path := editorWithEnv(t, "config.yaml", minimal, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	first := "debug"
	newStamp, err := e.Save(raw, stamp, Patch{LogLevel: &first})
	if err != nil {
		t.Fatal(err)
	}
	if newStamp == stamp {
		t.Fatal("stamp did not advance")
	}
	raw, current, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if current != newStamp {
		t.Fatal("read stamp does not match the returned stamp")
	}
	second := "warn"
	if _, err := e.Save(raw, newStamp, Patch{LogLevel: &second}); err != nil {
		t.Fatal("second save failed without a reload", err)
	}
	if c, err := Load(path); err != nil || c.LogLevel != "warn" {
		t.Fatal("second save not applied", c.LogLevel, err)
	}
}

func TestRestartOnlyChangeRejectedBeforeWrite(t *testing.T) {
	e, path := editorWithEnv(t, "config.yaml", minimal, "")
	current, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e.SetCurrent(func() Config { return current })
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	listen := ":9090"
	if _, err := e.Save(raw, stamp, Patch{Listen: &listen}); err == nil {
		t.Fatal("expected restart-only rejection")
	} else if !strings.Contains(err.Error(), "listen") {
		t.Fatal("expected listen in error, got", err)
	}
	if readFile(t, path) != minimal {
		t.Fatal("restart-only change was written")
	}
}

func TestTOMLPatchPreservesCommentsAndFormatting(t *testing.T) {
	body := "# top\n" +
		"qbt_url = \"http://host\"\n" +
		"\n" +
		"[tag_sync]\n" +
		"enabled = false # keep off\n" +
		"prefix = \"qbtw-\"\n" +
		"\n" +
		"[policies.stalled_no_seeders]\n" +
		"action = \"warn\"\n" +
		"threshold = \"30m\"\n"
	e, path := editorWithEnv(t, "config.toml", body, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	action := "delete"
	seconds := 60
	on := true
	if _, err := e.Save(raw, stamp, Patch{
		Policies:       map[string]PolicyPatch{"stalled_no_seeders": {Action: &action, ThresholdSeconds: &seconds}},
		TagSyncEnabled: &on,
	}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	for _, want := range []string{"# top", "# keep off", "[tag_sync]", "[policies.stalled_no_seeders]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("TOML content lost: %q\n%s", want, out)
		}
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Policies[StalledNoSeeders].Action != Delete || c.Policies[StalledNoSeeders].Threshold != 60e9 || !c.TagSync.Enabled {
		t.Fatal("TOML patch not applied", c.Policies[StalledNoSeeders], c.TagSync)
	}
}

func TestTOMLDottedKeyPatch(t *testing.T) {
	body := "qbt_url = \"http://host\"\ntag_sync.enabled = false # dotted\n"
	e, path := editorWithEnv(t, "config.toml", body, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := e.Save(raw, stamp, Patch{TagSyncEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	if !strings.Contains(out, "tag_sync.enabled = true # dotted") {
		t.Fatalf("dotted key not updated in place:\n%s", out)
	}
	if strings.Contains(out, "[tag_sync]") {
		t.Fatal("dotted key was duplicated as a table", out)
	}
}

func TestCompoundSecretReferencePreservedUntilReplaced(t *testing.T) {
	body := "qbt_url: 'http://host'\nqbt_api_key: '${PART_A}${PART_B}'\n"
	e, path := editorWithEnv(t, "config.yaml", body, "PART_A=one\nPART_B=two\n")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	// A keep patch (no secret entry) must not rewrite the compound reference.
	if _, err := e.Save(raw, stamp, Patch{LogLevel: strPtr("debug")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "${PART_A}${PART_B}") {
		t.Fatal("compound reference was rewritten", readFile(t, path))
	}
	// An explicit replace does change it.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_api_key": {Mode: "replace", Source: "env", Env: strPtr("PART_A")},
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "${PART_A}") || strings.Contains(readFile(t, path), "${PART_B}") {
		t.Fatal("explicit replace did not take effect", readFile(t, path))
	}
}

func TestEnvironmentMetadataMissingEmptyAndPrecedence(t *testing.T) {
	t.Setenv("QBTW_PRECEDENCE", "from-process")
	t.Setenv("QBTW_EMPTY_PROCESS", "")
	body := "qbt_url: 'http://host'\nqbt_api_key: '${QBTW_REFERENCED_MISSING}'\n"
	e, _ := editorWithEnv(t, "config.yaml", body, "QBTW_PRECEDENCE=from-dotenv\nQBTW_EMPTY_DOTENV=\n")
	environment, err := e.Environment()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]EnvVar{}
	for _, entry := range environment {
		byName[entry.Name] = entry
	}
	if entry := byName["QBTW_PRECEDENCE"]; !entry.Available || entry.Empty {
		t.Fatal("process value did not win over .env", entry)
	}
	if entry := byName["QBTW_EMPTY_DOTENV"]; !entry.Available || !entry.Empty {
		t.Fatal("empty .env value not reported as empty", entry)
	}
	if entry := byName["QBTW_EMPTY_PROCESS"]; !entry.Available || !entry.Empty {
		t.Fatal("empty process value not reported as empty", entry)
	}
	if entry := byName["QBTW_REFERENCED_MISSING"]; entry.Available || !entry.Configured {
		t.Fatal("referenced missing variable not listed as missing", entry)
	}
}

func TestServiceReportsApplyFailureWithoutClaimingSuccess(t *testing.T) {
	m, path := manager(t, minimal)
	editor := NewEditor(path)
	editor.SetCurrent(m.Current)
	service := NewService(editor, m, func(Config) error { return errors.New("client rebuild failed") })
	_, stamp, err := editor.Read()
	if err != nil {
		t.Fatal(err)
	}
	level := "debug"
	result, err := service.Patch(stamp, Patch{LogLevel: &level})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Saved || result.Applied {
		t.Fatal("apply failure reported as success", result)
	}
	if !strings.Contains(result.Message, "previous configuration remains active") {
		t.Fatal("apply failure did not name the active configuration", result.Message)
	}
	if m.Current().LogLevel != "info" {
		t.Fatal("failed apply changed the running configuration")
	}
}

func TestServiceRepeatedSavesAdvanceStamps(t *testing.T) {
	m, path := manager(t, minimal)
	editor := NewEditor(path)
	editor.SetCurrent(m.Current)
	service := NewService(editor, m, func(Config) error { return nil })
	_, stamp, err := editor.Read()
	if err != nil {
		t.Fatal(err)
	}
	first := "debug"
	result, err := service.Patch(stamp, Patch{LogLevel: &first})
	if err != nil || !result.Applied {
		t.Fatal("first save failed", err, result)
	}
	second := "warn"
	result, err = service.Patch(result.Stamp, Patch{LogLevel: &second})
	if err != nil || !result.Applied {
		t.Fatal("second save failed", err, result)
	}
	if m.Current().LogLevel != "warn" {
		t.Fatal("second save not applied", m.Current().LogLevel)
	}
}

func TestSettingsSecretSourceInference(t *testing.T) {
	body := "qbt_url: 'http://host'\n" +
		"qbt_api_key: 'literal-key'\n" +
		"qbt_password_file: '/run/secrets/pw'\n" +
		"integrations:\n" +
		"  sonarr:\n" +
		"    api_key: '${SONARR_KEY}'\n"
	e, _ := editorWithEnv(t, "config.yaml", body, "SONARR_KEY=abc\n")
	settings, err := e.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if got := secretByKey(t, settings.Secrets, "qbt_api_key"); got.Source != SecretValue || !got.Configured {
		t.Fatal("literal secret not inferred as value", got)
	}
	if got := secretByKey(t, settings.Secrets, "qbt_password"); got.Source != SecretPath || got.Path != "/run/secrets/pw" {
		t.Fatal("file secret not inferred as path", got)
	}
	if got := secretByKey(t, settings.Secrets, "integrations.sonarr.api_key"); got.Source != SecretEnv || got.EnvName != "SONARR_KEY" {
		t.Fatal("nested env secret not inferred", got)
	}
	if got := secretByKey(t, settings.Secrets, "integrations.radarr.api_key"); got.Source != SecretUnset || got.Configured {
		t.Fatal("absent secret not reported unset", got)
	}
}

func TestTOMLSettingsReadModel(t *testing.T) {
	body := "qbt_url = \"http://host\"\n" +
		"dry_run = false\n" +
		"[policies.stalled_no_seeders]\n" +
		"action = \"delete\"\n"
	e, _ := editorWithEnv(t, "config.toml", body, "")
	settings, err := e.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Format != TOML {
		t.Fatal("format not reported", settings.Format)
	}
	if settings.General.DryRun.Source != SourceExplicit || settings.General.DryRun.Value != false {
		t.Fatal("TOML explicit value not read", settings.General.DryRun)
	}
	if settings.Policies["stalled_no_seeders"].Action.Value != "delete" {
		t.Fatal("TOML policy not read", settings.Policies["stalled_no_seeders"])
	}
}

func secretByKey(t *testing.T, secrets []SecretSetting, key string) SecretSetting {
	t.Helper()
	for _, secret := range secrets {
		if secret.Key == key {
			return secret
		}
	}
	t.Fatalf("secret %q not found", key)
	return SecretSetting{}
}

func strPtr(value string) *string { return &value }
