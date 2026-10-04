package web

import (
	"fmt"
	"sort"
	"time"

	"qbt-watchdog/internal/arr"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// This file derives the presentation-only values the templates need: the
// header service indicators, the per-row gate strip, the "next action" wording
// and the one-sentence decision explanation. None of it changes the engine,
// the config or the watchdog logic; it only re-words what the snapshot already
// says, so the UI can never disagree with the machine values underneath.

// serviceIndicator is one compact health dot in the shared header. Status is a
// closed vocabulary the CSS maps to a colour; the rest is human text.
type serviceIndicator struct {
	ID          string
	Name        string
	Status      string // healthy | failed | stale | disabled | unknown
	StatusLabel string
	LastContact *time.Time
	// LastContactLabel names what LastContact actually is. For qBittorrent it is
	// the last successful poll, never the last network contact.
	LastContactLabel string
	Error            string
	LastAction       string
	// Rejected marks a response-validation failure, which is distinct from a
	// connection failure: qBittorrent answered, but the data failed validation.
	Rejected      bool
	Explanation   string
	QBTVersion    string
	WebAPIVersion string
}

// serviceIndicators builds the header dots: qBittorrent first, then each Arr
// integration in snapshot order. Missing information is "unknown", never
// fabricated; "enabled" is never conflated with "connected".
func serviceIndicators(s watchdog.Snapshot) []serviceIndicator {
	out := []serviceIndicator{qbtIndicator(s)}
	for _, it := range s.Integrations {
		out = append(out, arrIndicator(it, s.History))
	}
	return out
}

func qbtIndicator(s watchdog.Snapshot) serviceIndicator {
	ind := serviceIndicator{ID: "qbt", Name: "qBittorrent", Error: "—", LastAction: "—", LastContactLabel: "Last successful poll"}
	switch {
	case s.PollDiagnostic != nil && s.PollDiagnostic.Kind == watchdog.PollErrorResponseRejected:
		ind.Status, ind.StatusLabel = "failed", "Response rejected"
		ind.Error = s.PollError
		ind.Rejected = true
		ind.Explanation = "qBittorrent responded, but the returned torrent data failed validation."
		ind.QBTVersion = s.QBTVersion
		ind.WebAPIVersion = s.WebAPIVersion
	case s.PollError != "":
		ind.Status, ind.StatusLabel = "failed", "Failed"
		ind.Error = s.PollError
	case s.QBTUp:
		ind.Status, ind.StatusLabel = "healthy", "Healthy"
	case s.LastSuccess == nil:
		ind.Status, ind.StatusLabel = "unknown", "Unknown"
	default:
		ind.Status, ind.StatusLabel = "stale", "Stale"
	}
	ind.LastContact = s.LastSuccess
	if e, ok := latestEvent(s.History, ""); ok {
		ind.LastAction = actionEventLabel(e.Action) + " · " + outcomeLabel(e.Outcome)
	}
	return ind
}

func arrIndicator(it watchdog.IntegrationStatus, history []store.Event) serviceIndicator {
	ind := serviceIndicator{ID: string(it.Kind), Name: kindLabel(it.Kind), Error: "—", LastAction: "—", LastContactLabel: "Last contact"}
	switch {
	case !it.Enabled:
		ind.Status, ind.StatusLabel = "disabled", "Disabled"
	case it.Code != "" && it.Code != string(arr.Accepted):
		ind.Status, ind.StatusLabel = "failed", "Failed"
		ind.Error = it.Code
	case it.Fresh:
		ind.Status, ind.StatusLabel = "healthy", "Healthy"
	case it.QueueAt.IsZero():
		ind.Status, ind.StatusLabel = "unknown", "Unknown"
	default:
		ind.Status, ind.StatusLabel = "stale", "Stale"
	}
	if !it.QueueAt.IsZero() {
		t := it.QueueAt
		ind.LastContact = &t
	}
	if e, ok := latestEvent(history, it.Kind); ok {
		ind.LastAction = actionEventLabel(e.Action) + " · " + outcomeLabel(e.Outcome)
	}
	return ind
}

// latestEvent returns the newest audit event for an integration kind, or the
// newest engine action when integration is empty (qBittorrent's own actions).
func latestEvent(history []store.Event, integration config.ArrKind) (store.Event, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Integration == integration {
			return history[i], true
		}
	}
	return store.Event{}, false
}

