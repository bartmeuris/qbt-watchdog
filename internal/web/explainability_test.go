package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

// handlerOver serves exactly the given rows, so every assertion is made
// against what an operator's browser receives rather than against a source
// file. No rows means the empty-table fallback is rendered.
func handlerOver(t *testing.T, rows ...watchdog.Row) http.Handler {
	t.Helper()
	c, s, m := fixture(t)
	s.Torrents = rows
	return Handler(c, func() watchdog.Snapshot { return *s }, m)
}

func body(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := request(h, path, "viewer", "SECRET_PASSWORD")
	if w.Code != 200 {
		t.Fatal("not served", path, w.Code)
	}
	return w.Body.String()
}

// definesKey reports whether a JavaScript object literal defines the given
// key, quoted or bare, so a label table cannot pass by merely mentioning a
// word somewhere in prose.
func definesKey(source, name string) bool {
	return strings.Contains(source, "\n  "+name+":") ||
		strings.Contains(source, "\n  '"+name+"':")
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
	raw := request(h, "/api/v1/status", "viewer", "SECRET_PASSWORD").Body.Bytes()
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

func TestInterfaceExplainsEveryClassificationActionAndBlocker(t *testing.T) {
	js := body(t, handlerOver(t), "/assets/app.js")
	for _, id := range config.PolicyIDs() {
		if !definesKey(js, string(id)) {
			t.Fatal("policy has no friendly label", id)
		}
	}
	for _, action := range config.Actions() {
		if !definesKey(js, string(action)) {
			t.Fatal("action has no friendly label", action)
		}
	}
	for _, decision := range watchdog.Decisions() {
		if !definesKey(js, decision) {
			t.Fatal("decision reaches the operator unexplained", decision)
		}
	}
	// Machine values must survive into the row untranslated, so an operator
	// can still match a row against the JSON, the logs and the metrics.
	if !strings.Contains(js, "${t.policy} · ${t.state}") {
		t.Fatal("machine policy value hidden from the row")
	}
	// Both actions appear per row, and the override is named.
	if !strings.Contains(js, "actionLabel(t.configured_action)") ||
		!strings.Contains(js, "dry-run override") {
		t.Fatal("row does not contrast configured and effective action")
	}
}

func TestCountdownReadsAsEligibilityNotAsGuaranteedDeletion(t *testing.T) {
	h := handlerOver(t)
	page, js := body(t, h, "/"), body(t, h, "/assets/app.js")
	if !strings.Contains(page, "Eligible in") {
		t.Fatal("countdown column not renamed")
	}
	for _, caveat := range []string{
		"not a countdown to deletion", "action cap", "retry budget",
		"exclusions", "confirmation read",
	} {
		if !strings.Contains(page, caveat) {
			t.Fatal("eligibility caveat missing", caveat)
		}
	}
	// Overdue stays unmistakably overdue rather than decaying into a timer.
	if !strings.Contains(js, "overdue`") || !strings.Contains(js, "Threshold already met") {
		t.Fatal("overdue rendering lost")
	}
	// A row without a proven episode clock shows a dash, never an elapsed
	// time inferred across an outage.
	if !strings.Contains(js, "const counting = Boolean(t.first_seen_policy)") ||
		!strings.Contains(js, "counting ? duration(t.elapsed_seconds) : '—'") {
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
	page := body(t, h, "/")
	if strings.Contains(page, "<img src=x") || strings.Contains(page, "<script>alert") {
		t.Fatal("hostile value rendered as markup")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("hostile value not escaped into text")
	}

	// The refreshing view meets hostile values on every poll, so it must only
	// ever assign text nodes.
	js := body(t, h, "/assets/app.js")
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(js, sink) {
			t.Fatal("markup sink in the refreshing view", sink)
		}
	}
	if !strings.Contains(js, "td.textContent = value") || !strings.Contains(js, "small.textContent = detail") {
		t.Fatal("cells no longer built from text nodes")
	}

	// Nothing is fetched from elsewhere, so the page stays usable and private
	// under the strict content security policy it ships with.
	for _, remote := range []string{"https://", "http://", "//cdn", "@import"} {
		if strings.Contains(page, remote) {
			t.Fatal("remote reference in the page", remote)
		}
	}
}

func TestNoScriptFallbackAndRefreshedTableAgreeOnWidth(t *testing.T) {
	h := handlerOver(t)
	page, js := body(t, h, "/"), body(t, h, "/assets/app.js")
	torrents, _, found := strings.Cut(page, "Recent actions")
	if !found {
		t.Fatal("page layout changed")
	}
	_, torrents, _ = strings.Cut(torrents, "<h2>Torrents</h2>")
	columns := strings.Count(torrents, `<th scope="col">`)
	if columns < 10 {
		t.Fatal("torrent table lost columns", columns)
	}
	if !strings.Contains(js, fmt.Sprintf("empty(rows, %d,", columns)) ||
		!strings.Contains(torrents, fmt.Sprintf(`colspan="%d"`, columns)) {
		t.Fatal("fallback and refreshed table disagree on width", columns)
	}
}
