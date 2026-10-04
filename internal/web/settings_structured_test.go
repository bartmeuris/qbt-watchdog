package web

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/watchdog"
)

// structuredSettings is a synthetic, fully populated read model. It carries no
// secret values by construction, which is exactly what the page must render.
func structuredSettings() config.Settings {
	return config.Settings{
		Stamp:  "stamp-one",
		Format: config.YAML,
		General: config.GeneralSettings{
			DryRun:            config.Setting{Value: true, Effective: true, Source: config.SourceExplicit},
			MaxActionsPerPoll: config.Setting{Value: 10, Effective: 10, Source: config.SourceDefault},
			PollInterval:      config.Setting{Value: "30s", Effective: "30s", Source: config.SourceDefault},
			UIRefreshInterval: config.Setting{Value: "5s", Effective: "5s", Source: config.SourceDefault},
			HistoryLimit:      config.Setting{Value: 100, Effective: 100, Source: config.SourceDefault},
		},
		Policies: map[string]config.PolicySettings{
			"stalled_no_seeders": {
				Action:           config.Setting{Value: "warn", Effective: "warn", Source: config.SourceExplicit},
				ThresholdSeconds: config.Setting{Value: "30m", Effective: 1800, Source: config.SourceExplicit},
				ArrMode:          config.Setting{Value: "inherit", Effective: "inherit", Source: config.SourceInherited},
				MatchTags:        config.Setting{Value: []string{"tag-a"}, Effective: []string{"tag-a"}, Source: config.SourceExplicit},
			},
		},
		Connections: config.ConnectionSettings{
			QBTURL:      config.Setting{Value: "http://host", Effective: "http://host", Source: config.SourceExplicit},
			QBTUsername: config.Setting{Value: "operator", Effective: "operator", Source: config.SourceExplicit},
			QBTAuthMode: config.Setting{Value: "password", Effective: "password", Source: config.SourceExplicit},
			Arr: map[string]config.ArrConnectionSettings{
				"sonarr": {
					Enabled:    config.Setting{Value: true, Effective: true, Source: config.SourceExplicit},
					URL:        config.Setting{Value: "http://sonarr", Effective: "http://sonarr", Source: config.SourceExplicit},
					Mode:       config.Setting{Value: "blocklist_and_search", Effective: "blocklist_and_search", Source: config.SourceDefault},
					Timeout:    config.Setting{Value: "10s", Effective: "10s", Source: config.SourceDefault},
					Categories: config.Setting{Value: []string{"tv"}, Effective: []string{"tv"}, Source: config.SourceExplicit},
				},
				"radarr": {
					Enabled:    config.Setting{Value: false, Effective: false, Source: config.SourceDefault},
					URL:        config.Setting{Value: "", Effective: nil, Source: config.SourceDefault},
					Mode:       config.Setting{Value: "blocklist_and_search", Effective: "blocklist_and_search", Source: config.SourceDefault},
					Timeout:    config.Setting{Value: "10s", Effective: "10s", Source: config.SourceDefault},
					Categories: config.Setting{Value: []string{}, Effective: []string{}, Source: config.SourceDefault},
				},
			},
		},
		Scope: config.ScopeSettings{
			IncludeCategories: config.Setting{Value: []string{}, Effective: []string{}, Source: config.SourceDefault},
			ExcludeCategories: config.Setting{Value: []string{"adult"}, Effective: []string{"adult"}, Source: config.SourceExplicit},
			ExcludeTags:       config.Setting{Value: []string{"keep"}, Effective: []string{"keep"}, Source: config.SourceDefault},
		},
		Advanced: config.AdvancedSettings{
			MaxObservationGap:         config.Setting{Value: nil, Effective: nil, Source: config.SourceDefault},
			DeleteConfirmationTimeout: config.Setting{Value: "2m", Effective: "2m", Source: config.SourceDefault},
			HTTPTimeout:               config.Setting{Value: "10s", Effective: "10s", Source: config.SourceDefault},
			ReadinessMaxAge:           config.Setting{Value: "2m", Effective: "2m", Source: config.SourceDefault},
			TagSyncEnabled:            config.Setting{Value: false, Effective: false, Source: config.SourceDefault},
			TagSyncPrefix:             config.Setting{Value: "qbtw-", Effective: "qbtw-", Source: config.SourceDefault},
			TagSyncMaxWritesPerPoll:   config.Setting{Value: 20, Effective: 20, Source: config.SourceDefault},
			TLSInsecure:               config.Setting{Value: false, Effective: false, Source: config.SourceDefault},
			TLSCAFile:                 config.Setting{Value: "", Effective: "", Source: config.SourceDefault},
			LogLevel:                  config.Setting{Value: "info", Effective: "info", Source: config.SourceDefault},
			LogFormat:                 config.Setting{Value: "json", Effective: "json", Source: config.SourceDefault},
			LogColor:                  config.Setting{Value: "auto", Effective: "auto", Source: config.SourceDefault},
			StateFile:                 config.Setting{Value: "/data/state.json", Effective: "/data/state.json", Source: config.SourceDefault},
			Listen:                    config.Setting{Value: ":8080", Effective: ":8080", Source: config.SourceDefault},
		},
		Secrets: []config.SecretSetting{
			{Key: "qbt_api_key", Source: config.SecretEnv, Configured: true, EnvName: "MY_KEY"},
			{Key: "qbt_password", Source: config.SecretValue, Configured: true},
			{Key: "integrations.sonarr.api_key", Source: config.SecretUnset},
			{Key: "integrations.radarr.api_key", Source: config.SecretUnset},
		},
		Environment: []config.EnvVar{
			{Name: "MY_KEY", Available: true, Configured: true},
			{Name: "MISSING_KEY", Available: false, Configured: true},
			{Name: "EMPTY_KEY", Available: true, Empty: true, Configured: true},
		},
	}
}

