package web

import (
	"slices"
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
	Destructive      bool
	ActionAnchor     string
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
		Destructive:      p.EffectiveAction.Destructive(),
		ActionAnchor:     "policy-" + string(p.Policy) + "-action",
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

// warningGroup is one deduplicated safety notice. Warnings that share a stable
// code collapse into a single group whose Policies list names every affected
// policy, so the same warning is never printed once per policy. Tags are
// aggregated across the merged warnings.
type warningGroup struct {
	Code     config.WarningCode
	Scope    config.WarningScope
	Target   string
	Message  string
	Policies []config.PolicyID
	Tags     []string
}

// warningGroups groups the snapshot's warnings by their stable identity (Code)
// and collects the affected policies from the structured Policy field. Grouping
// never inspects the human message, so re-wording a warning cannot change how it
// is deduplicated, and a warning affecting several policies lists them once.
func warningGroups(warnings []config.Warning) []warningGroup {
	groups := []warningGroup{}
	index := map[config.WarningCode]int{}
	for _, w := range warnings {
		i, ok := index[w.Code]
		if !ok {
			i = len(groups)
			index[w.Code] = i
			groups = append(groups, warningGroup{Code: w.Code, Scope: w.Scope, Target: w.Target, Message: w.Message})
		}
		if w.Policy != "" && !slices.Contains(groups[i].Policies, w.Policy) {
			groups[i].Policies = append(groups[i].Policies, w.Policy)
		}
		for _, tag := range w.Tags {
			if !slices.Contains(groups[i].Tags, tag) {
				groups[i].Tags = append(groups[i].Tags, tag)
			}
		}
	}
	return groups
}

// warningFor returns the first warning carrying a code, so a surface can key on
// a stable identity instead of on message text.
func warningFor(warnings []config.Warning, code config.WarningCode) (config.Warning, bool) {
	for _, w := range warnings {
		if w.Code == code {
			return w, true
		}
	}
	return config.Warning{}, false
}

// diagnosticWarnings keeps the genuinely global notices for the compact,
// expandable diagnostics area. Policy-scoped warnings live on their own rows,
// and the dry-run notice lives in the header badge, so neither is repeated here.
func diagnosticWarnings(warnings []config.Warning) []warningGroup {
	out := []warningGroup{}
	for _, g := range warningGroups(warnings) {
		if g.Scope == config.WarningPolicy || g.Code == config.WarningDryRunDisabled {
			continue
		}
		out = append(out, g)
	}
	return out
}

// remediationLink is one distinct control an operator can reach to change a
// policy's behavior. The three links around the stopped-Arr policy are not
// interchangeable: each names a different control and a different effect.
type remediationLink struct {
	Label string
	Body  string
	Href  string
}

// policyGuide explains a policy that is currently armed, with the operator tags
// it selects on and the distinct settings controls that can disarm it.
type policyGuide struct {
	Message string
	Tags    []string
	Links   []remediationLink
}

// stoppedPolicyWarning returns the guide for the stopped-Arr policy only while
// that policy is armed; otherwise it is absent, so the callout is never shown
// for a policy that cannot act. The matching tags come from the policy view, not
// from the warning text.
func stoppedPolicyWarning(s watchdog.Snapshot) *policyGuide {
	warning, armed := warningFor(s.Warnings, config.WarningStoppedPolicyArmed)
	if !armed {
		return nil
	}
	tags := []string{}
	for _, p := range s.Policies {
		if p.Policy == config.StoppedArrManaged {
			tags = p.MatchTags
			break
		}
	}
	return &policyGuide{
		Message: warning.Message,
		Tags:    tags,
		Links: []remediationLink{
			{Label: "Match tags", Body: "clear them to stop selecting torrents through this policy.", Href: "/settings#policy-stopped_arr_managed-match-tags"},
			{Label: "Action", Body: "choose Report only to prevent this policy deleting torrents.", Href: "/settings#policy-stopped_arr_managed-action"},
			{Label: "Arr recovery", Body: "choose None to prevent this policy requesting recovery.", Href: "/settings#policy-stopped_arr_managed-arr"},
		},
	}
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