// torrentListNotice describes the torrent list when it is not a plain accepted
// list. Kind is a closed vocabulary the template maps to a class; the text is
// the compact operator-facing sentence. Extended diagnostics live in the
// service popover, not here.
type torrentNotice struct {
	Kind   string // waiting | rejected | stale
	Text   string
	Since  *time.Time
	Reason string
}

// torrentListNotice returns nil for an accepted list (rows or the normal empty
// state). It never uses len(Torrents) to decide whether data was ever received:
// LastTorrentListSuccess is the authoritative marker, and it survives a reload
// that resets LastSuccess.
func torrentListNotice(s watchdog.Snapshot) *torrentNotice {
	if s.TorrentDataStale {
		return &torrentNotice{Kind: "stale", Since: s.LastTorrentListSuccess, Reason: s.PollError}
	}
	if s.LastTorrentListSuccess != nil {
		return nil
	}
	if s.PollDiagnostic != nil && s.PollDiagnostic.Kind == watchdog.PollErrorResponseRejected {
		return &torrentNotice{Kind: "rejected", Text: "No valid torrent snapshot yet — qBittorrent response rejected."}
	}
	return &torrentNotice{Kind: "waiting", Text: "Waiting for the first torrent snapshot."}
}

// gateItem is one step of the horizontal evaluation strip. Symbol and Class are
// the closed presentation of a gate's state; Detail is the accessible
// explanation (the engine's own sentence fragment, optionally enriched).
type gateItem struct {
	Symbol string // ✓ ◷ ⊘ ○
	Class  string // pass | wait | block | skip
	Label  string
	Detail string
}

// gateStrip renders every gate in evaluation order. A gate the engine never
// reached is "not evaluated" (○), never "passed"; a skipped gate is not a
// passed gate. Continuity and threshold are the only "waiting" (◷) gates —
// they are the clock, not a blocker.
func gateStrip(row watchdog.Row) []gateItem {
	byName := make(map[string]watchdog.GateResult, len(row.Gates))
	for _, g := range row.Gates {
		byName[g.Name] = g
	}
	items := make([]gateItem, 0, len(watchdog.GateNames()))
	for _, name := range watchdog.GateNames() {
		g, evaluated := byName[name]
		item := gateItem{Label: gateShortLabel(name)}
		if !evaluated {
			item.Symbol, item.Class, item.Detail = "○", "skip", "Not evaluated"
			items = append(items, item)
			continue
		}
		switch {
		case g.OK:
			item.Symbol, item.Class = "✓", "pass"
		case name == watchdog.GateContinuity || name == watchdog.GateThreshold:
			item.Symbol, item.Class = "◷", "wait"
		default:
			item.Symbol, item.Class = "⊘", "block"
		}
		item.Detail = g.Detail
		if name == watchdog.GateThreshold && !g.OK && row.FirstSeen != nil && row.Remaining > 0 {
			item.Detail = fmt.Sprintf("%s · ~%s remaining", g.Detail, approxDuration(row.Remaining))
		}
		items = append(items, item)
	}
	return items
}

