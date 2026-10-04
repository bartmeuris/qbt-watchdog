package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/watchdog"
)

// settingsSaverFixture wires a ConfigSaverFunc that also implements the
// structured settings seam, recording the patch it received.
func settingsSaverFixture(t *testing.T) (config.Config, *watchdog.Snapshot, *observability.Metrics, ConfigSaverFunc, *config.Patch) {
	t.Helper()
	c, s, m := fixture(t)
	received := &config.Patch{}
	saver := ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return []byte("qbt_url: 'http://host'\n"), "stamp-one", nil },
		SaveFunc: func([]byte, string) (config.SaveResult, error) {
			return config.SaveResult{Stamp: "stamp-two", Saved: true, Applied: true, Status: config.Status{Generation: 2}}, nil
		},
		SettingsFunc: func() (config.Settings, error) {
			return config.Settings{
				Stamp:  "stamp-one",
				Format: config.YAML,
				General: config.GeneralSettings{
					DryRun: config.Setting{Value: true, Effective: true, Source: config.SourceDefault},
				},
				Secrets: []config.SecretSetting{{Key: "qbt_api_key", Source: config.SecretEnv, Configured: true, EnvName: "MY_KEY"}},
			}, nil
		},
		PatchFunc: func(stamp string, patch config.Patch) (config.SaveResult, error) {
			*received = patch
			return config.SaveResult{Stamp: "stamp-two", Saved: true, Applied: true, Status: config.Status{Generation: 2}}, nil
		},
		EnvironmentFunc: func() ([]config.EnvVar, error) {
			return []config.EnvVar{{Name: "MY_KEY", Available: true, Configured: true}}, nil
		},
	}
	return c, s, m, saver, received
}

func TestPostConfigReportsApplyFailure(t *testing.T) {
	c, s, m := fixture(t)
	saver := ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return []byte("qbt_url: 'http://host'\n"), "stamp-one", nil },
		SaveFunc: func([]byte, string) (config.SaveResult, error) {
			return config.SaveResult{Stamp: "stamp-two", Saved: true, Applied: false, Message: "saved but not applied; the previous configuration remains active"}, nil
		},
	}
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/config", strings.NewReader(`{"stamp":"stamp-one","raw":"qbt_url: 'http://host'\n"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"applied":false`) {
		t.Fatal("apply failure reported as success", rr.Body.String())
	}
}

func TestGetSettingsReturnsTypedModel(t *testing.T) {
	c, s, m, saver, _ := settingsSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/settings", nil))
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var model map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &model); err != nil {
		t.Fatal(err)
	}
	if model["stamp"] != "stamp-one" || model["format"] != "yaml" {
		t.Fatal("settings model missing stamp/format", model)
	}
	// The secret is reported by source only, never by value.
	body := rr.Body.String()
	if !strings.Contains(body, `"source":"env"`) || !strings.Contains(body, `"env_name":"MY_KEY"`) {
		t.Fatal("secret source metadata missing", body)
	}
}

func TestPostSettingsAppliesPatch(t *testing.T) {
	c, s, m, saver, received := settingsSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	body := `{"stamp":"stamp-one","patch":{"dry_run":false,"policies":{"stalled_no_seeders":{"action":"delete"}}}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if received.DryRun == nil || *received.DryRun {
		t.Fatal("dry_run patch not received", received)
	}
	if received.Policies["stalled_no_seeders"].Action == nil || *received.Policies["stalled_no_seeders"].Action != "delete" {
		t.Fatal("policy patch not received", received)
	}
	if !strings.Contains(rr.Body.String(), `"applied":true`) {
		t.Fatal("expected applied result", rr.Body.String())
	}
}

func TestPostSettingsRejectsWithoutCSRF(t *testing.T) {
	c, s, m, saver, _ := settingsSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(`{"stamp":"stamp-one","patch":{}}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestGetSettingsEnvironment(t *testing.T) {
	c, s, m, saver, _ := settingsSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/settings/environment", nil))
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"name":"MY_KEY"`) {
		t.Fatal("environment metadata missing", rr.Body.String())
	}
}

func TestSettingsEndpointsUnavailableWithoutSeam(t *testing.T) {
	c, s, m := fixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, nil)
	for _, path := range []string{"/api/v1/settings", "/api/v1/settings/environment"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 404 {
			t.Fatalf("%s expected 404, got %d", path, rr.Code)
		}
	}
}
