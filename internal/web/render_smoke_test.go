package web

import (
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// This file is the Phase 9 render smoke test. It drives every page and every
// partial through the real handlers with representative synthetic snapshots and
// asserts two things a unit test can miss: the handler answers 200 with a
// complete document, and the templates execute without a type error. The
// handlers deliberately swallow template execution errors (a partial write
// cannot change an already-sent status), so the direct execution pass below is
// what actually surfaces a template type error; the handler pass proves the
// routing and the completeness of what a browser receives.

// smokePages are the full page shells.
var smokePages = []string{"/", "/policies", "/activity", "/settings"}

// smokePartials are every section partial, live inner fragment and the raw
// editor fragment the server exposes.
var smokePartials = []string{
	"/partials/overview",
	"/partials/policies",
	"/partials/torrents",
	"/partials/recovery",
	"/partials/history",
	"/partials/recent",
	"/partials/settings",
	"/partials/overview-counters",
	"/partials/torrent-rows",
	"/partials/history-rows",
	"/partials/recovery-rows",
	"/partials/recent-rows",
	"/partials/policy-items",
	"/partials/services",
	"/partials/settings-editor",
}

func smokeBuild() observability.Build {
	return observability.NewBuild("smoke", "deadbeef", "2026-09-30")
}

// smokeGates returns a full gate trace with every gate evaluated, so the
// template's gate strip renders every branch (pass, wait, block and skip).
func smokeGates(ok bool) []watchdog.GateResult {
	gates := make([]watchdog.GateResult, 0, len(watchdog.GateNames()))
	for _, name := range watchdog.GateNames() {
		gates = append(gates, watchdog.GateResult{Name: name, OK: ok, Detail: name + " evaluated"})
	}
	return gates
}

// smokePopulated builds a snapshot that exercises every policy, every decision,
// every service state, pending and failed recovery, shared-command history and
// hostile/long torrent names. It is deliberately over-full: the point is to
// force every template branch to render at least once.
func smokePopulated(dryRun bool) watchdog.Snapshot {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	last := now.Add(-5 * time.Second)
	first := now.Add(-10 * time.Minute)
	requested := now.Add(-30 * time.Second)
	hostile := `</td><img src=x onerror=alert(1)><script>alert("x")</script>`
	longName := strings.Repeat("Long-Name-", 300) + hostile

	policies := []watchdog.PolicyView{
		{Policy: config.Metadata, Action: config.DeleteFile, EffectiveAction: config.DeleteFile, ThresholdSeconds: 1800},
		{Policy: config.StalledNoSeeders, Action: config.Delete, EffectiveAction: config.Warn, ThresholdSeconds: 3600},
		{Policy: config.StalledSeedersSeen, Action: config.Delete, EffectiveAction: config.Delete, ThresholdSeconds: 7200},
		{Policy: config.StalledPartial, Action: config.Warn, EffectiveAction: config.Warn, ThresholdSeconds: 600},
		{Policy: config.CompletedNoData, Action: config.DeleteFile, EffectiveAction: config.DeleteFile, ThresholdSeconds: 300},
		{Policy: config.StoppedArrManaged, Action: config.Delete, EffectiveAction: config.Delete, ThresholdSeconds: 600, ArrMode: config.InheritArrMode, MatchTags: []string{"Sonarr", "Radarr"}},
	}

	torrents := []watchdog.Row{
		{
			Name: "metadata", ShortHash: "aaaaaaaaaaaa", Hash: strings.Repeat("a", 40),
			State: "metaDL", Policy: config.Metadata, ConfiguredAction: config.DeleteFile, EffectiveAction: config.DeleteFile,
			ThresholdSeconds: 1800, FirstSeen: &first, Elapsed: 600, Remaining: 1200,
			Decision: watchdog.DecisionTracking, Gates: smokeGates(true),
			Progress: 0.1, NumSeeds: 1, NumLeechers: 2, Category: "tv", Tags: "tag-a",
			WatchdogTags: []string{"qbtw-x"}, DesiredTags: []string{"qbtw-x"}, AddedAt: &first,
			Attempts: 1, MaxAttempts: 3,
			PolicyTrace: []watchdog.PolicyRejection{{Policy: config.StalledPartial, Reason: "state is not stalledDL"}},
		},
		{
			Name: longName, ShortHash: "bbbbbbbbbbbb", Hash: strings.Repeat("b", 40),
			State: "stalledDL", Policy: config.StalledNoSeeders, ConfiguredAction: config.Delete, EffectiveAction: config.Warn,
			ThresholdSeconds: 3600, FirstSeen: &first, Elapsed: 4000, Remaining: -400,
			Decision: watchdog.DecisionEligible, Gates: smokeGates(false),
			Progress: 0, NumSeeds: 0, NumLeechers: 0, Category: hostile, Tags: hostile,
			DeleteRequestedAt: &requested, Attempts: 2, MaxAttempts: 3, DryRunNotified: true,
		},
		{
			Name: "seeders-seen", ShortHash: "cccccccccccc", Hash: strings.Repeat("c", 40),
			State: "stalledDL", Policy: config.StalledSeedersSeen, ConfiguredAction: config.Delete, EffectiveAction: config.Delete,
			ThresholdSeconds: 7200, FirstSeen: &first, Elapsed: 100, Remaining: 7100,
			Decision: watchdog.DecisionTracking, SeedObserved: true, Gates: smokeGates(true),
		},
		{
			Name: "partial", ShortHash: "dddddddddddd", Hash: strings.Repeat("d", 40),
			State: "stalledDL", Policy: config.StalledPartial, ConfiguredAction: config.Warn, EffectiveAction: config.Warn,
			ThresholdSeconds: 600, FirstSeen: &first, Elapsed: 700, Remaining: -100,
			Decision: watchdog.DecisionWarned, Progress: 0.5, Gates: smokeGates(true),
		},
		{
			Name: "completed-no-data", ShortHash: "eeeeeeeeeeee", Hash: strings.Repeat("e", 40),
			State: "uploading", Policy: config.CompletedNoData, ConfiguredAction: config.DeleteFile, EffectiveAction: config.DeleteFile,
			ThresholdSeconds: 300, FirstSeen: &first, Elapsed: 400, Remaining: -100,
			Decision: watchdog.DecisionEligible, Gates: smokeGates(true),
		},
		{
			Name: "stopped-arr", ShortHash: "ffffffffffff", Hash: strings.Repeat("f", 40),
			State: "stoppedUP", Policy: config.StoppedArrManaged, ConfiguredAction: config.Delete, EffectiveAction: config.Delete,
			ThresholdSeconds: 600, FirstSeen: &first, Elapsed: 700, Remaining: -100,
			Decision: watchdog.DecisionDeleteRequested, DeleteRequestedAt: &requested, Gates: smokeGates(true),
		},
		{
			Name: "unclassified", ShortHash: "111111111111", Hash: strings.Repeat("1", 40),
			State: "downloading", Decision: watchdog.DecisionNotApplicable, Progress: 0.3, Gates: smokeGates(true),
		},
		{
			Name: "protected", ShortHash: "222222222222", Hash: strings.Repeat("2", 40),
			State: "stalledDL", Policy: config.StalledPartial, Decision: watchdog.DecisionProtected, Gates: smokeGates(false),
		},
		{
			Name: "actions-disabled", ShortHash: "333333333333", Hash: strings.Repeat("3", 40),
			State: "stalledDL", Policy: config.StalledNoSeeders, Decision: watchdog.DecisionActionsDisabled, Gates: smokeGates(false),
		},
		{
			Name: "retry-limit", ShortHash: "444444444444", Hash: strings.Repeat("4", 40),
			State: "stalledDL", Policy: config.StalledNoSeeders, Decision: watchdog.DecisionRetryLimitReached, Gates: smokeGates(false),
		},
		{
			Name: "nonzero-progress", ShortHash: "555555555555", Hash: strings.Repeat("5", 40),
			State: "metaDL", Policy: config.Metadata, Decision: watchdog.DecisionNonzeroProgress, Gates: smokeGates(false),
		},
		{
			Name: "nonzero-downloaded", ShortHash: "666666666666", Hash: strings.Repeat("6", 40),
			State: "stalledDL", Policy: config.StalledNoSeeders, Decision: watchdog.DecisionNonzeroDownloaded, Gates: smokeGates(false),
		},
		{
			Name: "payload-present", ShortHash: "777777777777", Hash: strings.Repeat("7", 40),
			State: "uploading", Policy: config.CompletedNoData, Decision: watchdog.DecisionPayloadPresent, Gates: smokeGates(false),
		},
		{
			Name: "unclocked", ShortHash: "888888888888", Hash: strings.Repeat("8", 40),
			State: "stalledDL", Policy: config.StalledPartial, Decision: watchdog.DecisionTracking, Gates: smokeGates(false),
		},
		{
			Name: "ready", ShortHash: "999999999999", Hash: strings.Repeat("9", 40),
			State: "stalledDL", Policy: config.StalledPartial, Decision: watchdog.DecisionTracking,
			FirstSeen: &first, Remaining: 0, Gates: smokeGates(true),
		},
	}

	history := []store.Event{
		{ID: "evt-1", Time: now.Add(-2 * time.Minute), Action: "action_requested", Outcome: "accepted", ShortHash: "bbbbbbbbbbbb", Name: longName, Policy: config.StalledNoSeeders, EffectiveAction: config.Delete, CommandID: 44, DryRun: dryRun},
		{ID: "evt-2", Time: now.Add(-1 * time.Minute), Action: "action_confirmed", Outcome: "success", ShortHash: "bbbbbbbbbbbb", Name: longName, Policy: config.StalledNoSeeders, EffectiveAction: config.Delete, CommandID: 44},
		{Time: now.Add(-3 * time.Minute), Action: "warn", Outcome: "success", ShortHash: "dddddddddddd", Name: "partial", Policy: config.StalledPartial, EffectiveAction: config.Warn},
		{ID: "evt-3", Time: now.Add(-4 * time.Minute), Action: "recovery", Outcome: "failed", Integration: config.Sonarr, ShortHash: "bbbbbbbbbbbb", Name: longName, Error: hostile, CommandID: 44},
		{ID: "evt-4", Time: now.Add(-5 * time.Minute), Action: "action_failed", Outcome: "failed", ShortHash: "cccccccccccc", Name: "seeders-seen", Policy: config.StalledSeedersSeen, Error: "boom"},
	}

	recovery := []watchdog.RecoveryStatus{
		{ID: "job-pending", Kind: config.Sonarr, Stage: store.Prepared, Mode: config.BlocklistAndSearch, ShortHash: "bbbbbbbbbbbb", Policy: config.StalledNoSeeders, CommandID: 44, CapturedAt: now.Add(-time.Minute), ExpiresAt: now.Add(23 * time.Hour), NextAt: now.Add(time.Minute), Attempts: 1},
		{ID: "job-failed", Kind: config.Radarr, Stage: store.Uncertain, Mode: config.SearchOnly, Code: "command_failed", ShortHash: "cccccccccccc", Policy: config.StalledSeedersSeen, CapturedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(22 * time.Hour), NextAt: now, Attempts: 3},
		{ID: "job-completed", Kind: config.Sonarr, Stage: store.SearchPending, Mode: config.BlocklistAndSearch, Code: "search_completed", ShortHash: "eeeeeeeeeeee", Policy: config.CompletedNoData, CapturedAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(21 * time.Hour), NextAt: now, Attempts: 2},
	}

	warnings := []config.Warning{
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Target: "policy-metadata-action", Message: "Remove torrent and files", Policy: config.Metadata},
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Target: "policy-completed_no_data-action", Message: "Remove torrent and files", Policy: config.CompletedNoData},
		{Code: config.WarningTLSInsecure, Scope: config.WarningSetting, Target: "advanced", Message: "TLS certificate verification disabled"},
		{Code: config.WarningReservedPrefix, Scope: config.WarningSetting, Target: "scope", Message: "Tag resembles the reserved watchdog prefix but remains an operator tag", Tags: []string{"qbtw-keep"}},
		{Code: config.WarningStoppedPolicyArmed, Scope: config.WarningPolicy, Target: "policy-stopped_arr_managed", Message: "Stopping a torrent with these tags makes it eligible for this policy after its waiting period. Arr recovery depends on the configured recovery mode.", Policy: config.StoppedArrManaged},
	}
	if !dryRun {
		warnings = append(warnings, config.Warning{Code: config.WarningDryRunDisabled, Scope: config.WarningSetting, Target: "dry-run", Message: "Dry run disabled"})
	}

	return watchdog.Snapshot{
		SchemaVersion:  1,
		Build:          smokeBuild(),
		DryRun:         dryRun,
		RefreshSeconds: 5,
		UpdatedAt:      now,
		QBTUp:          true,
		QBTVersion:     "5.0.0",
		WebAPIVersion:  "2.11.0",
		LastSuccess:    &last,
		NextPoll:       &now,
		ConfigStatus:   config.Status{Generation: 7, LastReloadAt: now},
		Integrations: []watchdog.IntegrationStatus{
			{Kind: config.Sonarr, Enabled: true, Mode: config.BlocklistAndSearch, Fresh: true, QueueAt: last},
			{Kind: config.Radarr, Enabled: false, Mode: config.SearchOnly},
		},
		Policies:     policies,
		Torrents:     torrents,
		RecoveryJobs: recovery,
		History:      history,
		Warnings:     warnings,
		Summary:      watchdog.Summary{Total: len(torrents), Metadata: 2, Protected: 1, Overdue: 3, WouldDelete: 1, DeleteRequested: 1},
		SinceStartup: store.Counters{Deletions: 2, WouldDeletions: 3, DeleteRequests: 4},
		Lifetime:     store.Counters{Deletions: 20, WouldDeletions: 30, DeleteRequests: 40},
	}
}

