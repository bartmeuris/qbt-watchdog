package web

import (
	"fmt"
	"sort"
	"strconv"

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

// eventIdentity returns the stable DOM identity for an audit event. New events
// carry a persisted, globally unique ID. Retained events written before that
// field existed derive a deterministic key from their own fields; byte-identical
// duplicates are disambiguated with a count so even they keep distinct
// identities. The count is stable across refreshes because the stored history
// order is fixed and legacy events are never added to.
func eventIdentity(e store.Event, seen map[string]int) string {
	if e.ID != "" {
		return e.ID
	}
	key := legacyEventKey(e)
	n := seen[key]
	seen[key] = n + 1
	if n == 0 {
		return key
	}
	return key + "-" + strconv.Itoa(n)
}

// legacyEventKey derives a deterministic, id-safe key from the fields a
// pre-ID event already carried. Times are compared at their stored instant, so
// the key does not move when the event is re-rendered.
func legacyEventKey(e store.Event) string {
	return fmt.Sprintf("legacy-%d-%d-%s-%s-%s", e.Time.UTC().UnixNano(), e.CommandID, e.ShortHash, e.Action, e.Outcome)
}
