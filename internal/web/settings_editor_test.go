package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/watchdog"
)

// newSaverFixture wires a ConfigSaverFunc against a fixed raw document so the
// read/write path can be exercised without touching the filesystem. The last
// return value records what Save received.
func newSaverFixture(t *testing.T, raw, stamp string) (config.Config, *watchdog.Snapshot, *observability.Metrics, ConfigSaverFunc, *struct{ raw, stamp string }) {
	t.Helper()
	c, s, m := fixture(t)
	saved := struct{ raw, stamp string }{}
	saver := ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return []byte(raw), stamp, nil },
		SaveFunc: func(b []byte, st string) (string, config.Status, error) {
			saved.raw = string(b)
			saved.stamp = st
			return "new-stamp", config.Status{Generation: 2}, nil
		},
	}
	return c, s, m, saver, &saved
}

func TestSettingsEditorFragmentEscapesRawYAML(t *testing.T) {
	raw := "qbt_url: 'http://<script>alert(1)</script>'\n# comment <b>bold</b>\n"
	c, s, m, saver, _ := newSaverFixture(t, raw, "stamp-one")
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/partials/settings-editor", nil)
	req.SetBasicAuth("viewer", "SECRET_PASSWORD")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<textarea name="raw" rows="24" spellcheck="false">`) {
		t.Fatal("missing raw textarea")
	}
	if !strings.Contains(body, `name="stamp" value="stamp-one"`) {
		t.Fatal("missing stamp input")
	}
	// The YAML must be HTML-escaped, never emitted as live markup.
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>bold</b>") {
		t.Fatal("raw YAML was not escaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("escaped script tag absent")
	}
}

func TestSettingsEditorDisabledNote(t *testing.T) {
	c, s, m := fixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/partials/settings-editor", nil)
	req.SetBasicAuth("viewer", "SECRET_PASSWORD")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no config saver wired") {
		t.Fatal("missing disabled note")
	}
}

func TestPostConfigAcceptsFormEncodedInput(t *testing.T) {
	raw := "qbt_url: 'http://localhost'\n"
	c, s, m, saver, saved := newSaverFixture(t, raw, "stamp-one")
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	form := url.Values{}
	form.Set("raw", "qbt_url: 'http://edited'\n")
	form.Set("stamp", "stamp-one")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.SetBasicAuth("viewer", "SECRET_PASSWORD")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if saved.raw != "qbt_url: 'http://edited'\n" || saved.stamp != "stamp-one" {
		t.Fatalf("saver received (%q, %q)", saved.raw, saved.stamp)
	}
	if !strings.Contains(rr.Body.String(), `"status":"applied"`) {
		t.Fatal("expected applied response")
	}
}

func TestPostConfigRejectsWithoutCSRF(t *testing.T) {
	c, s, m, saver, _ := newSaverFixture(t, "qbt_url: 'http://localhost'\n", "stamp-one")
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	form := url.Values{}
	form.Set("raw", "qbt_url: 'http://edited'\n")
	form.Set("stamp", "stamp-one")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Deliberately omit HX-Request / Sec-Fetch-Site headers.
	req.SetBasicAuth("viewer", "SECRET_PASSWORD")
	h.ServeHTTP(rr, req)

	if rr.Code != 403 {
		t.Fatalf("expected 403 for missing CSRF, got %d", rr.Code)
	}
}
