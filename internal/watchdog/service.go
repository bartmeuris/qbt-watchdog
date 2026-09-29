package watchdog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

type Client interface {
	Versions(context.Context) (string, string, error)
	List(context.Context) ([]qbt.Torrent, error)
	Get(context.Context, string) (*qbt.Torrent, error)
	Delete(context.Context, string, bool) error
	AddTags(context.Context, []string, string) error
	RemoveTags(context.Context, []string, string) error
}
type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

type Row struct {
	Policy config.PolicyID `json:"policy"`
	// ConfiguredAction is what the operator wrote in the configuration file
	// and EffectiveAction is what the engine may actually do, so a dry-run
	// downgrade is visible per row instead of only in the global banner.
	// Both are copied from the row's PolicyView, which stays the single
	// source of the per-policy model; see publish.
	//
	// The field is purely additive, so the payload schema version does not
	// move. That version tracks store.SchemaVersion and only advances when
	// the meaning of an existing field changes.
	ConfiguredAction config.Action `json:"configured_action"`
	EffectiveAction  config.Action `json:"effective_action"`
	ThresholdSeconds float64       `json:"threshold_seconds"`
	SeedObserved     bool          `json:"seed_observed"`
	Name             string        `json:"name"`
	ShortHash        string        `json:"short_hash"`
	State            string        `json:"state"`
	Progress         float64       `json:"progress"`
	Downloaded       int64         `json:"downloaded"`
	Size             int64         `json:"size"`
	TotalSize        int64         `json:"total_size"`
	Completed        int64         `json:"completed"`
	AmountLeft       int64         `json:"amount_left"`
	DownloadSpeed    int64         `json:"download_speed"`
	NumSeeds         int           `json:"num_seeds"`
	NumLeechers      int           `json:"num_leechers"`
	Category         string        `json:"category"`
	Tags             string        `json:"tags"`
	WatchdogTags     []string      `json:"watchdog_tags"`
	DesiredTags      []string      `json:"desired_watchdog_tags"`
	AddedAt          *time.Time    `json:"added_at"`
	FirstSeen        *time.Time    `json:"first_seen_policy"`
	Elapsed          float64       `json:"elapsed_seconds"`
	Remaining        float64       `json:"remaining_seconds"`
	Decision         string        `json:"decision"`
}
type Summary struct {
	Total           int `json:"total"`
	Metadata        int `json:"metadata"`
	Protected       int `json:"protected"`
	Overdue         int `json:"overdue"`
	WouldDelete     int `json:"would_delete"`
	DeleteRequested int `json:"delete_requested"`
}
type Snapshot struct {
	Integrations []IntegrationStatus `json:"integrations"`
	RecoveryJobs []RecoveryStatus    `json:"recovery_jobs"`
	// ConfigStatus is the reload health owned by config.Manager. The service
	// keeps no copy of it, so the UI, the status payload, the metrics and
	// /readyz can never report three different answers.
	ConfigStatus     config.Status       `json:"config"`
	Policies         []PolicyView        `json:"policies"`
	SchemaVersion    int                 `json:"schema_version"`
	Build            observability.Build `json:"build"`
	DryRun           bool                `json:"dry_run"`
	TagSync          TagSyncStatus       `json:"tag_sync"`
	QBTUp            bool                `json:"qbt_up"`
	QBTVersion       string              `json:"qbt_version"`
	WebAPIVersion    string              `json:"webapi_version"`
	LastSuccess      *time.Time          `json:"last_successful_poll"`
	PollError        string              `json:"poll_error"`
	NextPoll         *time.Time          `json:"next_poll"`
	PersistenceError string              `json:"persistence_error"`
	StateLoadWarning string              `json:"state_load_warning"`
	UpdatedAt        time.Time           `json:"updated_at"`
	Summary          Summary             `json:"summary"`
	SinceStartup     store.Counters      `json:"since_startup"`
	Lifetime         store.Counters      `json:"lifetime"`
	Torrents         []Row               `json:"torrents"`
	History          []store.Event       `json:"history"`
	RefreshSeconds   float64             `json:"refresh_seconds"`
}

type TagSyncStatus struct {
	Enabled          bool   `json:"enabled"`
	Prefix           string `json:"prefix"`
	PersistedPrefix  string `json:"persisted_prefix"`
	DryRunSuppressed bool   `json:"dry_run_suppressed"`
	MaxWritesPerPoll int    `json:"max_writes_per_poll"`
}

