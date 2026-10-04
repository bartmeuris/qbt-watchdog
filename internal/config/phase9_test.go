package config

import (
	"strings"
	"testing"
)

// TestPatchAuthModeRemovesConflictingCredentials closes the auth-mode transition
// gap: switching qbt_auth_mode must atomically remove the credential keys the
// new mode cannot use, and a secret patch in the same request must win over the
// removal. Without this, switching from API key to password could leave a stale
// API key in the document and silently keep the old credential active.
func TestPatchAuthModeRemovesConflictingCredentials(t *testing.T) {
	t.Setenv("MY_KEY", "secret")
	body := "qbt_url: 'http://host'\nqbt_api_key: 'literal-key'\n"
	e, path := editorWithEnv(t, "config.yaml", body, "")

	// api_key -> password: the API key is removed and the password is written.
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	mode := "password"
	if _, err := e.Save(raw, stamp, Patch{
		QBTAuthMode: &mode,
		QBTUsername: strPtr("operator"),
		Secrets:     map[string]SecretPatch{"qbt_password": {Mode: "replace", Source: "value", Value: strPtr("pw")}},
	}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	if strings.Contains(out, "qbt_api_key") {
		t.Fatalf("api key survived the switch to password:\n%s", out)
	}
	if !strings.Contains(out, "qbt_username: operator") || !strings.Contains(out, "qbt_password: pw") {
		t.Fatalf("password credentials not written:\n%s", out)
	}

	// password -> api_key: the username and password are removed.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	mode = "api_key"
	if _, err := e.Save(raw, stamp, Patch{
		QBTAuthMode: &mode,
		Secrets:     map[string]SecretPatch{"qbt_api_key": {Mode: "replace", Source: "env", Env: strPtr("MY_KEY")}},
	}); err != nil {
		t.Fatal(err)
	}
	out = readFile(t, path)
	if strings.Contains(out, "qbt_username") || strings.Contains(out, "qbt_password") {
		t.Fatalf("password credentials survived the switch to api_key:\n%s", out)
	}
	if !strings.Contains(out, "${MY_KEY}") {
		t.Fatalf("api key not written:\n%s", out)
	}

	// api_key -> none: every credential key is removed.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	mode = "none"
	if _, err := e.Save(raw, stamp, Patch{QBTAuthMode: &mode}); err != nil {
		t.Fatal(err)
	}
	out = readFile(t, path)
	for _, key := range []string{"qbt_api_key", "qbt_username", "qbt_password"} {
		if strings.Contains(out, key) {
			t.Fatalf("credential %s survived the switch to none:\n%s", key, out)
		}
	}

	// An unknown mode is rejected before the write.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	bogus := "bogus"
	if _, err := e.Save(raw, stamp, Patch{QBTAuthMode: &bogus}); err == nil {
		t.Fatal("unknown auth mode accepted")
	}
}

// TestTOMLSecretClearAndSwitch closes the TOML secret-removal gap: the YAML
// path was covered, but clearing or switching a secret in a TOML document
// exercises the line-based remove path, which must delete the assignment while
// leaving comments and unrelated keys intact.
func TestTOMLSecretClearAndSwitch(t *testing.T) {
	t.Setenv("MY_KEY", "secret")
	body := "# top\n" +
		"qbt_url = \"http://host\"\n" +
		"qbt_api_key = \"literal-key\" # keep me\n" +
		"log_level = \"info\"\n"
	e, path := editorWithEnv(t, "config.toml", body, "")

	// Switch the literal value to an environment reference.
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_api_key": {Mode: "replace", Source: "env", Env: strPtr("MY_KEY")},
	}}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	if strings.Contains(out, "literal-key") {
		t.Fatalf("literal secret survived the switch:\n%s", out)
	}
	if !strings.Contains(out, `"${MY_KEY}"`) {
		t.Fatalf("env reference not written:\n%s", out)
	}
	if !strings.Contains(out, "# top") || !strings.Contains(out, `log_level = "info"`) {
		t.Fatalf("unrelated TOML content lost:\n%s", out)
	}

	// Clear removes the assignment entirely.
	raw, stamp, err = e.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Save(raw, stamp, Patch{Secrets: map[string]SecretPatch{
		"qbt_api_key": {Mode: "clear"},
	}}); err != nil {
		t.Fatal(err)
	}
	out = readFile(t, path)
	if strings.Contains(out, "qbt_api_key") {
		t.Fatalf("cleared secret still present:\n%s", out)
	}
	if !strings.Contains(out, "# top") || !strings.Contains(out, `log_level = "info"`) {
		t.Fatalf("clear damaged unrelated TOML content:\n%s", out)
	}
}

// TestTOMLCreatesMissingKeysAndTables closes the TOML creation gap: the YAML
// editor has explicit coverage for creating a missing key and a nested block,
// but the TOML line editor's insert path was untested. A patch that adds a new
// top-level key and a new nested table must produce a document that still loads.
func TestTOMLCreatesMissingKeysAndTables(t *testing.T) {
	body := "# top\nqbt_url = \"http://host\"\n"
	e, path := editorWithEnv(t, "config.toml", body, "")
	raw, stamp, err := e.Read()
	if err != nil {
		t.Fatal(err)
	}
	level := "debug"
	action := "delete"
	seconds := 60
	on := true
	if _, err := e.Save(raw, stamp, Patch{
		LogLevel:       &level,
		TagSyncEnabled: &on,
		Policies:       map[string]PolicyPatch{"stalled_no_seeders": {Action: &action, ThresholdSeconds: &seconds}},
	}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	for _, want := range []string{
		"# top",
		`log_level = "debug"`,
		"[tag_sync]",
		"enabled = true",
		"[policies.stalled_no_seeders]",
		`action = "delete"`,
		`threshold = "60s"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("created TOML content missing %q:\n%s", want, out)
		}
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "debug" || !c.TagSync.Enabled || c.Policies[StalledNoSeeders].Action != Delete || c.Policies[StalledNoSeeders].Threshold != 60e9 {
		t.Fatalf("created TOML keys not applied: %+v", c)
	}
}