// structuredSaverFixture wires the structured seam against the synthetic model
// and records the patch a form save produced.
func structuredSaverFixture(t *testing.T) (config.Config, *watchdog.Snapshot, *observability.Metrics, ConfigSaverFunc, *config.Patch) {
	t.Helper()
	c, s, m := fixture(t)
	received := &config.Patch{}
	model := structuredSettings()
	saver := ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return []byte("qbt_url: 'http://host'\n"), "stamp-one", nil },
		SaveFunc: func([]byte, string) (config.SaveResult, error) {
			return config.SaveResult{Stamp: "stamp-two", Saved: true, Applied: true, Status: config.Status{Generation: 2}}, nil
		},
		SettingsFunc: func() (config.Settings, error) { return model, nil },
		PatchFunc: func(stamp string, patch config.Patch) (config.SaveResult, error) {
			*received = patch
			return config.SaveResult{Stamp: "stamp-two", Saved: true, Applied: true, Status: config.Status{Generation: 2}}, nil
		},
		EnvironmentFunc: func() ([]config.EnvVar, error) { return model.Environment, nil },
	}
	return c, s, m, saver, received
}

func TestSettingsPageRendersStructuredSections(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	page := body(t, h, "/settings")

	for _, want := range []string{
		`id="settings-form"`,
		`id="general"`, `id="policies"`, `id="connections"`, `id="scope"`, `id="advanced"`, `id="appearance"`,
		`id="dry-run"`,
		`id="policy-stalled_no_seeders"`,
		`id="policy-stalled_no_seeders-action"`,
		`id="policy-stalled_no_seeders-match-tags"`,
		`id="policy-stalled_no_seeders-arr"`,
		`name="dry_run"`,
		`name="poll_interval_value"`,
		`name="qbt_auth_mode"`,
		`name="secret.qbt_api_key.type"`,
		`Advanced / raw editor`,
		`id="theme-toggle"`,
		`data-auth-mode="api_key"`,
		`data-auth-mode="password"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("structured settings page missing %q", want)
		}
	}
	// The raw editor is behind a disclosure and loaded on demand, never first.
	if strings.Contains(page, `<textarea name="raw"`) {
		t.Fatal("raw editor rendered inline instead of behind the disclosure")
	}
	// The form is server-rendered, not fetched by htmx, so it cannot be polled.
	if strings.Contains(page, `hx-get="/partials/settings"`) {
		t.Fatal("settings form is fetched by htmx instead of server-rendered")
	}
}

func TestSettingsPageMarksMissingAndEmptyEnvironment(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	page := body(t, h, "/settings")

	if !strings.Contains(page, "env-missing") || !strings.Contains(page, ">Missing<") {
		t.Fatal("missing environment variable not marked")
	}
	if !strings.Contains(page, "env-empty") || !strings.Contains(page, ">Empty<") {
		t.Fatal("empty environment variable not marked")
	}
	// The configured variable is listed even though it is the only reference.
	if !strings.Contains(page, `data-name="MY_KEY"`) {
		t.Fatal("configured environment variable not listed")
	}
}

func TestSettingsPageNeverRendersSecretValues(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	page := body(t, h, "/settings")

	for _, forbidden := range []string{"SECRET_PASSWORD", "supersecret", "literal-key", "MY_KEY_VALUE"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("secret value leaked into the page: %q", forbidden)
		}
	}
	// A configured value secret shows "Configured" and an empty password input.
	if !strings.Contains(page, "Configured") {
		t.Fatal("configured secret state missing")
	}
	if !strings.Contains(page, `type="password"`) {
		t.Fatal("secret value input is not a password field")
	}
}

func TestSettingsFormSecretPatchModes(t *testing.T) {
	cases := []struct {
		name  string
		form  url.Values
		check func(t *testing.T, patch config.Patch)
	}{
		{
			name: "switch env to value",
			form: url.Values{"secret.qbt_api_key.type": {"value"}, "secret.qbt_api_key.value": {"new-key"}},
			check: func(t *testing.T, patch config.Patch) {
				got := patch.Secrets["qbt_api_key"]
				if got.Mode != "replace" || got.Source != "value" || got.Value == nil || *got.Value != "new-key" {
					t.Fatalf("wrong patch %+v", got)
				}
			},
		},
		{
			name: "switch value to env",
			form: url.Values{"secret.qbt_password.type": {"env"}, "secret.qbt_password.env": {"MY_KEY"}},
			check: func(t *testing.T, patch config.Patch) {
				got := patch.Secrets["qbt_password"]
				if got.Mode != "replace" || got.Source != "env" || got.Env == nil || *got.Env != "MY_KEY" {
					t.Fatalf("wrong patch %+v", got)
				}
			},
		},
		{
			name: "switch to path",
			form: url.Values{"secret.qbt_password.type": {"path"}, "secret.qbt_password.path": {"/run/secrets/pw"}},
			check: func(t *testing.T, patch config.Patch) {
				got := patch.Secrets["qbt_password"]
				if got.Mode != "replace" || got.Source != "path" || got.Path == nil || *got.Path != "/run/secrets/pw" {
					t.Fatalf("wrong patch %+v", got)
				}
			},
		},
		{
			name: "clear",
			form: url.Values{"secret.qbt_api_key.clear": {"true"}},
			check: func(t *testing.T, patch config.Patch) {
				if got := patch.Secrets["qbt_api_key"]; got.Mode != "clear" {
					t.Fatalf("wrong patch %+v", got)
				}
			},
		},
		{
			name: "untouched keeps",
			form: url.Values{"secret.qbt_api_key.type": {"env"}, "secret.qbt_api_key.env": {"MY_KEY"}},
			check: func(t *testing.T, patch config.Patch) {
				if _, present := patch.Secrets["qbt_api_key"]; present {
					t.Fatal("untouched secret entered the patch")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s, m, saver, received := structuredSaverFixture(t)
			h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
			form := url.Values{"stamp": {"stamp-one"}}
			for key, values := range tc.form {
				form[key] = values
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			h.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			tc.check(t, *received)
		})
	}
}

func TestSettingsFormRejectsEmptySecretSource(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	form := url.Values{
		"stamp":                    {"stamp-one"},
		"secret.qbt_password.type": {"env"},
		"secret.qbt_password.env":  {""},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != 422 {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "qbt_password") {
		t.Fatal("field-specific error missing", rr.Body.String())
	}
}

func TestSettingsFormSendsOnlyChangedFields(t *testing.T) {
	c, s, m, saver, received := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	// Submit the current values unchanged: the diff must produce an empty patch.
	form := url.Values{
		"stamp":                     {"stamp-one"},
		"dry_run":                   {"true"},
		"max_actions_per_poll":      {"10"},
		"poll_interval_value":       {"30"},
		"poll_interval_unit":        {"s"},
		"history_limit":             {"100"},
		"max_observation_gap_value": {"0"},
		"max_observation_gap_unit":  {"s"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if received.DryRun != nil || received.MaxActionsPerPoll != nil || received.PollInterval != nil || received.HistoryLimit != nil {
		t.Fatalf("unchanged fields entered the patch: %+v", received)
	}
	if received.MaxObservationGap != nil {
		t.Fatalf("unset optional duration was frozen into the patch: %+v", received.MaxObservationGap)
	}
}

func TestSettingsFormConflictReturns409(t *testing.T) {
	c, s, m := fixture(t)
	saver := ConfigSaverFunc{
		SettingsFunc: func() (config.Settings, error) { return structuredSettings(), nil },
		PatchFunc: func(string, config.Patch) (config.SaveResult, error) {
			return config.SaveResult{}, config.ErrConflict
		},
	}
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	form := url.Values{"stamp": {"stale"}, "dry_run": {"false"}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSettingsFormSaveReturnsNewStamp(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	form := url.Values{"stamp": {"stamp-one"}, "dry_run": {"false"}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"stamp":"stamp-two"`) {
		t.Fatal("new stamp not returned", rr.Body.String())
	}
}

