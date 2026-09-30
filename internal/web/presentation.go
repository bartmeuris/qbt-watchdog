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
	Error       string
	LastAction  string
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
	ind := serviceIndicator{ID: "qbt", Name: "qBittorrent", Error: "—", LastAction: "—"}
	switch {
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
	ind := serviceIndicator{ID: string(it.Kind), Name: kindLabel(it.Kind), Error: "—", LastAction: "—"}
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

// torrentStates lists the distinct qBittorrent states in the snapshot, sorted,
// for the client-side state filter.
func torrentStates(s watchdog.Snapshot) []string {
	seen := map[string]bool{}
	for _, row := range s.Torrents {
		if row.State != "" {
			seen[row.State] = true
		}
	}
	states := make([]string, 0, len(seen))
	for st := range seen {
		states = append(states, st)
	}
	sort.Strings(states)
	return states
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