type Service struct {
	recoveryPolling [2]atomic.Bool
	arrFactory      ArrFactory
	integrations    map[config.ArrKind]*integrationRuntime
	running         atomic.Bool
	savedDigest     [32]byte
	savedKnown      bool
	mu              sync.Mutex
	// manager is the only owner of the running configuration and of reload
	// health; the service never keeps a second copy of either.
	manager  *config.Manager
	reloaded chan struct{}
	c        config.Config
	client   Client
	disk     store.Store
	clock    Clock
	log      *slog.Logger
	metrics  *observability.Metrics
	state    store.State
	startup  store.Counters
	snapshot atomic.Pointer[Snapshot]
	polling  atomic.Bool
	view     Snapshot
	torrents []qbt.Torrent
	// loggedReloadError deduplicates log lines only; reload health itself is
	// always read from manager.Status.
	loggedReloadError string
	writeBlocked      bool
}

func New(c config.Config, client Client, disk store.Store, clock Clock, log *slog.Logger, metrics *observability.Metrics, build observability.Build) *Service {
	state, err := disk.Load(clock.Now())
	endpointChanged := state.EndpointKey != "" && state.EndpointKey != c.EndpointKey()
	s := &Service{c: c.Clone(), client: client, disk: disk, clock: clock, log: log, metrics: metrics, state: state, manager: config.NewManager(c.ConfigFile, c), reloaded: make(chan struct{}, 1), view: Snapshot{SchemaVersion: store.SchemaVersion, Build: build, DryRun: c.DryRun, RefreshSeconds: c.UIRefreshInterval.Seconds()}}
	if s.state.SeedObserved == nil {
		s.state.SeedObserved = map[string]bool{}
	}
	if s.state.EndpointKey != c.EndpointKey() {
		s.state.SeedObserved = map[string]bool{}
		if s.state.EndpointKey != "" {
			s.state.Tracked = map[string]store.Episode{}
		}
	}
	if s.state.SafetyKey != c.SafetyKey() {
		s.resetTimers()
	}
	s.state.SafetyKey, s.state.EndpointKey = c.SafetyKey(), c.EndpointKey()
	if err != nil {
		s.view.StateLoadWarning = err.Error()
		log.Warn("state load problem", "event", "state_load_error", "error", err)
		if !errors.Is(err, store.ErrCorruptRecovered) {
			s.writeBlocked = true
			s.view.PersistenceError = "state could not be safely loaded; operator intervention required"
		}
	}
	s.initRecovery()
	if endpointChanged {
		for _, job := range s.state.RecoveryJobs {
			s.finishRecovery(job, "endpoint_changed")
		}
	}
	s.publish(nil)
	return s
}

// Snapshot returns a detached copy. The published object and its backing slices
// are immutable; HTTP consumers cannot mutate service state.
func (s *Service) Snapshot() Snapshot {
	v := *s.snapshot.Load()
	v.Torrents = slices.Clone(v.Torrents)
	v.History = slices.Clone(v.History)
	v.Policies = slices.Clone(v.Policies)
	for i := range v.Policies {
		v.Policies[i].MatchTags = slices.Clone(v.Policies[i].MatchTags)
	}
	v.Integrations = slices.Clone(v.Integrations)
	v.RecoveryJobs = slices.Clone(v.RecoveryJobs)
	v.LastSuccess = cloneTime(v.LastSuccess)
	v.NextPoll = cloneTime(v.NextPoll)
	for i := range v.Torrents {
		v.Torrents[i].FirstSeen = cloneTime(v.Torrents[i].FirstSeen)
		v.Torrents[i].AddedAt = cloneTime(v.Torrents[i].AddedAt)
		v.Torrents[i].WatchdogTags = slices.Clone(v.Torrents[i].WatchdogTags)
		v.Torrents[i].DesiredTags = slices.Clone(v.Torrents[i].DesiredTags)
	}
	return v
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	copy := *t
	return &copy
}

func (s *Service) protected(t qbt.Torrent) bool {
	if slices.Contains(s.c.ExcludeCategories, t.Category) {
		return true
	}
	if len(s.c.IncludeCategories) > 0 && !slices.Contains(s.c.IncludeCategories, t.Category) {
		return true
	}
	for _, tag := range s.operatorTags(t.Tags) {
		if slices.Contains(s.c.ExcludeTags, tag) {
			return true
		}
	}
	return false
}