// TestSettingsPageRendersFromRealService exercises the whole seam: a real
// editor, service and manager feed the page, so the structured form is proven
// against the same code path the binary wires.
func TestSettingsPageRendersFromRealService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	document := "qbt_url: 'http://host'\n" +
		"qbt_api_key: '${MY_KEY}'\n" +
		"dry_run: false\n" +
		"policies:\n" +
		"  stalled_no_seeders:\n" +
		"    action: delete\n" +
		"    threshold: 45m\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MY_KEY", "secret")
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
	page := body(t, h, "/settings")
	for _, want := range []string{
		`id="settings-form"`,
		`id="policy-stalled_no_seeders"`,
		`name="secret.qbt_api_key.type"`,
		`value="MY_KEY"`,
		`name="policy.stalled_no_seeders.action"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("real-service settings page missing %q", want)
		}
	}
	// The resolved secret value must never reach the page.
	if strings.Contains(page, ">secret<") {
		t.Fatal("resolved secret value leaked into the page")
	}
}

func TestSettingsClientScriptDrivesSecretControls(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	js := body(t, h, "/assets/app.js")
	for _, want := range []string{
		"settings-form", "secret-type", "data-active-type", "env-marker",
		"revealHash", "settings-status", "captureBaseline", "applyAuthMode",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("client script missing %q", want)
		}
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(js, sink) {
			t.Fatalf("markup sink in the client script: %q", sink)
		}
	}
}

func TestSettingsFormRejectsInvalidDuration(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	form := url.Values{
		"stamp":               {"stamp-one"},
		"poll_interval_value": {"not-a-number"},
		"poll_interval_unit":  {"s"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != 422 {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "poll_interval") {
		t.Fatal("field-specific duration error missing", rr.Body.String())
	}
}

func TestSettingsPageUnavailableWithoutStructuredSeam(t *testing.T) {
	c, s, m := fixture(t)
	saver := ConfigSaverFunc{
		ReadFunc: func() ([]byte, string, error) { return []byte("qbt_url: 'http://host'\n"), "stamp-one", nil },
		SaveFunc: func([]byte, string) (config.SaveResult, error) { return config.SaveResult{}, nil },
	}
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	page := body(t, h, "/settings")
	if !strings.Contains(page, "Structured settings are not available") {
		t.Fatal("missing structured-settings note")
	}
	if !strings.Contains(page, "Advanced / raw editor") {
		t.Fatal("raw editor disclosure missing when structured settings are unavailable")
	}
}

// TestSettingsRawEditorFetchesOnlyWhenOpened pins the privacy guarantee: the
// raw document (which may carry literal-mode secrets) must not be requested on
// page load. The editor fragment is fetched only when the operator opens the
// disclosure, and only once.
func TestSettingsRawEditorFetchesOnlyWhenOpened(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	page := body(t, h, "/settings")

	if strings.Contains(page, `hx-trigger="load"`) {
		t.Fatal("raw editor is fetched on page load")
	}
	if !strings.Contains(page, `hx-trigger="toggle once from:closest details"`) {
		t.Fatal("raw editor is not deferred to the disclosure toggle")
	}
	if !strings.Contains(page, `hx-get="/partials/settings-editor"`) {
		t.Fatal("raw editor fetch target missing")
	}
}

// TestSettingsClientScriptReadsSaveResultFields pins the raw-editor result
// handling to the server's SaveResult shape. The old handler tested a
// non-existent `status === 'applied'` string and read a `generation` field the
// server never sends, so a saved-but-unapplied document read as success.
func TestSettingsClientScriptReadsSaveResultFields(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	js := body(t, h, "/assets/app.js")

	for _, want := range []string{
		"parsed.saved && parsed.applied",
		"parsed.saved && !parsed.applied",
		"parsed.message",
		"function applySaveResult (result, form)",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("client script missing save-result handling %q", want)
		}
	}
	for _, stale := range []string{"parsed.generation", "parsed.status === 'applied'"} {
		if strings.Contains(js, stale) {
			t.Fatalf("client script still reads the stale result shape %q", stale)
		}
	}
}

// TestSettingsClientScriptPreservesActionStatusClasses pins the reconciliation
// fix: the ok/error classes are added by the script, so a fragment re-render
// must merge them back rather than let the server's class attribute strip them.
func TestSettingsClientScriptPreservesActionStatusClasses(t *testing.T) {
	c, s, m, saver, _ := structuredSaverFixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, saver)
	js := body(t, h, "/assets/app.js")

	if !strings.Contains(js, "statusClasses") ||
		!strings.Contains(js, "live.classList.add(statusClasses[i])") {
		t.Fatal("action-status classes are not merged back after reconciliation")
	}
}
