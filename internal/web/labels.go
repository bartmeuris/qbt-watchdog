package web

import (
	"fmt"
	"strings"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// This file is the single home of every human-readable label the dashboard
// prints. Machine values (policy IDs, actions, decisions, gates, recovery
// stages, Arr modes, audit actions and outcomes) keep their exact wire spelling
// in /api/v1/status, the metrics and the persisted state; only here are they
// made friendly. Moving these away from app.js means the template FuncMap and
// the explainability tests watch the same closed maps, so no machine value can
// reach a reader unexplained.

// PolicyLabels maps every closed policy ID to its headline. It is total over
// config.PolicyIDs(), proven by the explainability test.
func PolicyLabels() map[string]string {
	return map[string]string{
		string(config.Metadata):           "Metadata stall",
		string(config.StalledNoSeeders):   "Stalled · no seeders ever seen",
		string(config.StalledSeedersSeen): "Stalled · seeders seen before",
		string(config.StalledPartial):     "Stalled · partly downloaded",
		string(config.CompletedNoData):    "Completed · no payload data",
		string(config.StoppedArrManaged):  "Stopped · Arr managed",
	}
}

// PolicyDescriptions maps every closed policy ID to a one-sentence explanation
// of the predicate that selects it. It is total over config.PolicyIDs() and
// mirrors the actual predicates in watchdog.policy(): historical seeders are
// not the current seeder count, partial progress is not zero progress, and the
// stopped-Arr partition is selected by explicit operator tags.
func PolicyDescriptions() map[string]string {
	return map[string]string{
		string(config.Metadata):           "Metadata never downloaded within the allowed time.",
		string(config.StalledNoSeeders):   "No downloaded progress and no seeder ever observed.",
		string(config.StalledSeedersSeen): "No downloaded progress while stalled; connected seeders were observed earlier in this observation period.",
		string(config.StalledPartial):     "Stalled with some downloaded progress.",
		string(config.CompletedNoData):    "Completed-looking torrent that carries no payload data.",
		string(config.StoppedArrManaged):  "Stopped torrent that matches the configured Arr tags.",
	}
}

// ActionLabels maps every closed action to its headline. It is total over
// config.Actions().
func ActionLabels() map[string]string {
	return map[string]string{
		string(config.Warn):       "Warn only",
		string(config.Delete):     "Delete torrent, keep files",
		string(config.DeleteFile): "Delete torrent and files",
	}
}

// decisionLabels delegates to the watchdog package so the template and the
// engine share one truth; watchdog.DecisionLabels() is already closed and total.
var decisionLabels = watchdog.DecisionLabels()

var gateLabels = watchdog.GateLabels()

// stageLabels covers the closed recovery stage vocabulary in store.RecoveryJob.
var stageLabels = map[string]string{
	string(store.Prepared):         "Prepared",
	string(store.AwaitDelete):      "Awaiting delete confirmation",
	string(store.Resolving):        "Resolving identity",
	string(store.BlocklistIntent):  "Blocklist intended",
	string(store.BlocklistPending): "Blocklist pending",
	string(store.SearchIntent):     "Search intended",
	string(store.SearchPending):    "Search pending",
	string(store.CommandPending):   "Command pending",
	string(store.Uncertain):        "Uncertain",
}

// modeLabels covers the closed Arr mode vocabulary.
var modeLabels = map[string]string{
	string(config.InheritArrMode):     "Inherit",
	string(config.NoArrMode):          "None",
	string(config.BlocklistAndSearch): "Blocklist + search",
	string(config.BlocklistOnly):      "Blocklist only",
	string(config.SearchOnly):         "Search only",
}

// kindLabels covers the two Arr kinds; the rest of the pipeline uses the raw
// value because it doubles as the configuration key and log label.
var kindLabels = map[string]string{
	string(config.Sonarr): "Sonarr",
	string(config.Radarr): "Radarr",
}

// actionEventLabels covers the closed audit-event action vocabulary.
var actionEventLabels = map[string]string{
	"warn":             "Warning issued",
	"action_requested": "Deletion requested",
	"action_confirmed": "Deletion confirmed",
	"action_skipped":   "Skipped",
	"action_failed":    "Action failed",
	"recovery":         "Recovery",
}

// outcomeLabels covers the closed audit-event outcome vocabulary.
var outcomeLabels = map[string]string{
	"success":  "Success",
	"accepted": "Accepted",
	"skipped":  "Skipped",
	"failed":   "Failed",
}

// timelineStatusLabels covers the closed step-status vocabulary of a timeline.
var timelineStatusLabels = map[string]string{
	StatusWaiting:   "Waiting",
	StatusRequested: "Requested",
	StatusRunning:   "Running",
	StatusCompleted: "Completed",
	StatusSkipped:   "Skipped",
	StatusFailed:    "Failed",
	StatusUncertain: "Uncertain",
}

// Lookup helpers used by the template FuncMap. They are total: an unknown value
// falls back to something truthful rather than reaching an inherited property,
// so a future vocabulary cannot silently print a blank or a raw ID.

func policyLabel(id config.PolicyID) string {
	if l, ok := PolicyLabels()[string(id)]; ok {
		return l
	}
	if id == "" {
		return "Not applicable"
	}
	return string(id)
}

func policyDescription(id config.PolicyID) string {
	if d, ok := PolicyDescriptions()[string(id)]; ok {
		return d
	}
	return ""
}

func actionLabel(a config.Action) string {
	if a == "" {
		return "—"
	}
	if l, ok := ActionLabels()[string(a)]; ok {
		return l
	}
	return string(a)
}

func decisionLabel(d string) string {
	if l, ok := decisionLabels[d]; ok {
		return l
	}
	if d == "" {
		return "Unknown"
	}
	return d
}

func gateLabel(name string) string {
	if l, ok := gateLabels[name]; ok {
		return l
	}
	return name
}

func stageLabel(stage store.RecoveryStage) string {
	if l, ok := stageLabels[string(stage)]; ok {
		return l
	}
	return string(stage)
}

func modeLabel(m config.ArrMode) string {
	if m == "" {
		return "—"
	}
	if l, ok := modeLabels[string(m)]; ok {
		return l
	}
	return string(m)
}

func kindLabel(k config.ArrKind) string {
	if l, ok := kindLabels[string(k)]; ok {
		return l
	}
	return string(k)
}

func actionEventLabel(a string) string {
	if l, ok := actionEventLabels[a]; ok {
		return l
	}
	return a
}

func outcomeLabel(o string) string {
	if l, ok := outcomeLabels[o]; ok {
		return l
	}
	if o == "" {
		return "—"
	}
	return o
}

func policyLabelOrUnclassified(id config.PolicyID) string {
	if id == "" {
		return "Not applicable"
	}
	return policyLabel(id)
}

func statusLabel(s string) string {
	if l, ok := timelineStatusLabels[s]; ok {
		return l
	}
	return s
}

// Formatting helpers. Times render through a <time datetime> element so a tiny
// Intl formatter in app.js turns them into the operator's local zone; the raw
// value stays an RFC3339 UTC instant, so nothing depends on server-side TZ.

// dash renders "—" for an empty value so templates never print a bare empty cell.
func dash(s any) string {
	switch v := s.(type) {
	case string:
		if v == "" {
			return "—"
		}
		return v
	case nil:
		return "—"
	default:
		return fmt.Sprint(v)
	}
}

// duration renders an elapsed threshold/interval into the same compact form the
// old app.js used: seconds, then minutes+seconds, then hours+minutes.
func duration(seconds float64) string {
	n := int64(seconds)
	if n < 0 {
		n = -n
	}
	switch {
	case n < 60:
		return fmt.Sprintf("%ds", n)
	case n < 3600:
		return fmt.Sprintf("%dm %ds", n/60, n%60)
	default:
		return fmt.Sprintf("%dh %dm", n/3600, (n%3600)/60)
	}
}

// bytes renders a byte count with binary units, matching the old app.js.
func bytes(n int64) string {
	if n < 0 {
		n = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	var value float64 = float64(n)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

// pct renders a 0..1 progress fraction as a two-decimal percentage.
func pct(fraction float64) string {
	return fmt.Sprintf("%.2f%%", fraction*100)
}

// join renders a comma-separated list, defaulting to "—" when empty.
func join(values []string) string {
	if len(values) == 0 {
		return "—"
	}
	return strings.Join(values, ", ")
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
