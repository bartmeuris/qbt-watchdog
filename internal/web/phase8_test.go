package web

import (
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

// TestStateFilterAlwaysOffersTheFixedCatalog pins the fix for the empty
// snapshot: the state dropdown is a fixed catalog, not a projection of the
// current poll, so every supported state keeps a choice (at zero) and the
// operator's selection can never be silently dropped when a count reaches zero.
func TestStateFilterAlwaysOffersTheFixedCatalog(t *testing.T) {
	empty := body(t, handlerOver(t), "/")
	for _, state := range []string{
		"error", "missingFiles", "allocating", "checkingDL", "checkingUP",
		"downloading", "forcedDL", "forcedMetaDL", "forcedUP", "metaDL",
		"moving", "pausedDL", "pausedUP", "queuedDL", "queuedUP",
		"stalledDL", "stalledUP", "stoppedDL", "stoppedUP", "unknown", "uploading",
	} {
		if !strings.Contains(empty, `value="`+state+`"`) {
			t.Fatalf("empty snapshot dropped state option %s", state)
		}
	}
	if !strings.Contains(empty, "All states (0)") {
		t.Fatal("empty snapshot did not count zero torrents")
	}
	if !strings.Contains(empty, "Stalled download (0)") {
		t.Fatal("zero-count state is not offered with a readable label")
	}
}

// TestStateCountsComeFromTheFullSnapshot verifies counts are taken from the
// whole snapshot before any name search: two torrents sharing a state count as
// two even though a name filter could hide one, and a state with no torrents is
// still present at zero.
func TestStateCountsComeFromTheFullSnapshot(t *testing.T) {
	h := handlerOver(t,
		watchdog.Row{Name: "alpha", ShortHash: "aaaaaaaaaaaa", State: "stalledDL"},
		watchdog.Row{Name: "beta", ShortHash: "bbbbbbbbbbbb", State: "stalledDL"},
		watchdog.Row{Name: "gamma", ShortHash: "cccccccccccc", State: "metaDL"},
	)
	page := body(t, h, "/")
	if !strings.Contains(page, "Stalled download (2)") {
		t.Fatal("state count not taken from the full snapshot")
	}
	if !strings.Contains(page, "Downloading metadata (1)") {
		t.Fatal("state count missing for a present state")
	}
	if !strings.Contains(page, "Uploading (0)") {
		t.Fatal("zero-count state not offered")
	}
	// The client recounts every rendered row in place, never the name-filtered
	// subset, and never rebuilds the select.
	js := body(t, h, "/assets/app.js")
	for _, hook := range []string{"updateStateCounts", "data-state-label", "#torrent-list details.torrent"} {
		if !strings.Contains(js, hook) {
			t.Fatalf("client no longer updates state counts in place: %s", hook)
		}
	}
}

// TestUnknownStateIsAppendedNotHidden ensures a state the catalog does not know
// still gets a filter choice, so a torrent in an unexpected state can never be
// stranded behind a filter that cannot name it.
func TestUnknownStateIsAppendedNotHidden(t *testing.T) {
	h := handlerOver(t, watchdog.Row{Name: "odd", ShortHash: "aaaaaaaaaaaa", State: "someNewState"})
	page := body(t, h, "/")
	if !strings.Contains(page, `value="someNewState"`) {
		t.Fatal("unrecognized state was hidden from the filter")
	}
	if !strings.Contains(page, "someNewState (1)") {
		t.Fatal("unrecognized state count missing")
	}
}

// TestCountdownExplanationSitsBelowTheList pins the placement and wording of
// the shortened explanation: it follows the list, spans the content width, and
// the old multi-line block above the list is gone.
func TestCountdownExplanationSitsBelowTheList(t *testing.T) {
	h := handlerOver(t, watchdog.Row{
		Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, EffectiveAction: config.Delete,
		Decision: watchdog.DecisionTracking,
	})
	page := body(t, h, "/partials/torrents")
	const explanation = "Action timing assumes continued matching and successful safety checks. Actions run on a subsequent poll."
	if !strings.Contains(page, explanation) {
		t.Fatal("countdown explanation missing")
	}
	listAt := strings.Index(page, `id="torrent-list"`)
	explanationAt := strings.Index(page, explanation)
	if listAt < 0 || explanationAt < listAt {
		t.Fatal("countdown explanation is not below the torrent list")
	}
	if strings.Contains(page, "not a countdown to deletion") {
		t.Fatal("old above-list caveat still present")
	}
}

// TestServicePopoversAreHoverFirstAndAccessible pins the accessible markup and
// the client behaviour hooks: native keyed disclosures, a popover body, and the
// hover/focus/single-open/Escape/tap rules.
func TestServicePopoversAreHoverFirstAndAccessible(t *testing.T) {
	c, s, m := fixture(t)
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	page := body(t, h, "/")
	if !strings.Contains(page, `<details class="service service-`) ||
		!strings.Contains(page, `data-key="svc-qbt"`) {
		t.Fatal("service indicator is not an accessible keyed disclosure")
	}
	if !strings.Contains(page, `class="popover"`) {
		t.Fatal("service popover content missing")
	}
	js := body(t, h, "/assets/app.js")
	for _, hook := range []string{
		"initServicePopovers", "pointerenter", "pointerleave",
		"focusin", "focusout", "Escape", "toggle",
	} {
		if !strings.Contains(js, hook) {
			t.Fatalf("service popover behaviour hook missing: %s", hook)
		}
	}
}