func (s *Service) operatorTags(raw string) []string {
	tags := config.Split(raw)
	result := tags[:0]
	for _, tag := range tags {
		if !s.isWatchdogTag(tag) {
			result = append(result, tag)
		}
	}
	return result
}

func (s *Service) isWatchdogTag(tag string) bool {
	if strings.HasPrefix(tag, s.c.TagSync.Prefix) {
		return true
	}
	return s.state.WatchdogTagPrefix != "" && strings.HasPrefix(tag, s.state.WatchdogTagPrefix)
}

func (s *Service) decision(t qbt.Torrent, e store.Episode, now time.Time) string {
	id := s.policy(t)
	if id == "" && completedStates[t.State] && t.TotalSize > 0 && !zeroPayload(t) {
		return DecisionPayloadPresent
	}
	if id == "" {
		return DecisionNotApplicable
	}
	p, ok := s.c.Policies[id]
	if !ok {
		return DecisionNotApplicable
	}
	if s.protected(t) {
		return DecisionProtected
	}
	if id == config.Metadata && t.Progress != 0 {
		return DecisionNonzeroProgress
	}
	if id == config.CompletedNoData && !zeroPayload(t) {
		return DecisionPayloadPresent
	}
	if id != config.StalledPartial && id != config.StoppedArrManaged && t.Downloaded != 0 {
		return DecisionNonzeroDownloaded
	}
	if e.DeleteRequestedAt != nil {
		return DecisionDeleteRequested
	}
	if e.Policy != id || e.FirstSeen.IsZero() || now.Before(e.LastSeen) || now.Sub(e.LastSeen) > s.c.MaxObservationGap {
		return DecisionTracking
	}
	if now.Sub(e.FirstSeen) < p.Threshold {
		return DecisionTracking
	}
	if s.c.MaxDeletions == 0 {
		return DecisionActionsDisabled
	}
	if s.c.EffectiveAction(id) == config.Warn {
		if e.DryRunNotified {
			return DecisionWarned
		}
		return DecisionEligible
	}
	if e.Attempts >= store.MaxAttempts {
		return DecisionRetryLimitReached
	}
	return DecisionEligible
}

func (s *Service) event(action, outcome string, t qbt.Torrent, detail string) {
	id := s.policy(t)
	if id == "" {
		id = s.state.Tracked[t.Hash].Policy
	}
	e := (store.Event{Policy: id, EffectiveAction: s.c.EffectiveAction(id), Time: s.clock.Now().UTC(), Action: action, Outcome: outcome, Name: t.Name, ShortHash: qbt.ShortHash(t.Hash), DryRun: s.c.DryRun, Error: detail}).Bounded()
	s.state.History = append(s.state.History, e)
	if len(s.state.History) > s.c.HistoryLimit {
		s.state.History = s.state.History[len(s.state.History)-s.c.HistoryLimit:]
	}
	s.metrics.Actions.WithLabelValues(action, outcome, strconv.FormatBool(s.c.DryRun)).Inc()
	if id.Valid() {
		s.metrics.PolicyActions.WithLabelValues(string(id), string(e.EffectiveAction), outcome).Inc()
	}
	level := slog.LevelInfo
	if action == "warn" {
		level = slog.LevelWarn
	}
	s.log.Log(context.Background(), level, "watchdog action", "event", action, "policy", id, "effective_action", e.EffectiveAction, "outcome", outcome, "hash", e.ShortHash, "dry_run", s.c.DryRun, "error", detail)
	if action == "action_confirmed" {
		s.log.Info("deletion disappearance confirmed", "event", "delete_succeeded", "hash", e.ShortHash)
	}
}

func (s *Service) endObservedEpisodes(ts []qbt.Torrent) {
	present := map[string]qbt.Torrent{}
	for _, t := range ts {
		present[t.Hash] = t
		s.observeNegative(t.Hash, &t, s.clock.Now())
	}
	for hash := range s.state.SeedObserved {
		if _, ok := present[hash]; !ok {
			delete(s.state.SeedObserved, hash)
		}
	}
	for hash, e := range s.state.Tracked {
		t, exists := present[hash]
		if !exists {
			if e.DeleteRequestedAt != nil {
				s.state.Counters.Deletions++
				s.startup.Deletions++
				s.event("action_confirmed", "success", qbt.Torrent{Hash: hash}, "")
			}
			delete(s.state.Tracked, hash)
			s.log.Debug("tracking stopped", "event", "tracking_stop", "hash", qbt.ShortHash(hash))
			continue
		}
		if s.matchingPolicy(t) == "" && e.DeleteRequestedAt == nil {
			delete(s.state.Tracked, hash)
			s.log.Debug("tracking stopped", "event", "tracking_stop", "hash", qbt.ShortHash(hash))
		}
	}
}

