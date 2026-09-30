package web

import (
	"strings"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

// This file derives the presentation-only values for the Active policies page:
// the effective-policy table rows and the deduplicated warning groups. It only
// re-words what the snapshot already says (per-policy model, integration modes,
// limits and warnings); it never changes the engine, the config or the watchdog
// logic.

// policyRow is one row of the effective-policy table. ConfiguredAction and
// EffectiveAction are kept separate so a dry-run downgrade is legible as
// "configured intent" versus "current behavior" rather than a rewritten policy.
type policyRow struct {
	ID               config.PolicyID
	Label            string
	Description      string
	Threshold        string
	ConfiguredAction config.Action
	EffectiveAction  config.Action
	DryRunOverride   bool
	ArrSummary       string
	ArrDetails       []arrBehavior
	BlockedReason    string
	MatchTags        []string
}

// arrBehavior is one service's resolved recovery behavior for a policy.
type arrBehavior struct {
	Service   string
	Mode      string
	Inherited bool
}

// policyRows builds the effective-policy table in the snapshot's policy order
// (which is config.PolicyIDs() order), so the table is stable across refreshes.
func policyRows(s watchdog.Snapshot) []policyRow {
	rows := make([]policyRow, 0, len(s.Policies))
	for _, p := range s.Policies {
		rows = append(rows, policyRowFor(s, p))
	}
	return rows
}

func policyRowFor(s watchdog.Snapshot, p watchdog.PolicyView) policyRow {
	r := policyRow{
		ID:               p.Policy,
		Label:            policyLabel(p.Policy),
		Description:      policyDescription(p.Policy),
		Threshold:        duration(p.ThresholdSeconds),
		ConfiguredAction: p.Action,
		EffectiveAction:  p.EffectiveAction,
		DryRunOverride:   p.Action != p.EffectiveAction,
		MatchTags:        p.MatchTags,
	}
	r.ArrSummary, r.ArrDetails = arrBehaviorFor(s, p)
	r.BlockedReason = blockedReasonFor(s, p)
	return r
}

// arrBehaviorFor resolves a policy's recovery mode against the enabled
// integrations. "none" disables recovery outright; "inherit" (or an empty
// value) resolves to each service's own mode, which is where per-service
// differences surface; an explicit mode applies to every enabled service.
func arrBehaviorFor(s watchdog.Snapshot, p watchdog.PolicyView) (string, []arrBehavior) {
	if p.ArrMode == config.NoArrMode {
		return "No Arr recovery", nil
	}
	details := make([]arrBehavior, 0, len(s.Integrations))
	for _, it := range s.Integrations {
		if !it.Enabled {
			continue
		}
		mode := p.ArrMode
		inherited := false
		if mode == "" || mode == config.InheritArrMode {
			mode = it.Mode
			inherited = true
		}
		details = append(details, arrBehavior{Service: kindLabel(it.Kind), Mode: modeLabel(mode), Inherited: inherited})
	}
	if len(details) == 0 {
		return "No Arr recovery", nil
	}
	if p.ArrMode == "" || p.ArrMode == config.InheritArrMode {
		return "Inherits service mode", details
	}
	return modeLabel(p.ArrMode), details
}

// blockedReasonFor names the single thing that prevents a policy from acting,
// or "" when it can act. A dry-run downgrade and a zero action cap are the two
// ways the engine suppresses a destructive action; a warn-only policy is not
// "blocked", it is simply configured to report.
func blockedReasonFor(s watchdog.Snapshot, p watchdog.PolicyView) string {
	if p.EffectiveAction == config.Warn && p.Action.Destructive() {
		return "Dry run prevents deletion"
	}
	if p.EffectiveAction.Destructive() && s.Limits.MaxActionsPerPoll == 0 {
		return "Action cap is zero"
	}
	return ""
}

// warningGroup is one deduplicated safety notice. Warnings that repeat once per
// policy (the delete-files notice) collapse into a single group whose Policies
// list names every affected policy, so the same warning is never printed once
// per policy. Global warnings carry an empty Policies list.
type warningGroup struct {
	Message  string
	Policies []config.PolicyID
}

// warningGroups groups the snapshot's warnings by their structured identity.
// The message is the closed vocabulary from config.Warnings(); the Policy field
// is the structured scope. Grouping by exact message (never by substring) and
// collecting the affected policies keeps the dedup robust to reordering.
func warningGroups(warnings []config.Warning) []warningGroup {
	groups := []warningGroup{}
	index := map[string]int{}
	for _, w := range warnings {
		i, ok := index[w.Message]
		if !ok {
			i = len(groups)
			index[w.Message] = i
			groups = append(groups, warningGroup{Message: w.Message})
		}
		if w.Policy != "" {
			groups[i].Policies = append(groups[i].Policies, w.Policy)
		}
	}
	return groups
}

// policyNames renders the human labels of a policy list, comma-separated, for
// the "affects …" tail of a deduplicated warning.
func policyNames(policies []config.PolicyID) string {
	names := make([]string, 0, len(policies))
	for _, id := range policies {
		names = append(names, policyLabel(id))
	}
	return strings.Join(names, ", ")
}
