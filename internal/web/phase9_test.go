package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

// TestSettingsResponsesNeverEchoSecretValues closes the "no secret values in
// responses/errors" gap. It wires the real editor/service seam, submits a secret
// value through the structured form, and asserts the value never appears in a
// rejected error envelope, a successful save response, or the re-rendered page.
func TestSettingsResponsesNeverEchoSecretValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	document := "qbt_url: 'http://host'\nqbt_api_key: '${MY_KEY}'\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MY_KEY", "resolved-secret")
	current, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(path, current)
	editor := config.NewEditor(path)
	editor.SetCurrent(manager.Current)
	service := config.NewService(editor, manager, func(config.Config) error { return nil })
	saver := ConfigSaverFunc{
		ReadFunc:        editor.Read,
		SaveFunc:        service.SaveRaw,
		SettingsFunc:    service.Settings,
		PatchFunc:       service.Patch,
		EnvironmentFunc: service.Environment,
	}
	c, s, m := fixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	_, stamp, err := editor.Read()
	if err != nil {
		t.Fatal(err)
	}

	const secretValue = "SUPER_SECRET_VALUE_92753"

	// A rejected save: the secret value is submitted alongside an invalid
	// setting, so the response carries validation errors. The secret must not
	// appear in the error envelope.
	rejected := url.Values{
		"stamp":                    {stamp},
		"secret.qbt_api_key.type":  {"value"},
		"secret.qbt_api_key.value": {secretValue},
		"log_format":               {"bogus"},
	}
	rr := postSettingsForm(t, h, rejected)
	if rr.Code != 422 {
		t.Fatalf("expected 422 for invalid log_format, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secretValue) {
		t.Fatalf("rejected response echoed the secret value: %s", rr.Body.String())
	}

	// A successful save: the secret value is written but the response and the
	// re-rendered page must report only its source, never its value.
	accepted := url.Values{
		"stamp":                    {stamp},
		"secret.qbt_api_key.type":  {"value"},
		"secret.qbt_api_key.value": {secretValue},
	}
	rr = postSettingsForm(t, h, accepted)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secretValue) {
		t.Fatalf("save response echoed the secret value: %s", rr.Body.String())
	}
	page := body(t, h, "/settings")
	if strings.Contains(page, secretValue) {
		t.Fatal("settings page echoed the saved secret value")
	}
	if !strings.Contains(page, "Configured") {
		t.Fatal("saved secret not reported as configured")
	}
}

// postSettingsForm submits a structured settings form as a same-origin HTMX
// request, the only shape the mutation endpoint accepts.
func postSettingsForm(t *testing.T, h http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	return rr
}
