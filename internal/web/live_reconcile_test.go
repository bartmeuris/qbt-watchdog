package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// TestLiveFragmentsExposeStableReconciliationKeys pins the server half of the
// keyed coordinator: every live fragment must carry a data-key on each row so
// the client can match it to the element already in the DOM instead of
// replacing the list wholesale. If a template loses a key, a refresh silently
// degrades back to flashing.
func TestLiveFragmentsExposeStableReconciliationKeys(t *testing.T) {
	c, s, m := fixture(t)
	s.UpdatedAt = time.Now().UTC()
	s.Torrents = []watchdog.Row{{
		Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, EffectiveAction: config.Delete,
		Decision: watchdog.DecisionTracking,
	}}
	s.History = []store.Event{{
		ID: "evt-1", Time: s.UpdatedAt, Action: "action_confirmed",
		ShortHash: "aaaaaaaaaaaa", Outcome: "success",
	}}
	s.RecoveryJobs = []watchdog.RecoveryStatus{{
		ID: "job-1", Kind: config.Sonarr, Stage: store.Prepared, Attempts: 1,
	}}
	s.Policies = []watchdog.PolicyView{{
		Policy: config.StalledPartial, Action: config.Delete,
		EffectiveAction: config.Delete, ThresholdSeconds: 60,
	}}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	for _, tc := range []struct{ path, want string }{
		{"/partials/torrent-rows", `data-key="torrent-aaaaaaaaaaaa"`},
		{"/partials/history-rows", `data-key="history-evt-1"`},
		{"/partials/recovery-rows", `data-key="recovery-job-1"`},
		{"/partials/policy-items", `data-key="policy-stalled_partial"`},
		{"/partials/policy-items", `data-key="policy-table"`},
		{"/partials/overview-counters", `data-key="snapshot"`},
		{"/partials/services", `data-key="svc-qbt"`},
	} {
		page := body(t, h, tc.path)
		if !strings.Contains(page, tc.want) {
			t.Fatalf("live fragment %s lost stable identity %s", tc.path, tc.want)
		}
	}
}

// TestTorrentListOptsIntoStableOrder verifies the torrent list asks the client
// to keep existing row positions, so a countdown crossing a threshold cannot
// reshuffle the list during a routine poll.
func TestTorrentListOptsIntoStableOrder(t *testing.T) {
	h := handlerOver(t, watchdog.Row{
		Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, EffectiveAction: config.Delete,
		Decision: watchdog.DecisionTracking,
	})
	page := body(t, h, "/")
	if !strings.Contains(page, `id="torrent-list" data-refresh="/partials/torrent-rows" data-live-order="keep"`) {
		t.Fatal("torrent list does not opt into position-preserving reconciliation")
	}
}

// TestUnchangedSnapshotIsSuppressedWithNoContent is the no-op suppression path:
// when the client's digest matches the snapshot stamp the handler must reply
// 204 with no body, so the coordinator skips parsing and DOM work entirely. A
// changed snapshot returns 200 and the stamp for the next round trip.
func TestUnchangedSnapshotIsSuppressedWithNoContent(t *testing.T) {
	c, s, m := fixture(t)
	s.UpdatedAt = time.Now().UTC()
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	stamp := stampOf(*s)

	unchanged := request(h, "/partials/torrent-rows?digest="+url.QueryEscape(stamp))
	if unchanged.Code != http.StatusNoContent {
		t.Fatalf("unchanged snapshot was not suppressed: %d", unchanged.Code)
	}
	if unchanged.Body.Len() != 0 {
		t.Fatal("suppressed response carried a body")
	}

	changed := request(h, "/partials/torrent-rows?digest=stale")
	if changed.Code != http.StatusOK {
		t.Fatalf("changed snapshot was not served: %d", changed.Code)
	}
	if got := changed.Header().Get("X-Snapshot-Stamp"); got != stamp {
		t.Fatalf("stamp header mismatch: got %q want %q", got, stamp)
	}
}

// TestClientReconcilesByStableKeyNotWholeListSwaps pins the architecture change:
// the coordinator must parse fetched fragments and reconcile them by data-key.
// It must not fall back to htmx.ajax, which replaces whole list contents and
// loses open disclosures, focus, selection and popovers.
func TestClientReconcilesByStableKeyNotWholeListSwaps(t *testing.T) {
	c, s, m := fixture(t)
	js := body(t, Handler(c, func() watchdog.Snapshot { return *s }, m), "/assets/app.js")
	if !strings.Contains(js, "DOMParser") || !strings.Contains(js, "data-key") {
		t.Fatal("client no longer reconciles parsed fragments by stable key")
	}
	if strings.Contains(js, "htmx.ajax") {
		t.Fatal("client still performs whole-list htmx.ajax swaps")
	}
}

// TestSettingsPageOnlyPollsSharedStatus is the Settings invariant: the only
// live target on the page is the shared header service status. The settings
// form has no [data-refresh] container, so the coordinator can never replace or
// reconcile an operator's draft.
func TestSettingsPageOnlyPollsSharedStatus(t *testing.T) {
	c, s, m := fixture(t)
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, ConfigSaverFunc{
		SettingsFunc: func() (config.Settings, error) { return structuredSettings(), nil },
	})
	page := body(t, h, "/settings")
	if !strings.Contains(page, `id="settings-form"`) {
		t.Fatal("settings form missing")
	}
	if !strings.Contains(page, `<div class="services" id="services" data-refresh="/partials/services">`) {
		t.Fatal("shared service status target missing")
	}
	if strings.Count(page, "data-refresh=") != 1 {
		t.Fatal("settings page exposes a live target beyond shared status")
	}
}