func gateShortLabel(name string) string {
	switch name {
	case watchdog.GateClassification:
		return "Scope"
	case watchdog.GatePayloadPresent:
		return "Payload"
	case watchdog.GateConfigured:
		return "Policy"
	case watchdog.GateExclusions:
		return "Exclusions"
	case watchdog.GateFieldChecks:
		return "Fields"
	case watchdog.GatePendingDelete:
		return "Pending"
	case watchdog.GateContinuity:
		return "Continuity"
	case watchdog.GateThreshold:
		return "Waiting"
	case watchdog.GateCap:
		return "Cap"
	case watchdog.GateDryRun:
		return "Dry run"
	case watchdog.GateAttempts:
		return "Attempts"
	default:
		return name
	}
}

// nextActionLabel is the effective-action wording that replaces "Eligible in".
// It names the action the engine may actually take (already downgraded by
// dry-run) and the rough time left, or the single thing blocking it. A dry-run
// row therefore reads "Report in ~12m", never a destructive verb.
func nextActionLabel(row watchdog.Row) string {
	if row.DeleteRequestedAt != nil || row.Decision == watchdog.DecisionDeleteRequested {
		return "Removal requested"
	}
	if row.Policy == "" {
		return "—"
	}
	switch row.Decision {
	case watchdog.DecisionProtected:
		return "Blocked · excluded"
	case watchdog.DecisionActionsDisabled:
		return "Blocked · actions disabled"
	case watchdog.DecisionRetryLimitReached:
		return "Blocked · retry limit reached"
	case watchdog.DecisionWarned:
		return "Reported"
	case watchdog.DecisionNonzeroProgress, watchdog.DecisionNonzeroDownloaded, watchdog.DecisionPayloadPresent:
		return "Blocked · field check"
	case watchdog.DecisionEligible:
		return "Ready for next poll"
	case watchdog.DecisionTracking:
		if row.FirstSeen == nil {
			return "Blocked · observation interrupted"
		}
		if row.Remaining <= 0 {
			return "Ready for next poll"
		}
		return actionVerb(row.EffectiveAction) + " in ~" + approxDuration(row.Remaining)
	}
	return "—"
}

func actionVerb(a config.Action) string {
	switch a {
	case config.DeleteFile:
		return "Remove + files"
	case config.Delete:
		return "Remove"
	default:
		return "Report"
	}
}

// approxDuration renders a rough elapsed time ("12m", "45s", "2h 5m") for the
// "in ~12m" wording, where precision would imply a countdown the engine does
// not guarantee.
func approxDuration(seconds float64) string {
	n := int64(seconds)
	if n < 0 {
		n = -n
	}
	switch {
	case n < 60:
		return fmt.Sprintf("%ds", n)
	case n < 3600:
		return fmt.Sprintf("%dm", n/60)
	default:
		return fmt.Sprintf("%dh %dm", n/3600, (n%3600)/60)
	}
}

// decisionSentence is the one-line explanation of the current decision, derived
// from the same closed vocabulary the gate strip walks.
func decisionSentence(row watchdog.Row) string {
	switch row.Decision {
	case watchdog.DecisionNotApplicable:
		return "This torrent does not match any configured policy."
	case watchdog.DecisionTracking:
		if row.FirstSeen == nil {
			return "Observation has not been continuous long enough to act."
		}
		if row.Remaining > 0 {
			return fmt.Sprintf("Observed for %s; needs ~%s more continuous observation before it can be acted on.", approxDuration(row.Elapsed), approxDuration(row.Remaining))
		}
		return "Threshold met; awaiting the next poll."
	case watchdog.DecisionEligible:
		return "Threshold met; the next poll may act on it if every check still passes."
	case watchdog.DecisionProtected:
		return "Excluded by category or tag; no action will be taken."
	case watchdog.DecisionNonzeroProgress:
		return "Has downloaded progress, so the metadata policy does not apply."
	case watchdog.DecisionNonzeroDownloaded:
		return "Has downloaded payload, so the zero-progress policy does not apply."
	case watchdog.DecisionPayloadPresent:
		return "Completed but still carries payload data."
	case watchdog.DecisionDeleteRequested:
		return "A removal was requested and is awaiting confirmation."
	case watchdog.DecisionActionsDisabled:
		return "The per-poll action cap is zero, so actions are disabled."
	case watchdog.DecisionWarned:
		return "A warning was already emitted for this episode."
	case watchdog.DecisionRetryLimitReached:
		return "The attempt budget for this episode is exhausted."
	}
	return ""
}