func (s *Service) reconcile(ts []qbt.Torrent, now time.Time) {
	s.endObservedEpisodes(ts)
	for _, t := range ts {
		id := s.matchingPolicy(t)
		if id == "" {
			continue
		}
		e, exists := s.state.Tracked[t.Hash]
		repartitioned := !exists || e.Policy != id
		interrupted := now.Before(e.LastSeen) || now.Sub(e.LastSeen) > s.c.MaxObservationGap
		if repartitioned || interrupted {
			// A gap resets proven time, not the fact an external action was
			// attempted, so it keeps the retry budget. A partition change is a
			// new decision and starts its budget fresh; see carriedAttempts.
			attempts := e.Attempts
			if repartitioned {
				attempts = carriedAttempts(e)
			}
			e = store.Episode{Policy: id, FirstSeen: now, LastSeen: now, DeleteRequestedAt: e.DeleteRequestedAt, Attempts: attempts}
			s.log.Debug("policy episode start/reset", "event", "tracking_reset", "policy", id, "hash", qbt.ShortHash(t.Hash))
		}
		e.LastSeen = now
		if e.DeleteRequestedAt != nil && now.Sub(*e.DeleteRequestedAt) >= s.c.DeleteConfirmationTimeout {
			e.DeleteRequestedAt = nil
		}
		s.state.Tracked[t.Hash] = e
	}
}

func (s *Service) fail(err error) {
	s.view.PollError = err.Error()
	s.view.QBTUp = false
	s.metrics.Up.Set(0)
	s.metrics.PollErrors.Inc()
	s.log.Warn("poll failed", "event", "poll_error", "error", err)
}
func (s *Service) persist() {
	_ = s.persistState()
}

func (s *Service) persistState() bool {
	if s.writeBlocked {
		return false
	}
	s.state.SafetyKey, s.state.EndpointKey = s.c.SafetyKey(), s.c.EndpointKey()
	data, err := json.Marshal(s.state)
	if err != nil {
		s.view.PersistenceError = "cannot encode state"
		s.savedKnown = false
		return false
	}
	digest := sha256.Sum256(data)
	if s.savedKnown && digest == s.savedDigest && s.view.PersistenceError == "" {
		return true
	}
	if err := s.disk.Save(s.state); err != nil {
		s.savedKnown = false
		s.view.PersistenceError = err.Error()
		s.metrics.StateErrors.Inc()
		s.log.Error("state write failed", "event", "state_write_error", "error", err)
		return false
	}
	s.view.PersistenceError = ""
	s.savedKnown, s.savedDigest = true, digest
	return true
}

