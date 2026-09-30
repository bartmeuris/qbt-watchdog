package watchdog

import (
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
	"time"
)

type PolicyView struct {
	Policy           config.PolicyID `json:"policy"`
	Action           config.Action   `json:"action"`
	EffectiveAction  config.Action   `json:"effective_action"`
	ThresholdSeconds float64         `json:"threshold_seconds"`
	// ArrMode is the configured recovery mode for this policy (inherit, none
	// or an explicit mode). It is published so the UI can explain inheritance
	// and per-service differences without reaching into the running config.
	ArrMode   config.ArrMode `json:"arr_mode"`
	MatchTags []string       `json:"match_tags,omitempty"`
}

// The decision vocabulary is closed. Every value either states that an action
// is possible now, or names the single thing that prevents one, so no row can
// reach a reader unexplained. Presentation layers must cover all of them; the
// wire values are unchanged and remain the contract of Row.Decision.
const (
	DecisionNotApplicable     = "not applicable"
	DecisionTracking          = "tracking"
	DecisionEligible          = "eligible"
	DecisionProtected         = "protected"
	DecisionNonzeroProgress   = "nonzero progress"
	DecisionNonzeroDownloaded = "nonzero downloaded"
	DecisionPayloadPresent    = "payload present"
	DecisionDeleteRequested   = "delete requested"
	DecisionActionsDisabled   = "actions disabled"
	DecisionWarned            = "warned"
	DecisionRetryLimitReached = "retry limit reached"
)

// Decisions lists that closed vocabulary so a presentation layer can be
// checked for completeness instead of trusted to be complete.
func Decisions() []string {
	return []string{
		DecisionNotApplicable, DecisionTracking, DecisionEligible,
		DecisionProtected, DecisionNonzeroProgress, DecisionNonzeroDownloaded,
		DecisionPayloadPresent, DecisionDeleteRequested, DecisionActionsDisabled,
		DecisionWarned, DecisionRetryLimitReached,
	}
}

// DecisionLabels maps every Decision value to its human headline (the same
// twelve pairs the user interface renders). It is a closed, total map so a
// Phase 4 template can rely on every value naming an explanation instead of
// being trusted to. An unknown value is impossible: Decisions() is closed.
func DecisionLabels() map[string]string {
	return map[string]string{
		DecisionNotApplicable:     "Not applicable",
		DecisionTracking:          "Counting down",
		DecisionEligible:          "Eligible now",
		DecisionProtected:         "Protected",
		DecisionNonzeroProgress:   "Has progress",
		DecisionNonzeroDownloaded: "Has payload",
		DecisionPayloadPresent:    "Payload present",
		DecisionDeleteRequested:   "Delete pending",
		DecisionActionsDisabled:   "Actions disabled",
		DecisionWarned:            "Already warned",
		DecisionRetryLimitReached: "Retry limit reached",
	}
}

// Gate Name values are a closed vocabulary. Each names one evaluation stage in
// the exact order evaluate() walks it, so a rendered trace is always total and
// ordered. Presentation layers list them via GateNames and label them via
// GateLabels, never by hard-coding free-form strings.
const (
	GateClassification = "classification"
	GatePayloadPresent = "payload_present"
	GateConfigured     = "configured"
	GateExclusions     = "exclusions"
	GateFieldChecks    = "field_checks"
	GatePendingDelete  = "pending_delete"
	GateContinuity     = "continuity"
	GateThreshold      = "threshold"
	GateCap            = "cap"
	GateDryRun         = "dry_run"
	GateAttempts       = "attempts"
)

// GateNames lists the closed gate vocabulary in evaluation order.
func GateNames() []string {
	return []string{
		GateClassification, GatePayloadPresent, GateConfigured, GateExclusions,
		GateFieldChecks, GatePendingDelete, GateContinuity, GateThreshold,
		GateCap, GateDryRun, GateAttempts,
	}
}

// GateLabels maps every gate Name to a headline for an ordered walkthrough.
func GateLabels() map[string]string {
	return map[string]string{
		GateClassification: "Classification",
		GatePayloadPresent: "Payload present",
		GateConfigured:     "Policy configured",
		GateExclusions:     "Exclusions",
		GateFieldChecks:    "Field checks",
		GatePendingDelete:  "Pending delete",
		GateContinuity:     "Continuity",
		GateThreshold:      "Threshold",
		GateCap:            "Action cap",
		GateDryRun:         "Dry run",
		GateAttempts:       "Attempt budget",
	}
}

// ProtectedBy names each distinct exclusion that can fire, in the order
// protected() evaluates them, so the trace can say which one held.
const (
	ProtectedByCategoryExcluded    = "category-excluded"
	ProtectedByCategoryNotIncluded = "category-not-included"
	ProtectedByTagExcluded         = "tag-excluded"
)

var completedStates = map[string]bool{
	"uploading": true,
	"stalledUP": true,
	"forcedUP":  true,
	"queuedUP":  true,
	// Fully deselected or completed torrents land in qBittorrent's UP state
	// variants. The DL variants are operator-stopped in-progress downloads.
	"pausedUP":  true,
	"stoppedUP": true,
}

var stoppedStates = map[string]bool{
	"pausedUP":  true,
	"stoppedUP": true,
	"pausedDL":  true,
	"stoppedDL": true,
}

func zeroPayload(t qbt.Torrent) bool {
	return t.Size == 0 && t.TotalSize > 0 && t.Downloaded == 0 && t.AmountLeft == 0
}

