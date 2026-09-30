package web

import (
	"sort"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// This file derives the presentation-only values for the Activity page: the
// service a history event belongs to, the outcome/action styling classes, and
// the distinct filter options. It only re-words the audit events; it never
// changes the engine, the config or the watchdog logic.

// historyService names the service an audit event belongs to. Engine actions
// (empty integration) are qBittorrent's own; recovery events carry their Arr
// integration. The value doubles as the client-side service filter key.
func historyService(e store.Event) string {
	if e.Integration != "" {
		return kindLabel(e.Integration)
	}
	return "qBittorrent"
}

// historyRowClass maps an audit action to a styling class that distinguishes
// requested, confirmed, skipped, failed and uncertain (recovery) outcomes.
func historyRowClass(e store.Event) string {
	switch e.Action {
	case "action_requested":
		return " action-requested"
	case "action_confirmed":
		return " action-confirmed"
	case "action_skipped":
		return " action-skipped"
	case "action_failed":
		return " action-failed"
	case "recovery":
		return " action-uncertain"
	default:
		return ""
	}
}

// outcomeClass maps an audit outcome to a styling class.
func outcomeClass(outcome string) string {
	switch outcome {
	case "success":
		return " outcome-success"
	case "accepted":
		return " outcome-accepted"
	case "skipped":
		return " outcome-skipped"
	case "failed":
		return " outcome-failed"
	default:
		return ""
	}
}

// historyPolicies lists the distinct policy IDs present in the history, sorted,
// for the client-side policy filter.
func historyPolicies(s watchdog.Snapshot) []config.PolicyID {
	seen := map[config.PolicyID]bool{}
	for _, e := range s.History {
		if e.Policy != "" {
			seen[e.Policy] = true
		}
	}
	ids := make([]config.PolicyID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// historyOutcomes lists the distinct audit outcomes present in the history,
// sorted, for the client-side outcome filter.
func historyOutcomes(s watchdog.Snapshot) []string {
	seen := map[string]bool{}
	for _, e := range s.History {
		if e.Outcome != "" {
			seen[e.Outcome] = true
		}
	}
	outcomes := make([]string, 0, len(seen))
	for o := range seen {
		outcomes = append(outcomes, o)
	}
	sort.Strings(outcomes)
	return outcomes
}

// historyServices lists the distinct service names present in the history,
// sorted, for the client-side service filter.
func historyServices(s watchdog.Snapshot) []string {
	seen := map[string]bool{}
	for _, e := range s.History {
		seen[historyService(e)] = true
	}
	services := make([]string, 0, len(seen))
	for svc := range seen {
		services = append(services, svc)
	}
	sort.Strings(services)
	return services
}
