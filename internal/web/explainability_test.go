package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

func ptrTime() *time.Time {
	t := time.Now().UTC()
	return &t
}

// handlerOver serves exactly the given rows, so every assertion is made
// against what an operator's browser receives rather than against a source
// file. No rows means the empty-list fallback is rendered.
func handlerOver(t *testing.T, rows ...watchdog.Row) http.Handler {
	t.Helper()
	c, s, m := fixture(t)
	s.Torrents = rows
	return Handler(c, func() watchdog.Snapshot { return *s }, m)
}

func body(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := request(h, path)
	if w.Code != 200 {
		t.Fatal("not served", path, w.Code)
	}
	return w.Body.String()
}

func TestStatusRowPublishesConfiguredAndEffectiveActionSeparately(t *testing.T) {
	c, s, m := fixture(t)
	s.DryRun = true
	s.Policies = []watchdog.PolicyView{{
		Policy: config.StalledPartial, Action: config.DeleteFile,
		EffectiveAction: config.Warn, ThresholdSeconds: 60,
	}}
	s.Torrents = []watchdog.Row{
		{
			Name: "overdue", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
			Policy: config.StalledPartial, ConfiguredAction: config.DeleteFile,
			EffectiveAction: config.Warn, ThresholdSeconds: 60,
			Elapsed: 90, Remaining: -30, Decision: watchdog.DecisionEligible,
		},
		{
			Name: "ordinary", ShortHash: "bbbbbbbbbbbb", State: "downloading",
			Decision: watchdog.DecisionNotApplicable,
		},
	}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	var decoded struct {
		Torrents []map[string]any `json:"torrents"`
	}
	raw := request(h, "/api/v1/status").Body.Bytes()
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	classified := decoded.Torrents[0]
	if _, present := classified["configured_action"]; !present {
		t.Fatal("configured action absent from the payload")
	}
	// Both actions are present and they differ, which is the only way a
	// dry-run downgrade is legible per row.
	if classified["configured_action"] != "delete_file" || classified["effective_action"] != "warn" {
		t.Fatal("dry-run override not visible per row", classified)
	}
	// Existing fields keep their names and their meaning; the payload grew.
	for field, want := range map[string]any{
		"policy": "stalled_partial", "threshold_seconds": 60.,
		"elapsed_seconds": 90., "remaining_seconds": -30.,
	} {
		if classified[field] != want {
			t.Fatal("existing status field changed", field, classified[field])
		}
	}

	unclassified := decoded.Torrents[1]
	if unclassified["policy"] != "" || unclassified["configured_action"] != "" ||
		unclassified["effective_action"] != "" || unclassified["first_seen_policy"] != nil {
		t.Fatal("unclassified row carried a policy or a clock", unclassified)
	}
}

// TestInterfaceExplainsEveryClassificationActionAndBlocker re-roots the old
// app.js label-table guarantee onto the Go label maps. Every machine value the
// engine can publish must have a friendly label in exactly one place — Go — so
// no value reaches an operator unexplained.
func TestInterfaceExplainsEveryClassificationActionAndBlocker(t *testing.T) {
	policyLabels := PolicyLabels()
	for _, id := range config.PolicyIDs() {
		if policyLabels[string(id)] == "" {
			t.Fatal("policy has no friendly label", id)
		}
	}
	if len(policyLabels) != len(config.PolicyIDs()) {
		t.Fatal("policy label map has stray entries", len(policyLabels), len(config.PolicyIDs()))
	}

	actionLabels := ActionLabels()
	for _, action := range config.Actions() {
		if actionLabels[string(action)] == "" {
			t.Fatal("action has no friendly label", action)
		}
	}
	if len(actionLabels) != len(config.Actions()) {
		t.Fatal("action label map has stray entries", len(actionLabels), len(config.Actions()))
	}

	decisionLabels := watchdog.DecisionLabels()
	for _, decision := range watchdog.Decisions() {
		if decisionLabels[decision] == "" {
			t.Fatal("decision reaches the operator unexplained", decision)
		}
	}

	gateLabels := watchdog.GateLabels()
	for _, gate := range watchdog.GateNames() {
		if gateLabels[gate] == "" {
			t.Fatal("gate reaches the operator unexplained", gate)
		}
	}

	// Machine values must survive into the row untranslated, so an operator
	// can still match a row against the JSON, the logs and the metrics.
	h := handlerOver(t, watchdog.Row{
		Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, ConfiguredAction: config.Delete,
		EffectiveAction: config.Warn, Decision: watchdog.DecisionTracking,
	})
	page := body(t, h, "/partials/torrents")
	if !strings.Contains(page, "stalled_partial · stalledDL") {
		t.Fatal("machine policy value hidden from the row")
	}
	// Both actions appear per row, and the override is named. The wording
	// comes from the shared action labels, so a dry-run downgrade reads
	// "Remove torrent -> Report only".
	if !strings.Contains(page, "Remove torrent") ||
		!strings.Contains(page, "Report only") ||
		!strings.Contains(page, "dry-run override") {
		t.Fatal("row does not contrast configured and effective action")
	}
}