// stateOption is one choice in the torrent state filter: the exact qBittorrent
// state value (kept verbatim so a row can always be matched against the JSON,
// the logs and the metrics), a readable label, and the number of torrents in
// the full current snapshot that carry it.
type stateOption struct {
	Value string
	Label string
	Count int
}

// torrentStateCatalog is the fixed, ordered set of qBittorrent states the
// filter always offers. It is deliberately independent of the current snapshot:
// a state with zero torrents still gets a choice, so the dropdown never
// collapses to "All states" on an empty poll and an operator's selection is
// never silently dropped when its count reaches zero. The values are the exact
// qBittorrent WebUI state strings; the labels are the readable form.
var torrentStateCatalog = []stateOption{
	{Value: "error", Label: "Error"},
	{Value: "missingFiles", Label: "Missing files"},
	{Value: "allocating", Label: "Allocating"},
	{Value: "checkingDL", Label: "Checking download"},
	{Value: "checkingUP", Label: "Checking upload"},
	{Value: "downloading", Label: "Downloading"},
	{Value: "forcedDL", Label: "Forced download"},
	{Value: "forcedMetaDL", Label: "Forced metadata"},
	{Value: "forcedUP", Label: "Forced upload"},
	{Value: "metaDL", Label: "Downloading metadata"},
	{Value: "moving", Label: "Moving"},
	{Value: "pausedDL", Label: "Paused download"},
	{Value: "pausedUP", Label: "Paused upload"},
	{Value: "queuedDL", Label: "Queued download"},
	{Value: "queuedUP", Label: "Queued upload"},
	{Value: "stalledDL", Label: "Stalled download"},
	{Value: "stalledUP", Label: "Stalled upload"},
	{Value: "stoppedDL", Label: "Stopped download"},
	{Value: "stoppedUP", Label: "Stopped upload"},
	{Value: "unknown", Label: "Unknown"},
	{Value: "uploading", Label: "Uploading"},
}

// torrentStateOptions returns the fixed catalog with counts taken from the full
// current torrent snapshot. Counts are computed here, before any name search,
// so the number beside a state never changes as the operator types. A state the
// catalog does not recognize is appended (sorted, labelled with its raw value)
// rather than hidden, so a torrent in an unexpected state still has a filter
// choice and can never be stranded.
func torrentStateOptions(s watchdog.Snapshot) []stateOption {
	counts := map[string]int{}
	for _, row := range s.Torrents {
		if row.State != "" {
			counts[row.State]++
		}
	}

	options := make([]stateOption, 0, len(torrentStateCatalog)+len(counts))
	known := make(map[string]bool, len(torrentStateCatalog))
	for _, entry := range torrentStateCatalog {
		known[entry.Value] = true
		entry.Count = counts[entry.Value]
		options = append(options, entry)
	}

	unrecognized := make([]string, 0)
	for state := range counts {
		if !known[state] {
			unrecognized = append(unrecognized, state)
		}
	}
	sort.Strings(unrecognized)
	for _, state := range unrecognized {
		options = append(options, stateOption{Value: state, Label: state, Count: counts[state]})
	}
	return options
}

// progressSymbol maps a timeline step status to a compact glyph for the
// action-progress strip.
func progressSymbol(status string) string {
	switch status {
	case StatusCompleted:
		return "✓"
	case StatusRequested, StatusRunning:
		return "◷"
	case StatusFailed:
		return "⊘"
	case StatusSkipped:
		return "—"
	case StatusUncertain:
		return "?"
	default:
		return "○"
	}
}
