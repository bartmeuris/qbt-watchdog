package watchdog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"sync"
	"time"

	"qbt-watchdog/internal/arr"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

const queueFreshness = 2 * time.Minute
const queueInterval = 30 * time.Second

type ArrClient interface {
	Queue(context.Context) ([]arr.QueueItem, error)
	Map([]arr.QueueItem, string) (arr.Job, bool)
	RemoveJob(context.Context, arr.Job) error
	Search(context.Context, []int64) (arr.Command, error)
	Status(context.Context, int64) (arr.Command, error)
	History(context.Context, string) ([]arr.HistoryRecord, error)
	HasFile(context.Context, int64) (bool, error)
	CloseIdleConnections()
}

type ArrFactory func(config.ArrService) (ArrClient, error)

type integrationRuntime struct {
	config    config.ArrService
	client    ArrClient
	ctx       context.Context
	cancel    context.CancelFunc
	queue     []arr.QueueItem
	queueAt   time.Time
	nextQueue time.Time
	code      string
}

type IntegrationStatus struct {
	Kind    config.ArrKind `json:"kind"`
	Enabled bool           `json:"enabled"`
	Fresh   bool           `json:"queue_fresh"`
	QueueAt time.Time      `json:"queue_at"`
	Code    string         `json:"code,omitempty"`
}

type RecoveryStatus struct {
	Kind       config.ArrKind      `json:"kind"`
	Stage      store.RecoveryStage `json:"stage"`
	Mode       config.ArrMode      `json:"mode"`
	Code       string              `json:"code,omitempty"`
	CapturedAt time.Time           `json:"captured_at"`
	ExpiresAt  time.Time           `json:"expires_at"`
	NextAt     time.Time           `json:"next_at"`
	Attempts   int                 `json:"attempts"`
}

func (s *Service) initRecovery() {
	if s.arrFactory == nil {
		s.arrFactory = func(c config.ArrService) (ArrClient, error) { return arr.New(c, s.log) }
	}
	if s.state.RecoveryJobs == nil {
		s.state.RecoveryJobs = map[string]store.RecoveryJob{}
	}
	s.integrations = map[config.ArrKind]*integrationRuntime{}
	clients, err := s.prepareIntegrations(s.c)
	if err != nil {
		s.log.Error("integration initialization failed", "event", "arr_init_error")
	} else {
		s.adoptIntegrations(s.c, clients)
	}
	for id, job := range s.state.RecoveryJobs {
		switch job.Stage {
		case store.Prepared, store.BlocklistIntent, store.SearchIntent:
			job.Stage, job.LastCode = store.Uncertain, "restart_uncertain"
			s.state.RecoveryJobs[id] = job
		}
	}
}

func (s *Service) prepareIntegrations(c config.Config) (map[config.ArrKind]ArrClient, error) {
	clients := map[config.ArrKind]ArrClient{}
	for _, cfg := range c.Integrations.Active() {
		old := s.integrations[cfg.Kind]
		if old != nil && old.config.Equal(cfg) {
			clients[cfg.Kind] = old.client
			continue
		}
		client, err := s.arrFactory(cfg)
		if err != nil {
			for kind, created := range clients {
				if old := s.integrations[kind]; old == nil || old.client != created {
					created.CloseIdleConnections()
				}
			}
			return nil, err
		}
		clients[cfg.Kind] = client
	}
	return clients, nil
}