// TestUnknownSizeRendersAsPlaceholderNotNegativeBytes covers the qBittorrent
// 5.2.3 -1 sentinel in the UI: an unknown size must read as a placeholder, not
// as "-1 B" or a misleading "0 B".
func TestUnknownSizeRendersAsPlaceholderNotNegativeBytes(t *testing.T) {
	h := handlerOver(t, watchdog.Row{
		Name: "magnet", ShortHash: "aaaaaaaaaaaa", State: "metaDL",
		Progress: 0, Downloaded: 0, Size: -1, TotalSize: -1,
		Decision: watchdog.DecisionNotApplicable,
	})
	page := body(t, h, "/partials/torrents")
	if !strings.Contains(page, "size — / —") {
		t.Fatalf("unknown size not rendered as a placeholder: %s", page)
	}
	if strings.Contains(page, "-1 B") {
		t.Fatal("unknown size rendered as a negative byte count")
	}
}

func TestCountdownReadsAsEligibilityNotAsGuaranteedDeletion(t *testing.T) {
	// The detailed conditions live in the per-row tooltip, not in a long block
	// above the list; the list itself carries only the short explanation.
	h := handlerOver(t, watchdog.Row{
		Name: "tracking", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, ConfiguredAction: config.Delete,
		EffectiveAction: config.Delete, ThresholdSeconds: 600,
		FirstSeen: ptrTime(), Elapsed: 60, Remaining: 540,
		Decision: watchdog.DecisionTracking,
	})
	page := body(t, h, "/partials/torrents")
	if !strings.Contains(page, "Remove in ~") {
		t.Fatal("next-action wording missing")
	}
	if !strings.Contains(page, "Not a countdown to deletion") {
		t.Fatal("per-row timing tooltip lost its eligibility caveat")
	}
	// Overdue stays unmistakably overdue rather than decaying into a timer.
	overdue := handlerOver(t, watchdog.Row{
		Name: "overdue", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, ConfiguredAction: config.Delete,
		EffectiveAction: config.Delete, ThresholdSeconds: 60,
		FirstSeen: ptrTime(), Elapsed: 90, Remaining: -30,
		Decision: watchdog.DecisionEligible,
	})
	overduePage := body(t, overdue, "/partials/torrents")
	if !strings.Contains(overduePage, "Threshold already met") ||
		!strings.Contains(overduePage, "overdue") {
		t.Fatal("overdue rendering lost")
	}
	// A row without a proven episode clock reads as observation-interrupted,
	// never an elapsed time inferred across an outage.
	unclocked := handlerOver(t, watchdog.Row{
		Name: "unclocked", ShortHash: "bbbbbbbbbbbb", State: "stalledDL",
		Policy: config.StalledPartial, ConfiguredAction: config.Delete,
		EffectiveAction: config.Delete, Decision: watchdog.DecisionTracking,
	})
	if !strings.Contains(body(t, unclocked, "/partials/torrents"), "Blocked · observation interrupted") {
		t.Fatal("timer not gated on a proven episode clock")
	}
}

func TestHostileTorrentValuesStayInertInEveryRenderedSurface(t *testing.T) {
	hostile := `</td><img src=x onerror=alert(1)><script>alert("x")</script>`
	h := handlerOver(t, watchdog.Row{
		Name: hostile, ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Category: hostile, Tags: hostile, Policy: config.StalledNoSeeders,
		ConfiguredAction: config.Delete, EffectiveAction: config.Warn,
		Decision: watchdog.DecisionTracking,
	})
	page := body(t, h, "/partials/torrents")
	if strings.Contains(page, "<img src=x") || strings.Contains(page, "<script>alert") {
		t.Fatal("hostile value rendered as markup")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("hostile value not escaped into text")
	}

	// The client script only ever assigns text nodes; it never builds markup.
	js := body(t, h, "/assets/app.js")
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(js, sink) {
			t.Fatal("markup sink in the client script", sink)
		}
	}
	if !strings.Contains(js, "textContent") {
		t.Fatal("client script no longer assigns text nodes")
	}

	// Nothing is fetched from elsewhere, so the page stays usable and private
	// under the strict content security policy it ships with.
	fullPage := body(t, h, "/")
	for _, remote := range []string{"https://", "http://", "//cdn", "@import"} {
		if strings.Contains(fullPage, remote) {
			t.Fatal("remote reference in the page", remote)
		}
	}
}

// TestServerRenderedPageAndPartialAgreeOnTorrents replaces the old
// "no-script fallback and refreshed table agree on width" guarantee: there is
// no second renderer anymore, so the full page and the HTMX partial must both
// come from the same server-side template and show the same rows. In the
// multipage shell torrents render only through the shared partial, so the
// partial is the single surface asserted here.
func TestServerRenderedPageAndPartialAgreeOnTorrents(t *testing.T) {
	h := handlerOver(t, watchdog.Row{
		Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, ConfiguredAction: config.Delete,
		EffectiveAction: config.Delete, Decision: watchdog.DecisionTracking,
	})
	partial := body(t, h, "/partials/torrents")
	for _, want := range []string{"example", "aaaaaaaaaaaa", "stalled_partial"} {
		if !strings.Contains(partial, want) {
			t.Fatal("torrent partial missing torrent", want)
		}
	}
	// The compact redesign uses expandable rows, not a wide table.
	if strings.Contains(partial, `<th scope="col">`) {
		t.Fatal("torrent section still renders a wide table")
	}
	if !strings.Contains(partial, `<details class="torrent`) {
		t.Fatal("torrent rows are not expandable details")
	}
	// An empty snapshot renders the fallback, not a broken table.
	empty := body(t, handlerOver(t), "/partials/torrents")
	if !strings.Contains(empty, "No torrents in the latest snapshot.") {
		t.Fatal("empty torrent fallback missing")
	}
}