// smokeScenarios is the matrix the smoke test walks: an empty snapshot, a
// populated snapshot with dry run on and off, a degraded snapshot (qBittorrent
// failed, one Arr failed, one stale) and a stale snapshot (qBittorrent stale,
// Arr stale). Together they cover healthy, failed, stale, disabled and unknown
// service states.
func smokeScenarios(t *testing.T) []struct {
	name     string
	snapshot watchdog.Snapshot
} {
	t.Helper()
	empty := watchdog.Snapshot{SchemaVersion: 1, Build: smokeBuild()}

	degraded := smokePopulated(false)
	degraded.QBTUp = false
	degraded.PollError = "connection refused"
	degraded.LastSuccess = nil
	degraded.Integrations = []watchdog.IntegrationStatus{
		{Kind: config.Sonarr, Enabled: true, Mode: config.BlocklistAndSearch, Fresh: false, QueueAt: time.Now().UTC().Add(-time.Hour)},
		{Kind: config.Radarr, Enabled: true, Mode: config.SearchOnly, Code: "failed"},
	}

	stale := smokePopulated(false)
	stale.QBTUp = false
	old := time.Now().UTC().Add(-time.Hour)
	stale.LastSuccess = &old
	stale.Integrations = []watchdog.IntegrationStatus{
		{Kind: config.Sonarr, Enabled: true, Mode: config.BlocklistAndSearch, Fresh: false, QueueAt: old},
	}

	return []struct {
		name     string
		snapshot watchdog.Snapshot
	}{
		{"empty", empty},
		{"populated-dry-run", smokePopulated(true)},
		{"populated-live", smokePopulated(false)},
		{"degraded", degraded},
		{"stale", stale},
	}
}