func (s *Service) policy(t qbt.Torrent) config.PolicyID {
	if t.State == "metaDL" {
		return config.Metadata
	}
	if completedStates[t.State] {
		if zeroPayload(t) {
			return config.CompletedNoData
		}
		if stoppedStates[t.State] && s.matchesStoppedArrManaged(t) {
			return config.StoppedArrManaged
		}
		return ""
	}
	if stoppedStates[t.State] && s.matchesStoppedArrManaged(t) {
		return config.StoppedArrManaged
	}
	if t.State != "stalledDL" || t.Progress >= 1 {
		return ""
	}
	if t.Progress > 0 {
		return config.StalledPartial
	}
	if s.state.SeedObserved[t.Hash] || t.NumSeeds > 0 {
		return config.StalledSeedersSeen
	}
	return config.StalledNoSeeders
}

func (s *Service) matchesStoppedArrManaged(t qbt.Torrent) bool {
	policy, ok := s.c.Policies[config.StoppedArrManaged]
	if !ok || len(policy.MatchTags) == 0 {
		return false
	}
	for _, tag := range s.operatorTags(t.Tags) {
		for _, match := range policy.MatchTags {
			if tag == match {
				return true
			}
		}
	}
	return false
}

func (s *Service) matchingPolicy(t qbt.Torrent) config.PolicyID {
	id := s.policy(t)
	if id == "" || s.protected(t) {
		return ""
	}
	if _, ok := s.c.Policies[id]; !ok {
		return ""
	}
	if id == config.Metadata && t.Progress != 0 {
		return ""
	}
	if id == config.CompletedNoData && !zeroPayload(t) {
		return ""
	}
	if id != config.StalledPartial && id != config.StoppedArrManaged && t.Downloaded != 0 {
		return ""
	}
	return id
}

// PolicyRejection names one configured policy that did not win classification
// for this torrent and why, in a self-contained human sentence fragment.
type PolicyRejection struct {
	Policy config.PolicyID `json:"policy"`
	Reason string          `json:"reason"`
}

// classify returns the single policy that won this torrent's evaluation and,
// for every configured policy that did not win, why it did not. The winner is
// the same value matchingPolicy() yields, so the classifier and the execution
// gate can never disagree; only the explanation is added.
func (s *Service) classify(t qbt.Torrent) (config.PolicyID, []PolicyRejection) {
	won := s.matchingPolicy(t)
	rejections := make([]PolicyRejection, 0, len(s.c.Policies))
	for _, id := range config.PolicyIDs() {
		if _, configured := s.c.Policies[id]; !configured {
			continue
		}
		if id == won {
			continue
		}
		rejections = append(rejections, PolicyRejection{Policy: id, Reason: s.rejectReason(t, id)})
	}
	return won, rejections
}

// rejectReason explains why a configured policy id is not this torrent's
// partition. It mirrors the branches of policy() in reverse, so every string
// is a stable, self-contained fragment a template can render verbatim.
func (s *Service) rejectReason(t qbt.Torrent, id config.PolicyID) string {
	if s.protected(t) {
		return "protected by exclusion"
	}
	if id == config.Metadata && t.State != "metaDL" {
		return "state is not metaDL"
	}
	if id == config.Metadata {
		return "metadata state matched, but nonzero progress narrowed it away"
	}
	switch id {
	case config.CompletedNoData:
		if !completedStates[t.State] {
			return "state is not a completed state"
		}
		if zeroPayload(t) {
			return "completed with no payload, but another partition took precedence"
		}
		return "downloaded payload present"
	case config.StoppedArrManaged:
		if !stoppedStates[t.State] && !completedStates[t.State] {
			return "state is not a stopped state"
		}
		if !s.matchesStoppedArrManaged(t) {
			return "stopped but not Arr-managed via match tags"
		}
		return "stopped and Arr-managed, but completed-no-data took precedence"
	default: // the three stalled partitions
		if t.State != "stalledDL" {
			return "state is not stalledDL"
		}
		if t.Progress >= 1 {
			return "progress >= 1"
		}
		if t.Progress > 0 {
			return "progress > 0 so it would be partial"
		}
		seeded := s.state.SeedObserved[t.Hash] || t.NumSeeds > 0
		if id == config.StalledSeedersSeen && !seeded {
			return "no seeders observed after payload check"
		}
		if id == config.StalledNoSeeders && seeded {
			return "seeders observed, so it would be seeders-seen"
		}
		return "a sibling stalled partition won"
	}
}

// Retain exits, transitions and seed sightings even if later batch reads fail.
// A positive repeat alone never advances continuity in a failed batch.
func (s *Service) observeNegative(hash string, t *qbt.Torrent, now time.Time) {
	if t == nil {
		delete(s.state.SeedObserved, hash)
		delete(s.state.Tracked, hash)
		return
	}
	if t.NumSeeds > 0 {
		s.state.SeedObserved[hash] = true
	}
	e, exists := s.state.Tracked[hash]
	if !exists {
		return
	}
	id := s.matchingPolicy(*t)
	if id == e.Policy {
		return
	}
	if id == "" && e.DeleteRequestedAt == nil {
		delete(s.state.Tracked, hash)
		return
	}
	s.state.Tracked[hash] = store.Episode{Policy: id, FirstSeen: now, LastSeen: now, Attempts: carriedAttempts(e), DeleteRequestedAt: e.DeleteRequestedAt}
}

// carriedAttempts clears the per-episode attempt budget when a torrent changes
// partition, because the new partition is a new decision and must start with a
// full, finite budget of its own. A request still awaiting confirmation is the
// single exception: its attempt stays counted, so a torrent cannot refresh its
// budget mid-flight by flapping between partitions. Refreshing the budget is
// still bounded, because a new partition must also re-prove its whole
// threshold from the reset episode clock before it may act at all.
func carriedAttempts(e store.Episode) int {
	if e.DeleteRequestedAt != nil {
		return e.Attempts
	}
	return 0
}
