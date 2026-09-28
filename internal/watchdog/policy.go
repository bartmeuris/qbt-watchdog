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
		DecisionDeleteRequested, DecisionActionsDisabled, DecisionWarned,
		DecisionRetryLimitReached,
	}
}

func (s *Service) policy(t qbt.Torrent) config.PolicyID {
	if t.State == "metaDL" {
		return config.Metadata
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

func (s *Service) matchingPolicy(t qbt.Torrent) config.PolicyID {
	id := s.policy(t)
	if id == "" || s.protected(t) {
		return ""
	}
	if id == config.Metadata && t.Progress != 0 {
		return ""
	}
	if id != config.StalledPartial && t.Downloaded != 0 {
		return ""
	}
	return id
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