// Poll is serialized even when called outside Run. Initial list and candidate
// reads finish before any action. Each mutation has an additional immediate
// confirmation; later failures stop the batch, but cannot undo accepted actions.
func (s *Service) Poll(ctx context.Context) error {
	if !s.polling.CompareAndSwap(false, true) {
		return errors.New("poll already running")
	}
	defer s.polling.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
	defer func() { s.metrics.Duration.Observe(time.Since(started).Seconds()) }()
	defer func() { s.persist(); s.publish(nil) }()
	if err := s.tagPrefixBootError(); err != nil {
		s.fail(err)
		return err
	}
	app, api, err := s.client.Versions(ctx)
	if err != nil {
		s.fail(err)
		return err
	}
	ts, err := s.client.List(ctx)
	if err != nil {
		s.fail(err)
		return err
	}
	now := s.clock.Now().UTC()
	// Determine candidates without generating externally visible events.
	s.expireRecovery(now)
	s.confirmRecovery(ts, now)
	candidates := []qbt.Torrent{}
	for _, t := range ts {
		e, ok := s.state.Tracked[t.Hash]
		if !ok {
			continue
		}
		if e.DeleteRequestedAt != nil && now.Sub(*e.DeleteRequestedAt) >= s.c.DeleteConfirmationTimeout {
			e.DeleteRequestedAt = nil
		}
		if s.decision(t, e, now) == DecisionEligible {
			candidates = append(candidates, t)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := s.state.Tracked[candidates[i].Hash], s.state.Tracked[candidates[j].Hash]
		if a.FirstSeen.Equal(b.FirstSeen) {
			return candidates[i].Hash < candidates[j].Hash
		}
		return a.FirstSeen.Before(b.FirstSeen)
	})
	if len(candidates) > s.c.MaxDeletions {
		candidates = candidates[:s.c.MaxDeletions]
	}
	// Negative observations end episodes even when a later read aborts actions.
	s.endObservedEpisodes(ts)
	s.torrents = slices.Clone(ts)
	fresh := map[string]*qbt.Torrent{}
	for _, t := range candidates {
		confirmed, e := s.client.Get(ctx, t.Hash)
		if e != nil {
			s.fail(e)
			return e
		}
		fresh[t.Hash] = confirmed
		s.replaceTorrent(t.Hash, confirmed)
		s.observeNegative(t.Hash, confirmed, s.clock.Now())
	}
	if err := ctx.Err(); err != nil {
		s.fail(err)
		return err
	}
	s.reconcile(s.torrents, now)
	if s.view.QBTVersion != app || s.view.WebAPIVersion != api {
		s.log.Info("qBittorrent versions detected", "event", "qbt_versions", "application_version", app, "webapi_version", api)
	}
	s.view.QBTVersion = app
	s.view.WebAPIVersion = api
	s.view.PollError = ""
	s.view.QBTUp = true
	s.view.LastSuccess = &now
	s.metrics.Up.Set(1)
	s.metrics.LastSuccess.Set(float64(now.Unix()))
	for _, t := range candidates {
		confirmed := fresh[t.Hash]
		e, ok := s.state.Tracked[t.Hash]
		if confirmed == nil || s.matchingPolicy(*confirmed) == "" {
			s.replaceTorrent(t.Hash, confirmed)
			s.event("action_skipped", "skipped", t, "torrent disappeared or left its policy partition")
			continue
		}
		s.replaceTorrent(t.Hash, confirmed)
		if !ok || s.decision(*confirmed, e, s.clock.Now()) != DecisionEligible {
			s.event("action_skipped", "skipped", t, "fresh safety or continuity check failed")
			continue
		}
		if s.c.EffectiveAction(e.Policy) == config.Warn {
			e.DryRunNotified = true
			s.state.Tracked[t.Hash] = e
			s.state.Counters.WouldDeletions++
			s.startup.WouldDeletions++
			s.event("warn", "success", *confirmed, "")
			continue
		}
		// Reserve durably before the final read; no persistence may separate
		// that read from the mutation. Known-unsent reservations are released.
		reserved := e
		reserved.Attempts++
		s.state.Tracked[t.Hash] = reserved
		s.prepareRecovery(*confirmed, e)
		s.persist()
		if s.view.PersistenceError != "" {
			s.releaseRecovery(t.Hash)
			s.state.Tracked[t.Hash] = e
			s.event("action_skipped", "skipped", t, "state persistence unavailable")
			continue
		}
		confirmed, err = s.client.Get(ctx, t.Hash)
		if err != nil {
			s.releaseRecovery(t.Hash)
			s.state.Tracked[t.Hash] = e
			s.fail(err)
			return err
		}
		// Release the known-unsent reservation before retaining negative observations.
		s.state.Tracked[t.Hash] = e
		s.observeNegative(t.Hash, confirmed, s.clock.Now())
		if confirmed == nil || s.matchingPolicy(*confirmed) == "" {
			s.releaseRecovery(t.Hash)
			s.replaceTorrent(t.Hash, confirmed)
			s.event("action_skipped", "skipped", t, "torrent disappeared or left its policy partition")
			continue
		}
		s.replaceTorrent(t.Hash, confirmed)
		// Check the pre-reservation budget so its final slot remains usable.
		if s.decision(*confirmed, s.state.Tracked[t.Hash], s.clock.Now()) != DecisionEligible || s.decision(*confirmed, e, s.clock.Now()) != DecisionEligible {
			s.releaseRecovery(t.Hash)
			s.event("action_skipped", "skipped", t, "fresh safety or continuity check failed")
			continue
		}
		if err = ctx.Err(); err != nil {
			s.releaseRecovery(t.Hash)
			s.state.Tracked[t.Hash] = e
			s.fail(err)
			return err
		}
		s.state.Tracked[t.Hash] = reserved
		if err = s.client.Delete(ctx, t.Hash, s.c.EffectiveAction(e.Policy) == config.DeleteFile); err != nil {
			s.releaseRecovery(t.Hash)
			s.event("action_failed", "failed", t, "delete request failed")
			s.fail(err)
			return err
		}
		requested := s.clock.Now().UTC()
		s.acceptedRecovery(t.Hash, requested)
		reserved.DeleteRequestedAt = &requested
		s.state.Tracked[t.Hash] = reserved
		s.state.Counters.DeleteRequests++
		s.startup.DeleteRequests++
		s.event("action_requested", "accepted", *confirmed, "")
		s.persist()
	}
	if err := s.syncWatchdogTags(ctx, now); err != nil {
		s.fail(err)
		return err
	}
	s.log.Debug("poll succeeded", "event", "poll_success", "torrents", len(ts))
	return nil
}