// Called only by the central state owner, after all factories succeed. Runtime
// identity is the generation fence: no result from a replaced runtime applies.
func (s *Service) adoptIntegrations(c config.Config, clients map[config.ArrKind]ArrClient) {
	for _, cfg := range c.Integrations.Services() {
		old := s.integrations[cfg.Kind]
		if old != nil && old.config.Equal(cfg) && s.c.SafetyKey() == c.SafetyKey() {
			continue
		}
		if old != nil {
			old.cancel()
			if old.client != nil && old.client != clients[cfg.Kind] {
				old.client.CloseIdleConnections()
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := &integrationRuntime{config: cfg.Clone(), client: clients[cfg.Kind], ctx: ctx, cancel: cancel}
		s.integrations[cfg.Kind] = r
		for id, job := range s.state.RecoveryJobs {
			if job.Kind != cfg.Kind {
				continue
			}
			if cfg.Enabled && job.Endpoint != cfg.EndpointKey() {
				s.finishRecovery(job, "endpoint_changed")
				continue
			}
			if job.Stage == store.BlocklistIntent || job.Stage == store.SearchIntent {
				job.Stage, job.LastCode = store.Uncertain, "ambiguous_timeout"
				s.state.RecoveryJobs[id] = job
			}
		}
	}
}

func (s *Service) recoveryEvent(kind config.ArrKind, code string) {
	e := store.Event{Time: s.clock.Now().UTC(), Action: "recovery", Integration: kind, Outcome: "failed", Error: code}
	if code == "search_completed" {
		e.Outcome = "success"
	}
	if code == "already_imported" || code == "replacement_queued" || code == "cancelled" || code == "own_search_pending" {
		e.Outcome = "skipped"
	}
	s.state.History = append(s.state.History, e)
	if len(s.state.History) > s.c.HistoryLimit {
		s.state.History = s.state.History[len(s.state.History)-s.c.HistoryLimit:]
	}
	s.log.Info("media recovery outcome", "event", "recovery", "kind", kind, "outcome", code)
}

func (s *Service) finishRecovery(job store.RecoveryJob, code string) {
	s.recoveryEvent(job.Kind, code)
	outcome := s.state.History[len(s.state.History)-1].Outcome
	s.metrics.RecoveryOutcomes.WithLabelValues(string(job.Kind), string(job.Stage), outcome).Inc()
	delete(s.state.RecoveryJobs, job.ID)
}

func (s *Service) expireRecovery(now time.Time) {
	for _, job := range s.state.RecoveryJobs {
		if !now.Before(job.ExpiresAt) {
			s.finishRecovery(job, "expired")
		}
	}
}

// Capture is pure with respect to the network. The caller persists it in the
// same write as the qBittorrent reservation, before the final GET/DELETE pair.
func (s *Service) prepareRecovery(t qbt.Torrent, episode store.Episode) {
	if s.c.DryRun || !s.c.EffectiveAction(episode.Policy).Destructive() {
		return
	}
	now := s.clock.Now().UTC()
	for _, cfg := range s.c.Integrations.Active() {
		if !cfg.Handles(t.Category) {
			continue
		}
		mode, ok := s.c.EffectiveArrMode(episode.Policy, cfg)
		if !ok {
			continue
		}
		duplicate := false
		for _, job := range s.state.RecoveryJobs {
			if job.Kind == cfg.Kind && job.Endpoint == cfg.EndpointKey() && job.Hash == t.Hash && job.Policy == episode.Policy {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if len(s.state.RecoveryJobs) >= store.MaxRecoveryJobs {
			s.recoveryEvent(cfg.Kind, "capacity")
			continue
		}
		identity := string(cfg.Kind) + cfg.EndpointKey() + t.Hash + string(episode.Policy) + episode.FirstSeen.UTC().Format(time.RFC3339Nano)
		digest := sha256.Sum256([]byte(identity))
		job := store.RecoveryJob{ID: hex.EncodeToString(digest[:]), Kind: cfg.Kind, Endpoint: cfg.EndpointKey(), Hash: t.Hash, Policy: episode.Policy, Action: s.c.EffectiveAction(episode.Policy), EpisodeAt: episode.FirstSeen, CapturedAt: now, ExpiresAt: now.Add(store.RecoveryTTL), Mode: mode, Stage: store.Prepared}
		if r := s.integrations[cfg.Kind]; r != nil && r.client != nil && !r.queueAt.IsZero() && now.Sub(r.queueAt) >= 0 && now.Sub(r.queueAt) <= queueFreshness {
			mapped, ok := r.client.Map(r.queue, t.Hash)
			if ok && !mapped.Truncated && !mapped.Incomplete {
				job.QueueIDs, job.MediaIDs = slices.Clone(mapped.ItemIDs), slices.Clone(mapped.MediaIDs)
			}
		}
		s.state.RecoveryJobs[job.ID] = job
	}
}

func (s *Service) releaseRecovery(hash string) {
	for id, job := range s.state.RecoveryJobs {
		if job.Hash == hash && job.Stage == store.Prepared {
			delete(s.state.RecoveryJobs, id)
		}
	}
}

func (s *Service) acceptedRecovery(hash string, now time.Time) {
	for id, job := range s.state.RecoveryJobs {
		if job.Hash != hash || (job.Stage != store.Prepared && job.Stage != store.AwaitDelete) {
			continue
		}
		job.Stage, job.AcceptedAt = store.AwaitDelete, now
		s.state.RecoveryJobs[id] = job
	}
}

// Only a successful full qBittorrent list can confirm deletion. Candidate GET
// negatives, failed lists and prepared reservations are not this evidence.
func (s *Service) confirmRecovery(torrents []qbt.Torrent, now time.Time) {
	present := map[string]bool{}
	for _, t := range torrents {
		present[t.Hash] = true
	}
	for id, job := range s.state.RecoveryJobs {
		if job.Stage != store.AwaitDelete || job.AcceptedAt.IsZero() {
			continue
		}
		if present[job.Hash] {
			continue
		}
		job.DeletedAt, job.Stage, job.Attempts = now, store.Resolving, 0
		s.state.RecoveryJobs[id] = job
	}
}

func (s *Service) recoverySnapshot(now time.Time) ([]IntegrationStatus, []RecoveryStatus) {
	s.metrics.RecoveryPending.Reset()
	apps := make([]IntegrationStatus, 0, 2)
	for _, cfg := range s.c.Integrations.Services() {
		v := IntegrationStatus{Kind: cfg.Kind, Enabled: cfg.Enabled}
		if r := s.integrations[cfg.Kind]; r != nil {
			v.QueueAt, v.Code = r.queueAt, r.code
			v.Fresh = !r.queueAt.IsZero() && now.Sub(r.queueAt) >= 0 && now.Sub(r.queueAt) <= queueFreshness
		}
		apps = append(apps, v)
		healthy := 0.0
		if v.Enabled && v.Fresh && v.Code == string(arr.Accepted) {
			healthy = 1
		}
		s.metrics.IntegrationHealthy.WithLabelValues(string(cfg.Kind)).Set(healthy)
	}
	jobs := make([]RecoveryStatus, 0, len(s.state.RecoveryJobs))
	for _, j := range s.state.RecoveryJobs {
		s.metrics.RecoveryPending.WithLabelValues(string(j.Kind), string(j.Stage)).Inc()
		jobs = append(jobs, RecoveryStatus{Kind: j.Kind, Stage: j.Stage, Mode: j.Mode, Code: j.LastCode, CapturedAt: j.CapturedAt, ExpiresAt: j.ExpiresAt, NextAt: j.NextAt, Attempts: j.Attempts})
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Kind != jobs[j].Kind {
			return jobs[i].Kind < jobs[j].Kind
		}
		return jobs[i].CapturedAt.Before(jobs[j].CapturedAt)
	})
	return apps, jobs
}

func (s *Service) startRecovery(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	for _, kind := range []config.ArrKind{config.Sonarr, config.Radarr} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for ctx.Err() == nil {
				s.recoveryCycle(ctx, kind)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	return func() {
		cancel()
		workers.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, r := range s.integrations {
			if r.client != nil {
				r.client.CloseIdleConnections()
			}
		}
	}
}

// Two independent, bounded app workers use the same central writer as Poll
// and reload. All HTTP runs after releasing mu; claims carry runtime identity
// and a cancellation fence, never pointers into mutable job state.
func (s *Service) recoveryCycle(parent context.Context, kind config.ArrKind) {
	index := 0
	if kind == config.Radarr {
		index = 1
	} else if kind != config.Sonarr {
		return
	}
	if !s.recoveryPolling[index].CompareAndSwap(false, true) {
		return
	}
	defer s.recoveryPolling[index].Store(false)
	s.mu.Lock()
	r := s.integrations[kind]
	if (r == nil || r.client == nil || !r.config.Enabled) && len(s.state.RecoveryJobs) == 0 {
		s.mu.Unlock()
		return
	}
	now := s.clock.Now().UTC()
	s.expireRecovery(now)
	if r == nil || r.client == nil || !r.config.Enabled {
		s.persist()
		s.publish(nil)
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.config.Timeout)
	stop := context.AfterFunc(parent, cancel)
	if parent.Err() != nil {
		cancel()
	}
	defer func() { stop(); cancel() }()
	refresh := !now.Before(r.nextQueue)
	if refresh {
		r.nextQueue = now.Add(queueInterval)
	}
	s.mu.Unlock()
	if refresh {
		items, err := r.client.Queue(ctx)
		s.mu.Lock()
		if s.integrations[kind] == r {
			r.code = string(arr.OutcomeOf(err))
			if err == nil {
				r.queue, r.queueAt = items, s.clock.Now().UTC()
			}
		}
		s.publish(nil)
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.integrations[kind] != r || s.c.DryRun || ctx.Err() != nil {
		s.persist()
		s.publish(nil)
		s.mu.Unlock()
		return
	}
	var claimed *store.RecoveryJob
	for _, j := range s.state.RecoveryJobs {
		if j.Kind != kind || j.Endpoint != r.config.EndpointKey() || now.Before(j.NextAt) || !s.c.EffectiveAction(j.Policy).Destructive() || s.c.MaxDeletions == 0 {
			continue
		}
		switch j.Stage {
		case store.Resolving, store.BlocklistPending, store.SearchPending, store.CommandPending:
			if claimed == nil || j.CapturedAt.Before(claimed.CapturedAt) || (j.CapturedAt.Equal(claimed.CapturedAt) && j.ID < claimed.ID) {
				copy := j.Clone()
				claimed = &copy
			}
		}
	}
	s.persist()
	s.publish(nil)
	blocked := s.writeBlocked || s.view.PersistenceError != ""
	s.mu.Unlock()
	if claimed == nil || blocked {
		return
	}
	s.advanceRecovery(ctx, r, *claimed)
}

func (s *Service) claimCurrent(ctx context.Context, r *integrationRuntime, j store.RecoveryJob) bool {
	if _, ok := s.c.EffectiveArrMode(j.Policy, r.config); !ok {
		return false
	}
	current, ok := s.state.RecoveryJobs[j.ID]
	return ok && current.Stage == j.Stage && s.integrations[j.Kind] == r && !s.c.DryRun && r.config.Enabled &&
		s.c.EffectiveAction(j.Policy).Destructive() && s.c.MaxDeletions > 0 &&
		ctx.Err() == nil && r.ctx.Err() == nil && s.clock.Now().Before(j.ExpiresAt) && !s.writeBlocked
}

func (s *Service) applyRecovery(ctx context.Context, r *integrationRuntime, old, next store.RecoveryJob, terminal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimCurrent(ctx, r, old) {
		return
	}
	if terminal != "" {
		s.finishRecovery(old, terminal)
	} else {
		s.state.RecoveryJobs[next.ID] = next
	}
	s.persist()
	s.publish(nil)
}

func recoveryBackoff(j store.RecoveryJob) time.Duration {
	// Stable jitter makes restart scheduling reproducible and tests clock-only.
	digest := sha256.Sum256([]byte(j.ID))
	return (time.Second * time.Duration(1<<min(j.Attempts, 8))) + time.Duration(digest[0])*time.Millisecond
}

func (s *Service) retryRecovery(ctx context.Context, r *integrationRuntime, j store.RecoveryJob, code string) {
	next := j.Clone()
	next.Attempts++
	next.LastCode = code
	if next.Attempts >= store.MaxRecoveryAttempts {
		s.applyRecovery(ctx, r, j, next, code)
		return
	}
	s.mu.Lock()
	now := s.clock.Now().UTC()
	s.mu.Unlock()
	next.NextAt = now.Add(recoveryBackoff(next))
	s.applyRecovery(ctx, r, j, next, "")
}

func (s *Service) advanceRecovery(ctx context.Context, r *integrationRuntime, job store.RecoveryJob) {
	if job.Stage == store.CommandPending {
		command, err := r.client.Status(ctx, job.CommandID)
		if err != nil {
			s.retryRecovery(context.WithoutCancel(ctx), r, job, string(arr.OutcomeOf(err)))
			return
		}
		if command.Finished() {
			code := "command_failed"
			if command.Succeeded() {
				code = "search_completed"
			}
			s.applyRecovery(ctx, r, job, job, code)
			return
		}
		next := job.Clone()
		s.mu.Lock()
		next.NextAt = s.clock.Now().Add(queueInterval)
		s.mu.Unlock()
		s.applyRecovery(ctx, r, job, next, "")
		return
	}
	items, err := r.client.Queue(ctx)
	if err != nil {
		s.retryRecovery(context.WithoutCancel(ctx), r, job, string(arr.OutcomeOf(err)))
		return
	}
	mapped, found := r.client.Map(items, job.Hash)
	if job.Stage == store.Resolving {
		next := job.Clone()
		if found {
			if mapped.Incomplete || mapped.Truncated {
				s.applyRecovery(ctx, r, job, job, "identity_incomplete")
				return
			}
			if (len(job.MediaIDs) > 0 && !slices.Equal(mapped.MediaIDs, job.MediaIDs)) || (len(job.QueueIDs) > 0 && !slices.Equal(mapped.ItemIDs, job.QueueIDs)) {
				s.applyRecovery(ctx, r, job, job, "identity_incomplete")
				return
			}
			next.QueueIDs, next.MediaIDs = slices.Clone(mapped.ItemIDs), slices.Clone(mapped.MediaIDs)
		}
		if job.Mode.Blocklists() {
			if !found {
				s.applyRecovery(ctx, r, job, job, "queue_vanished")
				return
			}
			next.Stage = store.BlocklistPending
		} else {
			if len(next.MediaIDs) == 0 {
				history, err := r.client.History(ctx, job.Hash)
				if err != nil {
					s.retryRecovery(context.WithoutCancel(ctx), r, job, string(arr.OutcomeOf(err)))
					return
				}
				for _, record := range history {
					if arr.NormalizeDownloadID(record.DownloadID) != job.Hash || record.MediaID == nil || *record.MediaID <= 0 || record.ID <= 0 {
						continue
					}
					next.HistoryIDs = append(next.HistoryIDs, record.ID)
					next.MediaIDs = append(next.MediaIDs, *record.MediaID)
				}
				slices.Sort(next.HistoryIDs)
				next.HistoryIDs = slices.Compact(next.HistoryIDs)
				slices.Sort(next.MediaIDs)
				next.MediaIDs = slices.Compact(next.MediaIDs)
				if len(next.MediaIDs) > store.MaxRecoveryIDs || len(next.HistoryIDs) > store.MaxRecoveryIDs {
					s.applyRecovery(ctx, r, job, job, "identity_incomplete")
					return
				}
				if len(next.MediaIDs) == 0 {
					s.retryRecovery(ctx, r, job, "identity_missing")
					return
				}
			}
			next.Stage = store.SearchPending
		}
		next.Attempts, next.LastCode = 0, ""
		s.applyRecovery(ctx, r, job, next, "")
		return
	}
	if job.Stage == store.BlocklistPending {
		if !found {
			s.applyRecovery(ctx, r, job, job, "queue_vanished")
			return
		}
		if mapped.Incomplete || mapped.Truncated || !slices.Equal(mapped.MediaIDs, job.MediaIDs) || !slices.Equal(mapped.ItemIDs, job.QueueIDs) {
			s.applyRecovery(ctx, r, job, job, "identity_incomplete")
			return
		}
		for _, id := range job.MediaIDs {
			hasFile, err := r.client.HasFile(ctx, id)
			if err != nil {
				s.retryRecovery(context.WithoutCancel(ctx), r, job, "safety_unavailable")
				return
			}
			if hasFile {
				s.applyRecovery(ctx, r, job, job, "already_imported")
				return
			}
		}
		if !s.mutationIntent(ctx, r, &job, store.BlocklistIntent) {
			return
		}
		err := r.client.RemoveJob(ctx, mapped)
		s.mutationResult(r, job, arr.Command{}, err)
		return
	}
	if len(job.MediaIDs) == 0 {
		s.applyRecovery(ctx, r, job, job, "identity_missing")
		return
	}
	for _, row := range items {
		if row.DownloadID != job.Hash && row.MediaID != nil && slices.Contains(job.MediaIDs, *row.MediaID) {
			s.applyRecovery(ctx, r, job, job, "replacement_queued")
			return
		}
	}
	for _, id := range job.MediaIDs {
		hasFile, err := r.client.HasFile(ctx, id)
		if err != nil {
			s.retryRecovery(context.WithoutCancel(ctx), r, job, "safety_unavailable")
			return
		}
		if hasFile {
			s.applyRecovery(ctx, r, job, job, "already_imported")
			return
		}
	}
	if !s.mutationIntent(ctx, r, &job, store.SearchIntent) {
		return
	}
	command, err := r.client.Search(ctx, job.MediaIDs)
	s.mutationResult(r, job, command, err)
}

func overlap(a, b []int64) bool {
	for _, id := range a {
		if slices.Contains(b, id) {
			return true
		}
	}
	return false
}

func (s *Service) mutationIntent(ctx context.Context, r *integrationRuntime, job *store.RecoveryJob, stage store.RecoveryStage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimCurrent(ctx, r, *job) {
		return false
	}
	if stage == store.SearchIntent {
		for _, other := range s.state.RecoveryJobs {
			if other.ID != job.ID && other.Kind == job.Kind && other.Endpoint == job.Endpoint &&
				(other.Stage == store.SearchIntent || other.Stage == store.CommandPending || other.Stage == store.Uncertain) && overlap(other.MediaIDs, job.MediaIDs) {
				s.finishRecovery(*job, "own_search_pending")
				s.persist()
				s.publish(nil)
				return false
			}
		}
	}
	job.Stage, job.LastCode = stage, ""
	s.state.RecoveryJobs[job.ID] = job.Clone()
	s.persist()
	s.publish(nil)
	return s.view.PersistenceError == "" && ctx.Err() == nil && r.ctx.Err() == nil
}

func (s *Service) mutationResult(r *integrationRuntime, job store.RecoveryJob, command arr.Command, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.state.RecoveryJobs[job.ID]
	if !exists || current.Stage != job.Stage || s.integrations[job.Kind] != r {
		return
	}
	outcome := arr.OutcomeOf(err)
	job.LastCode = string(outcome)
	switch outcome {
	case arr.Accepted:
		if job.Stage == store.BlocklistIntent {
			job.Stage, job.Attempts = store.SearchPending, 0
		} else if command.ID > 0 {
			job.Stage, job.CommandID, job.Attempts = store.CommandPending, command.ID, 0
		} else {
			job.Stage, job.LastCode = store.Uncertain, "ambiguous_timeout"
		}
	case arr.NotFound:
		s.finishRecovery(job, "not_found")
		s.persist()
		s.publish(nil)
		return
	case arr.Rejected:
		job.Attempts++
		if job.Attempts >= store.MaxRecoveryAttempts {
			s.finishRecovery(job, "rejected")
			s.persist()
			s.publish(nil)
			return
		}
		if job.Stage == store.BlocklistIntent {
			job.Stage = store.BlocklistPending
		} else {
			job.Stage = store.SearchPending
		}
		job.NextAt = s.clock.Now().Add(recoveryBackoff(job))
	default:
		job.Stage = store.Uncertain
	}
	s.state.RecoveryJobs[job.ID] = job
	s.persist()
	s.publish(nil)
}