// TestRenderSmokeEveryPageAndPartial drives every page and partial through the
// real handler for every scenario. A page must answer 200 and end with </html>;
// a partial must answer 200 with a non-empty body. A swallowed template error
// truncates the body, so the completeness check catches it at the HTTP layer.
func TestRenderSmokeEveryPageAndPartial(t *testing.T) {
	_, _, _, saver, _ := structuredSaverFixture(t)
	for _, scenario := range smokeScenarios(t) {
		t.Run(scenario.name, func(t *testing.T) {
			c, _, m := fixture(t)
			h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return scenario.snapshot }, m, saver)
			for _, path := range smokePages {
				w := request(h, path)
				if w.Code != http.StatusOK {
					t.Fatalf("page %s: status %d", path, w.Code)
				}
				if !strings.HasSuffix(strings.TrimSpace(w.Body.String()), "</html>") {
					t.Fatalf("page %s: incomplete render", path)
				}
			}
			for _, path := range smokePartials {
				w := request(h, path)
				if w.Code != http.StatusOK {
					t.Fatalf("partial %s: status %d", path, w.Code)
				}
				if strings.TrimSpace(w.Body.String()) == "" {
					t.Fatalf("partial %s: empty render", path)
				}
			}
		})
	}
}

// TestRenderSmokeTemplatesExecuteWithoutError executes every top-level template
// directly with the same embedded template set and FuncMap the handler uses, so
// a template type error is surfaced as a test failure instead of being swallowed
// by the handler. This is the assertion the HTTP layer cannot make.
func TestRenderSmokeTemplatesExecuteWithoutError(t *testing.T) {
	page := template.Must(template.New("base").Funcs(newViewFuncs()).ParseFS(templates, "templates/*.html"))
	model := structuredSettings()

	for _, scenario := range smokeScenarios(t) {
		t.Run(scenario.name, func(t *testing.T) {
			v := buildView(scenario.snapshot)
			for _, name := range []string{
				"overview", "overview-counters", "overview_toolbar",
				"torrents", "torrent-rows", "recent", "recent-rows",
				"policies", "policy-items", "history", "history-rows",
				"recovery", "recovery-rows", "services", "services-inner",
			} {
				var buf strings.Builder
				if err := page.ExecuteTemplate(&buf, name, v); err != nil {
					t.Fatalf("template %s: %v", name, err)
				}
			}
			// Every full page shell, including the settings page with no
			// structured model (the unavailable branch).
			for _, pageName := range []string{"overview", "policies", "activity", "settings"} {
				var buf strings.Builder
				if err := page.ExecuteTemplate(&buf, "base", buildPageView(scenario.snapshot, pageName)); err != nil {
					t.Fatalf("page %s: %v", pageName, err)
				}
			}
			// The structured settings form and the raw editor fragment.
			var settingsBuf strings.Builder
			if err := page.ExecuteTemplate(&settingsBuf, "settings", buildSettingsPageView(scenario.snapshot, &model, "")); err != nil {
				t.Fatalf("settings form: %v", err)
			}
			var editorBuf strings.Builder
			if err := page.ExecuteTemplate(&editorBuf, "settings-editor", settingsEditorView{Raw: "qbt_url: 'http://host'\n", Stamp: "stamp-one"}); err != nil {
				t.Fatalf("settings-editor: %v", err)
			}
			var disabledBuf strings.Builder
			if err := page.ExecuteTemplate(&disabledBuf, "settings-editor-disabled", nil); err != nil {
				t.Fatalf("settings-editor-disabled: %v", err)
			}
		})
	}
}

// TestRenderSmokeHostileValuesStayEscaped confirms the smoke snapshots' hostile
// torrent names and error text reach the browser as inert text on every page,
// not just the torrent partial.
func TestRenderSmokeHostileValuesStayEscaped(t *testing.T) {
	_, _, _, saver, _ := structuredSaverFixture(t)
	c, _, m := fixture(t)
	snapshot := smokePopulated(false)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return snapshot }, m, saver)
	for _, path := range []string{"/", "/policies", "/activity", "/settings", "/partials/torrents", "/partials/history-rows"} {
		body := body(t, h, path)
		if strings.Contains(body, "<img src=x") || strings.Contains(body, "<script>alert") {
			t.Fatalf("%s rendered a hostile value as markup", path)
		}
	}
}