func (s *Service) replaceTorrent(hash string, t *qbt.Torrent) {
	for i := range s.torrents {
		if s.torrents[i].Hash == hash {
			if t == nil {
				s.torrents = append(s.torrents[:i], s.torrents[i+1:]...)
			} else {
				s.torrents[i] = *t
			}
			return
		}
	}
}

func (s *Service) tagPrefixBootError() error {
	if !s.c.TagSync.Enabled || s.state.WatchdogTagPrefix == "" || s.state.WatchdogTagPrefix == s.c.TagSync.Prefix {
		return nil
	}
	return fmt.Errorf("tag_sync prefix mismatch: state claims %q, config wants %q; disable tag_sync to remediate", s.state.WatchdogTagPrefix, s.c.TagSync.Prefix)
}

func (s *Service) ensureTagPrefixClaim() bool {
	if s.state.WatchdogTagPrefix == s.c.TagSync.Prefix {
		return true
	}
	if s.state.WatchdogTagPrefix != "" && s.state.WatchdogTagPrefix != s.c.TagSync.Prefix {
		s.view.PersistenceError = "tag_sync prefix mismatch; disable tag_sync to remediate"
		return false
	}
	s.state.WatchdogTagPrefix = s.c.TagSync.Prefix
	if s.persistState() {
		return true
	}
	s.state.WatchdogTagPrefix = ""
	return false
}

func (s *Service) syncWatchdogTags(ctx context.Context, now time.Time) error {
	if s.c.DryRun {
		return nil
	}
	if s.state.WatchdogTagPrefix != "" && !s.c.TagSync.Enabled {
		return s.sweepWatchdogTags(ctx, s.state.WatchdogTagPrefix)
	}
	if !s.c.TagSync.Enabled {
		return nil
	}
	if !s.ensureTagPrefixClaim() {
		return nil
	}
	addByTag := map[string][]string{}
	removeByTag := map[string][]string{}
	for _, t := range s.torrents {
		e := s.state.Tracked[t.Hash]
		if e.DeleteRequestedAt != nil {
			continue
		}
		actual := stringSet(s.watchdogTags(t.Tags, s.c.TagSync.Prefix))
		desired := stringSet(s.desiredWatchdogTags(t, now))
		for tag := range desired {
			if !actual[tag] {
				addByTag[tag] = append(addByTag[tag], t.Hash)
			}
		}
		for tag := range actual {
			if !desired[tag] {
				removeByTag[tag] = append(removeByTag[tag], t.Hash)
			}
		}
	}
	return s.applyTagWrites(ctx, addByTag, removeByTag, s.c.TagSync.MaxWritesPerPoll)
}

func (s *Service) sweepWatchdogTags(ctx context.Context, prefix string) error {
	removeByTag := map[string][]string{}
	for _, t := range s.torrents {
		for _, tag := range s.watchdogTags(t.Tags, prefix) {
			removeByTag[tag] = append(removeByTag[tag], t.Hash)
		}
	}
	if len(removeByTag) == 0 {
		s.state.WatchdogTagPrefix = ""
		return nil
	}
	complete, err := s.applyTagWritesBounded(ctx, nil, removeByTag, s.c.TagSync.MaxWritesPerPoll)
	if err != nil {
		return err
	}
	if complete {
		s.state.WatchdogTagPrefix = ""
	}
	return nil
}

func (s *Service) applyTagWrites(ctx context.Context, addByTag, removeByTag map[string][]string, limit int) error {
	_, err := s.applyTagWritesBounded(ctx, addByTag, removeByTag, limit)
	return err
}

func (s *Service) applyTagWritesBounded(ctx context.Context, addByTag, removeByTag map[string][]string, limit int) (bool, error) {
	writes := 0
	for _, tag := range sortedKeys(removeByTag) {
		if writes >= limit {
			return false, nil
		}
		if err := s.client.RemoveTags(ctx, removeByTag[tag], tag); err != nil && !benignTagRace(err) {
			return false, err
		}
		writes++
	}
	for _, tag := range sortedKeys(addByTag) {
		if writes >= limit {
			return false, nil
		}
		if err := s.client.AddTags(ctx, addByTag[tag], tag); err != nil && !benignTagRace(err) {
			return false, err
		}
		writes++
	}
	return true, nil
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key, hashes := range m {
		if len(hashes) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func benignTagRace(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "404") || strings.Contains(message, "not found")
}

func (s *Service) watchdogTags(raw, prefix string) []string {
	tags := []string{}
	for _, tag := range config.Split(raw) {
		if strings.HasPrefix(tag, prefix) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func (s *Service) desiredWatchdogTags(t qbt.Torrent, now time.Time) []string {
	e, ok := s.state.Tracked[t.Hash]
	if !ok || e.DeleteRequestedAt != nil || e.Policy == "" || e.Policy != s.matchingPolicy(t) {
		return nil
	}
	policy, ok := s.c.Policies[e.Policy]
	if !ok {
		return nil
	}
	desired := []string{s.c.TagSync.Prefix + string(e.Policy)}
	if !e.FirstSeen.IsZero() && now.Sub(e.FirstSeen) >= policy.Threshold {
		desired = append(desired, s.c.TagSync.Prefix+"due")
	}
	return desired
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func (s *Service) publish(next *time.Time) {
	now := s.clock.Now().UTC()
	v := s.view
	v.Integrations, v.RecoveryJobs = s.recoverySnapshot(now)
	v.DryRun = s.c.DryRun
	v.TagSync = TagSyncStatus{Enabled: s.c.TagSync.Enabled, Prefix: s.c.TagSync.Prefix, PersistedPrefix: s.state.WatchdogTagPrefix, DryRunSuppressed: s.c.DryRun, MaxWritesPerPoll: s.c.TagSync.MaxWritesPerPoll}
	v.RefreshSeconds = s.c.UIRefreshInterval.Seconds()
	v.ConfigStatus = s.manager.Status()
	healthy := 0.
	if v.ConfigStatus.Healthy() {
		healthy = 1
	}
	s.metrics.ReloadHealthy.Set(healthy)
	s.metrics.ConfigGeneration.Set(float64(v.ConfigStatus.Generation))
	v.Policies = nil
	for _, id := range config.PolicyIDs() {
		p, ok := s.c.Policies[id]
		if !ok {
			continue
		}
		v.Policies = append(v.Policies, PolicyView{Policy: id, Action: p.Action, EffectiveAction: s.c.EffectiveAction(id), ThresholdSeconds: p.Threshold.Seconds(), MatchTags: slices.Clone(p.MatchTags)})
	}
	// Rows read their configured action, effective action and threshold from
	// the published per-policy model, so the table can never disagree with the
	// policy summary above it. Unclassified rows are simply absent from the
	// map and therefore carry no action and no threshold at all.
	byPolicy := make(map[config.PolicyID]PolicyView, len(v.Policies))
	for _, p := range v.Policies {
		byPolicy[p.Policy] = p
	}
	v.UpdatedAt = now
	v.NextPoll = next
	v.SinceStartup = s.startup
	v.Lifetime = s.state.Counters
	v.History = slices.Clone(s.state.History)
	if v.History == nil {
		v.History = []store.Event{}
	}
	v.Torrents = make([]Row, 0, len(s.torrents))
	v.Summary = Summary{Total: len(s.torrents)}
	for _, t := range s.torrents {
		e := s.state.Tracked[t.Hash]
		decision := s.decision(t, e, now)
		r := Row{Name: t.Name, ShortHash: qbt.ShortHash(t.Hash), State: t.State, Progress: t.Progress, Downloaded: t.Downloaded, Size: t.Size, TotalSize: t.TotalSize, Completed: t.Completed, AmountLeft: t.AmountLeft, DownloadSpeed: t.DownloadSpeed, NumSeeds: t.NumSeeds, NumLeechers: t.NumLeechers, Category: t.Category, Tags: t.Tags, WatchdogTags: s.watchdogTags(t.Tags, s.c.TagSync.Prefix), Decision: decision}
		r.Policy = s.policy(t)
		r.DesiredTags = s.desiredWatchdogTags(t, now)
		r.SeedObserved = s.state.SeedObserved[t.Hash]
		if p, classified := byPolicy[r.Policy]; classified {
			r.ConfiguredAction, r.EffectiveAction, r.ThresholdSeconds = p.Action, p.EffectiveAction, p.ThresholdSeconds
		}
		if t.AddedOn > 0 {
			added := time.Unix(t.AddedOn, 0).UTC()
			r.AddedAt = &added
		}
		if t.State == "metaDL" {
			v.Summary.Metadata++
		}
		if r.Policy != "" && e.Policy == r.Policy {
			if !e.FirstSeen.IsZero() {
				first := e.FirstSeen
				r.FirstSeen = &first
				r.Elapsed = max(0, e.LastSeen.Sub(first).Seconds())
				r.Remaining = r.ThresholdSeconds - r.Elapsed
				if r.Remaining <= 0 {
					v.Summary.Overdue++
				}
			}
		}
		switch decision {
		case DecisionProtected:
			v.Summary.Protected++
		case DecisionWarned:
			v.Summary.WouldDelete++
		case DecisionDeleteRequested:
			v.Summary.DeleteRequested++
		}
		v.Torrents = append(v.Torrents, r)
	}
	sort.SliceStable(v.Torrents, func(i, j int) bool {
		a, b := v.Torrents[i], v.Torrents[j]
		ao, bo := a.FirstSeen != nil && a.Remaining <= 0, b.FirstSeen != nil && b.Remaining <= 0
		if ao != bo {
			return ao
		}
		if a.Remaining != b.Remaining {
			return a.Remaining < b.Remaining
		}
		return a.ShortHash < b.ShortHash
	})
	s.metrics.Total.Set(float64(v.Summary.Total))
	s.metrics.Tracked.Set(float64(len(s.state.Tracked)))
	s.metrics.Overdue.Set(float64(v.Summary.Overdue))
	for _, policy := range v.Policies {
		tracked, overdue := 0, 0
		for _, row := range v.Torrents {
			if row.Policy != policy.Policy || row.FirstSeen == nil {
				continue
			}
			tracked++
			if row.Remaining <= 0 {
				overdue++
			}
		}
		id := string(policy.Policy)
		s.metrics.PolicyTracked.WithLabelValues(id).Set(float64(tracked))
		s.metrics.PolicyOverdue.WithLabelValues(id).Set(float64(overdue))
		s.metrics.PolicyThreshold.WithLabelValues(id).Set(policy.ThresholdSeconds)
		for _, action := range []config.Action{config.Warn, config.Delete, config.DeleteFile} {
			enabled := 0.
			if action == policy.EffectiveAction {
				enabled = 1
			}
			s.metrics.PolicyEffective.WithLabelValues(id, string(action)).Set(enabled)
		}
	}
	s.snapshot.Store(&v)
}

func (s *Service) Run(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	defer s.running.Store(false)
	stopRecovery := s.startRecovery(ctx)
	defer stopRecovery()
	failures := 0
	for ctx.Err() == nil {
		err := s.Poll(ctx)
		if ctx.Err() != nil {
			return
		}
		c := s.Config()
		delay := c.PollInterval
		if err != nil {
			failures = min(failures+1, 6)
			maximum := max(c.PollInterval, 5*time.Minute)
			for range failures {
				if delay >= maximum/2 {
					delay = maximum
					break
				}
				delay *= 2
			}
			delay = time.Duration(float64(delay) * (0.75 + rand.Float64()*0.25))
			s.log.Warn("poll backoff", "event", "poll_backoff", "delay", delay)
		} else {
			failures = 0
		}
		next := s.clock.Now().Add(delay).UTC()
		if s.polling.CompareAndSwap(false, true) {
			s.mu.Lock()
			s.publish(&next)
			s.mu.Unlock()
			s.polling.Store(false)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.reloaded:
			timer.Stop()
		}
	}
}

// Ready reports whether the process is converged. A rejected reload degrades
// readiness deliberately: the file on disk is not what the process is running,
// so an orchestrator must not treat the rollout as complete. Liveness is
// unaffected, because the last known good configuration keeps working.
func Ready(v Snapshot, now time.Time, maxAge time.Duration) (bool, string) {
	if !v.ConfigStatus.Healthy() {
		return false, "configuration reload rejected"
	}
	if v.LastSuccess == nil {
		return false, "no successful poll yet"
	}
	if now.Sub(*v.LastSuccess) > maxAge {
		return false, "last successful poll is stale"
	}
	if v.PersistenceError != "" {
		return false, "state persistence unavailable"
	}
	return true, "ready"
}
