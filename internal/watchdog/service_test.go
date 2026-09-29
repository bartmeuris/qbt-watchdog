package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const hashC = "cccccccccccccccccccccccccccccccccccccccc"

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type fakeClient struct {
	torrents                                       []qbt.Torrent
	listError, versionError, getError, deleteError error
	tagError                                       error
	fresh                                          func(string) *qbt.Torrent
	deletes, gets                                  []string
	files                                          []bool
	adds, removes                                  []string
	versionCalls, listCalls                        int
	block                                          func(context.Context)
}

func (f *fakeClient) Versions(context.Context) (string, string, error) {
	f.versionCalls++
	return "5.0.1", "2.11.2", f.versionError
}
func (f *fakeClient) List(ctx context.Context) ([]qbt.Torrent, error) {
	f.listCalls++
	if f.block != nil {
		f.block(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return slices.Clone(f.torrents), f.listError
}
func (f *fakeClient) Get(_ context.Context, hash string) (*qbt.Torrent, error) {
	f.gets = append(f.gets, hash)
	if f.getError != nil {
		return nil, f.getError
	}
	if f.fresh != nil {
		return f.fresh(hash), nil
	}
	for _, t := range f.torrents {
		if t.Hash == hash {
			return &t, nil
		}
	}
	return nil, nil
}
func (f *fakeClient) Delete(_ context.Context, hash string, files bool) error {
	f.deletes = append(f.deletes, hash)
	f.files = append(f.files, files)
	return f.deleteError
}
func (f *fakeClient) AddTags(_ context.Context, hashes []string, tag string) error {
	f.adds = append(f.adds, tag+":"+strings.Join(hashes, "|"))
	return f.tagError
}
func (f *fakeClient) RemoveTags(_ context.Context, hashes []string, tag string) error {
	f.removes = append(f.removes, tag+":"+strings.Join(hashes, "|"))
	return f.tagError
}

type memoryStore struct {
	state store.State
	err   error
	saves int
}

func (m *memoryStore) Load(time.Time) (store.State, error) {
	if m.state.Tracked == nil {
		return store.Empty(), nil
	}
	data, _ := json.Marshal(m.state)
	var state store.State
	_ = json.Unmarshal(data, &state)
	return state, nil
}
func (m *memoryStore) Save(s store.State) error {
	m.saves++
	if m.err != nil {
		return m.err
	}
	data, _ := json.Marshal(s)
	var saved store.State
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	m.state = saved
	return nil
}
func torrent(hash string) qbt.Torrent {
	return qbt.Torrent{Hash: hash, Name: "test " + qbt.ShortHash(hash), State: "metaDL", AddedOn: 100}
}
func fixture(t *testing.T) (*Service, *fakeClient, *fakeClock, *memoryStore) {
	t.Helper()
	c, err := config.Decode(config.YAML, []byte("qbt_url: 'http://localhost'\npoll_interval: '10s'\nmax_observation_gap: '30s'\ndelete_confirmation_timeout: '20s'"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range config.PolicyIDs() {
		policy := c.Policies[id]
		policy.Action = config.Delete
		policy.Threshold = 20 * time.Second
		c.Policies[id] = policy
	}
	clock := &fakeClock{time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}
	client := &fakeClient{torrents: []qbt.Torrent{torrent(hashA)}}
	disk := &memoryStore{}
	build := observability.NewBuild("test", "test", "test")
	service := New(c, client, disk, clock, slog.New(slog.NewTextHandler(io.Discard, nil)), observability.New(build), build)
	return service, client, clock, disk
}
func poll(t *testing.T, s *Service) {
	t.Helper()
	if e := s.Poll(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestTrackingContinuityTimeoutAndDryRunDedup(t *testing.T) {
	s, c, clock, disk := fixture(t)
	poll(t, s)
	first := s.state.Tracked[hashA].FirstSeen
	if !first.Equal(clock.Now()) {
		t.Fatal("new tracking clock")
	}
	clock.Advance(10 * time.Second)
	poll(t, s)
	if !s.state.Tracked[hashA].FirstSeen.Equal(first) || len(s.state.History) != 0 {
		t.Fatal("early action or reset")
	}
	clock.Advance(10 * time.Second)
	poll(t, s)
	clock.Advance(10 * time.Second)
	poll(t, s)
	if len(c.deletes) != 0 || len(s.state.History) != 1 || s.state.History[0].Action != "warn" || s.state.Counters.WouldDeletions != 1 || !disk.state.Tracked[hashA].DryRunNotified {
		t.Fatal("dry run not deduplicated")
	}
	if s.Snapshot().Torrents[0].Decision != "warned" || s.Snapshot().Torrents[0].Elapsed != 30 {
		t.Fatal(s.Snapshot())
	}
	if testutil.ToFloat64(s.metrics.Actions.WithLabelValues("warn", "success", "true")) != 1 {
		t.Fatal("action metric")
	}
}
func TestExactStateExitReentryAndDisappearance(t *testing.T) {
	for _, state := range []string{"queuedDL", "pausedDL", "downloading", "unknown", "forcedMetaDL", ""} {
		t.Run(state, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			poll(t, s)
			clock.Advance(10 * time.Second)
			c.torrents[0].State = state
			poll(t, s)
			if len(s.state.Tracked) != 0 {
				t.Fatal("tracked other state")
			}
			clock.Advance(10 * time.Second)
			c.torrents[0].State = "metaDL"
			poll(t, s)
			if !s.state.Tracked[hashA].FirstSeen.Equal(clock.Now()) {
				t.Fatal("episode not reset")
			}
			c.torrents = nil
			poll(t, s)
			if len(s.state.Tracked) != 0 {
				t.Fatal("missing torrent retained")
			}
		})
	}
}
func TestObservationGapAndOutageNeverCountAsContinuity(t *testing.T) {
	s, c, clock, _ := fixture(t)
	poll(t, s)
	first := s.state.Tracked[hashA].FirstSeen
	c.listError = errors.New("API unavailable")
	clock.Advance(time.Minute)
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	if !s.state.Tracked[hashA].FirstSeen.Equal(first) || len(c.deletes) > 0 {
		t.Fatal("failure reset or deleted")
	}
	if s.Snapshot().QBTUp || s.Snapshot().PollError == "" {
		t.Fatal("failure not published")
	}
	c.listError = nil
	poll(t, s)
	if !s.state.Tracked[hashA].FirstSeen.Equal(clock.Now()) || s.state.Counters.WouldDeletions != 0 {
		t.Fatal("gap counted")
	}
}
func TestActiveFreshConfirmationDefaultDeleteFilesAndPending(t *testing.T) {
	s, c, clock, disk := fixture(t)
	s.c.DryRun = false
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	if len(c.gets) < 1 || len(c.deletes) != 1 || c.files[0] || disk.state.Tracked[hashA].DeleteRequestedAt == nil || s.state.Counters.Deletions != 0 || s.state.Counters.DeleteRequests != 1 {
		t.Fatal("delete not safely requested")
	}
	clock.Advance(10 * time.Second)
	poll(t, s)
	if len(c.deletes) != 1 || s.Snapshot().Summary.DeleteRequested != 1 {
		t.Fatal("pending request repeated")
	}
	c.torrents = nil
	clock.Advance(10 * time.Second)
	poll(t, s)
	if s.state.Counters.Deletions != 1 || len(s.state.Tracked) != 0 || s.state.History[len(s.state.History)-1].Action != "action_confirmed" {
		t.Fatal("not confirmed")
	}
}
func TestFreshConfirmationRechecksEveryGuard(t *testing.T) {
	cases := map[string]func(*qbt.Torrent){"state": func(t *qbt.Torrent) { t.State = "downloading" }, "progress": func(t *qbt.Torrent) { t.Progress = .01 }, "downloaded": func(t *qbt.Torrent) { t.Downloaded = 1 }, "category exclude": func(t *qbt.Torrent) { t.Category = "excluded" }, "category include": func(t *qbt.Torrent) { t.Category = "other" }, "tag": func(t *qbt.Torrent) { t.Tags = " keep " }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.DryRun = false
			s.c.IncludeCategories = []string{"allowed"}
			s.c.ExcludeCategories = []string{"excluded"}
			c.torrents[0].Category = "allowed"
			poll(t, s)
			clock.Advance(20 * time.Second)
			c.fresh = func(string) *qbt.Torrent { fresh := c.torrents[0]; change(&fresh); return &fresh }
			poll(t, s)
			if len(c.deletes) != 0 {
				t.Fatal("guard bypassed")
			}
			if name == "state" && len(s.state.Tracked) != 0 {
				t.Fatal("state not reset")
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		s.c.DryRun = false
		poll(t, s)
		clock.Advance(20 * time.Second)
		c.fresh = func(string) *qbt.Torrent { return nil }
		poll(t, s)
		if len(c.deletes) > 0 || len(s.state.Tracked) > 0 {
			t.Fatal("deleted missing torrent")
		}
	})
	t.Run("late state change", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		s.c.DryRun = false
		poll(t, s)
		clock.Advance(20 * time.Second)
		calls := 0
		c.fresh = func(string) *qbt.Torrent {
			calls++
			fresh := c.torrents[0]
			if calls == 2 {
				fresh.State = "downloading"
			}
			return &fresh
		}
		poll(t, s)
		if len(c.deletes) > 0 {
			t.Fatal("immediate recheck missing")
		}
	})
}
func TestSafetyAndExclusionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		category, tags   string
		progress         float64
		downloaded       int64
		include, exclude []string
		eligible         bool
	}{{name: "zero", eligible: true}, {name: "progress", progress: .01}, {name: "bytes", downloaded: 1}, {name: "include", category: "yes", include: []string{"yes"}, eligible: true}, {name: "outside include", category: "no", include: []string{"yes"}}, {name: "exclude wins", category: "yes", include: []string{"yes"}, exclude: []string{"yes"}}, {name: "tag exact", tags: " keeper ", eligible: true}, {name: "tag trimmed", tags: "misc, keep "}, {name: "reserved tag ignored", tags: "qbtw-keep", eligible: true}, {name: "tag case sensitive", tags: "Keep", eligible: true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.IncludeCategories = tc.include
			s.c.ExcludeCategories = tc.exclude
			c.torrents[0].Category = tc.category
			c.torrents[0].Tags = tc.tags
			c.torrents[0].Progress = tc.progress
			c.torrents[0].Downloaded = tc.downloaded
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
			if (s.state.Counters.WouldDeletions == 1) != tc.eligible {
				t.Fatal("wrong decision", s.Snapshot().Torrents[0].Decision)
			}
		})
	}
	t.Run("partial policy allows payload", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		c.torrents[0].State = "stalledDL"
		c.torrents[0].Downloaded = 1
		c.torrents[0].Progress = .1
		poll(t, s)
		clock.Advance(20 * time.Second)
		poll(t, s)
		if s.state.Counters.WouldDeletions != 1 {
			t.Fatal("disabled guards applied")
		}
	})
}
func TestDeterministicOldestFirstCapAndZero(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	s.c.MaxDeletions = 1
	c.torrents = []qbt.Torrent{torrent(hashC)}
	poll(t, s)
	clock.Advance(10 * time.Second)
	c.torrents = []qbt.Torrent{torrent(hashB), torrent(hashC), torrent(hashA)}
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	if !slices.Equal(c.deletes, []string{hashC}) {
		t.Fatal(c.deletes)
	}
	clock.Advance(time.Second)
	poll(t, s)
	if !slices.Equal(c.deletes, []string{hashC, hashA}) {
		t.Fatal("tie ordering", c.deletes)
	}
	for _, dry := range []bool{true, false} {
		s, c, clock, _ := fixture(t)
		s.c.DryRun = dry
		s.c.MaxDeletions = 0
		poll(t, s)
		clock.Advance(20 * time.Second)
		poll(t, s)
		if len(c.deletes) > 0 || len(c.gets) > 0 || len(s.state.History) > 0 {
			t.Fatal("zero cap performed action")
		}
	}
}
func TestReadFailuresPreventActionsAndPreserveState(t *testing.T) {
	for _, phase := range []string{"version", "list", "confirmation"} {
		t.Run(phase, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.DryRun = false
			poll(t, s)
			before := s.state.Tracked[hashA]
			clock.Advance(20 * time.Second)
			err := errors.New("API unavailable")
			switch phase {
			case "version":
				c.versionError = err
			case "list":
				c.listError = err
			case "confirmation":
				c.getError = err
			}
			if s.Poll(context.Background()) == nil {
				t.Fatal("expected failure")
			}
			if len(c.deletes) != 0 || s.state.Tracked[hashA] != before {
				t.Fatal("read failure mutated state")
			}
			if testutil.ToFloat64(s.metrics.PollErrors) != 1 {
				t.Fatal("missing error metric")
			}
		})
	}
}
func TestFiniteRetriesAndRestartPersistence(t *testing.T) {
	s, c, clock, disk := fixture(t)
	s.c.DryRun = false
	poll(t, s)
	for range 5 {
		clock.Advance(20 * time.Second)
		poll(t, s)
	}
	if len(c.deletes) != store.MaxAttempts || s.state.Tracked[hashA].Attempts != 3 || s.Snapshot().Torrents[0].Decision != "retry limit reached" {
		t.Fatal("unbounded retries", len(c.deletes))
	}
	build := s.view.Build
	restarted := New(s.c, c, disk, clock, s.log, observability.New(build), build)
	clock.Advance(10 * time.Second)
	poll(t, restarted)
	if len(c.deletes) != 3 || restarted.Snapshot().SinceStartup.DeleteRequests != 0 || restarted.Snapshot().Lifetime.DeleteRequests != 3 {
		t.Fatal("retry budget/counters lost on restart")
	}
	c.torrents[0].State = "downloading"
	poll(t, restarted)
	c.torrents[0].State = "metaDL"
	poll(t, restarted)
	clock.Advance(20 * time.Second)
	poll(t, restarted)
	if len(c.deletes) != 4 {
		t.Fatal("new episode did not clear retry budget")
	}
}
func TestShortRestartDryRunDedupAndPending(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry", false: "active"}[dry], func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.DryRun = dry
			disk := store.File{Path: filepath.Join(t.TempDir(), "state.json"), HistoryLimit: 100}
			s.disk = disk
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
			first := s.state.Tracked[hashA].FirstSeen
			clock.Advance(5 * time.Second)
			build := s.view.Build
			restarted := New(s.c, c, disk, clock, s.log, observability.New(build), build)
			poll(t, restarted)
			if !restarted.state.Tracked[hashA].FirstSeen.Equal(first) || restarted.startup != (store.Counters{}) || len(restarted.state.History) != 1 {
				t.Fatal("restart lost episode state")
			}
			if dry && len(c.deletes) > 0 {
				t.Fatal("dry-run deleted")
			}
			if !dry && len(c.deletes) != 1 {
				t.Fatal("pending lost")
			}
		})
	}
}
func TestPersistenceReadinessAndRecovery(t *testing.T) {
	s, c, clock, disk := fixture(t)
	if ok, _ := Ready(s.Snapshot(), clock.Now(), time.Minute); ok {
		t.Fatal("ready before poll")
	}
	poll(t, s)
	if ok, _ := Ready(s.Snapshot(), clock.Now(), time.Minute); !ok {
		t.Fatal("not ready")
	}
	disk.err = errors.New("state unavailable")
	clock.Advance(10 * time.Second)
	poll(t, s)
	if ok, _ := Ready(s.Snapshot(), clock.Now(), time.Minute); ok {
		t.Fatal("ready with broken persistence")
	}
	if testutil.ToFloat64(s.metrics.StateErrors) != 1 {
		t.Fatal("state metric")
	}
	s.c.DryRun = false
	clock.Advance(10 * time.Second)
	poll(t, s)
	if len(c.deletes) > 0 {
		t.Fatal("delete without durable intent")
	}
	disk.err = nil
	poll(t, s)
	if ok, _ := Ready(s.Snapshot(), clock.Now(), time.Minute); !ok {
		t.Fatal("not recovered")
	}
	if ok, _ := Ready(s.Snapshot(), clock.Now().Add(2*time.Minute), time.Minute); ok {
		t.Fatal("stale ready")
	}
}
func TestImmutableSnapshots(t *testing.T) {
	s, _, clock, _ := fixture(t)
	poll(t, s)
	v := s.Snapshot()
	v.Torrents[0].Name = "mutated"
	*v.LastSuccess = clock.Now().Add(time.Hour)
	*v.Torrents[0].FirstSeen = clock.Now().Add(time.Hour)
	again := s.Snapshot()
	if again.Torrents[0].Name == "mutated" || !again.LastSuccess.Equal(clock.Now()) || !again.Torrents[0].FirstSeen.Equal(clock.Now()) {
		t.Fatal("mutable snapshot")
	}
	data, _ := json.Marshal(again)
	if strings.Contains(string(data), hashA) || strings.Contains(string(data), "http://localhost") {
		t.Fatal("sensitive data exposed")
	}
}
func TestCancellationNoOverlapAndConcurrentSnapshots(t *testing.T) {
	s, c, _, _ := fixture(t)
	started := make(chan struct{})
	var active, maxActive atomic.Int32
	c.block = func(ctx context.Context) {
		n := active.Add(1)
		maxActive.Store(max(maxActive.Load(), n))
		close(started)
		<-ctx.Done()
		active.Add(-1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first poll not immediate")
	}
	if e := s.Poll(context.Background()); e == nil {
		t.Fatal("overlapping poll accepted")
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation leaked goroutine")
	}
	if active.Load() != 0 || maxActive.Load() != 1 {
		t.Fatal("overlap")
	}
}
func TestFailureBudgetFinite(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	c.deleteError = errors.New("delete unavailable")
	poll(t, s)
	for range 5 {
		clock.Advance(20 * time.Second)
		_ = s.Poll(context.Background())
	}
	if len(c.deletes) != 3 || s.state.Counters.DeleteRequests != 0 {
		t.Fatal("failed requests not bounded")
	}
}

func TestLongRestartResetsClockButPreservesPendingBudget(t *testing.T) {
	s, c, clock, disk := fixture(t)
	s.c.DryRun = false
	s.c.DeleteConfirmationTimeout = 5 * time.Minute
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	clock.Advance(time.Minute)
	build := s.view.Build
	restarted := New(s.c, c, disk, clock, s.log, observability.New(build), build)
	poll(t, restarted)
	e := restarted.state.Tracked[hashA]
	if !e.FirstSeen.Equal(clock.Now()) || e.Attempts != 1 || e.DeleteRequestedAt == nil || len(c.deletes) != 1 {
		t.Fatal("long restart lost safety state", e)
	}
}

func TestBatchPreflightFailurePreventsAllDeletes(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	c.torrents = []qbt.Torrent{torrent(hashA), torrent(hashB)}
	poll(t, s)
	clock.Advance(20 * time.Second)
	c.fresh = func(hash string) *qbt.Torrent {
		c.getError = errors.New("next confirmation fails")
		t := torrent(hash)
		return &t
	}
	if s.Poll(context.Background()) == nil || len(c.deletes) != 0 {
		t.Fatal("partial preflight triggered deletion")
	}
	if s.state.Tracked[hashA].LastSeen.Equal(clock.Now()) {
		t.Fatal("failed preflight updated observation")
	}
}

func TestMissingPolicyDoesNotAct(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	delete(s.c.Policies, config.Metadata)
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	if len(c.deletes) != 0 || len(s.state.History) != 0 || s.Snapshot().Torrents[0].Decision != DecisionNotApplicable {
		t.Fatal("missing policy acted", c.deletes, s.state.History, s.Snapshot().Torrents[0].Decision)
	}
}

func TestTagSyncDryRunIdempotentAndBenignRaces(t *testing.T) {
	t.Run("dry-run suppresses writes", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		s.c.TagSync.Enabled = true
		p := s.c.Policies[config.Metadata]
		p.Action = config.Warn
		s.c.Policies[config.Metadata] = p
		poll(t, s)
		clock.Advance(20 * time.Second)
		poll(t, s)
		if len(c.adds) != 0 || len(c.removes) != 0 || s.state.WatchdogTagPrefix != "" {
			t.Fatal("dry-run wrote tags", c.adds, c.removes, s.state.WatchdogTagPrefix)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		s.c.DryRun = false
		s.c.MaxDeletions = 0
		s.c.TagSync.Enabled = true
		c.torrents[0].Tags = "qbtw-metadata"
		poll(t, s)
		clock.Advance(20 * time.Second)
		c.torrents[0].Tags = "qbtw-metadata, qbtw-due"
		poll(t, s)
		if len(c.adds) != 0 || len(c.removes) != 0 || s.Snapshot().PollError != "" {
			t.Fatal("idempotent sync wrote tags", c.adds, c.removes, s.Snapshot().PollError)
		}
		c.torrents[0].Tags = ""
		poll(t, s)
		if len(c.adds) == 0 {
			t.Fatal("missing desired tags were not written")
		}
	})
	t.Run("delete race is benign", func(t *testing.T) {
		s, c, _, _ := fixture(t)
		s.c.DryRun = false
		s.c.MaxDeletions = 0
		s.c.TagSync.Enabled = true
		c.tagError = errors.New("qBittorrent API returned HTTP 404")
		poll(t, s)
		if s.Snapshot().PollError != "" {
			t.Fatal("benign tag race set poll error", s.Snapshot().PollError)
		}
	})
}

func TestTagSyncPrefixMismatchAndRemediation(t *testing.T) {
	t.Run("reload rejection retains old config", func(t *testing.T) {
		s, c, _, _ := fixture(t)
		s.state.WatchdogTagPrefix = "old-"
		next := s.c.Clone()
		next.TagSync.Enabled = true
		next.TagSync.Prefix = "new-"
		if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err == nil {
			t.Fatal("mismatched enabled prefix accepted")
		}
		if s.Config().TagSync.Prefix == "new-" {
			t.Fatal("rejected prefix became active")
		}
	})
	t.Run("poll refuses mismatched enabled prefix before qbt calls", func(t *testing.T) {
		s, c, _, _ := fixture(t)
		s.c.TagSync.Enabled = true
		s.c.TagSync.Prefix = "new-"
		s.state.WatchdogTagPrefix = "old-"
		if err := s.Poll(context.Background()); err == nil {
			t.Fatal("mismatched enabled prefix poll succeeded")
		}
		if c.versionCalls != 0 || c.listCalls != 0 || len(c.gets) != 0 || len(c.deletes) != 0 || len(c.adds) != 0 || len(c.removes) != 0 {
			t.Fatal("qBittorrent API called before prefix refusal", c.versionCalls, c.listCalls, c.gets, c.deletes, c.adds, c.removes)
		}
	})
	t.Run("failed claim save prevents tag writes and reverts in memory", func(t *testing.T) {
		s, c, _, disk := fixture(t)
		s.c.DryRun = false
		s.c.MaxDeletions = 0
		s.c.TagSync.Enabled = true
		disk.err = errors.New("save failed")
		poll(t, s)
		if len(c.adds) != 0 || len(c.removes) != 0 || s.state.WatchdogTagPrefix != "" {
			t.Fatal("failed claim wrote tags or retained prefix", c.adds, c.removes, s.state.WatchdogTagPrefix)
		}
	})
	t.Run("disabled sweep uses persisted prefix", func(t *testing.T) {
		s, c, _, _ := fixture(t)
		s.c.DryRun = false
		s.c.TagSync.Enabled = false
		s.c.TagSync.Prefix = "new-"
		s.state.WatchdogTagPrefix = "old-"
		c.torrents[0].Tags = "old-metadata"
		poll(t, s)
		if len(c.removes) != 1 || !strings.HasPrefix(c.removes[0], "old-metadata:") || s.state.WatchdogTagPrefix != "" {
			t.Fatal("old prefix not swept", c.removes, s.state.WatchdogTagPrefix)
		}
	})
	t.Run("partial sweep keeps claim", func(t *testing.T) {
		s, c, _, _ := fixture(t)
		s.c.DryRun = false
		s.c.TagSync.Enabled = false
		s.c.TagSync.MaxWritesPerPoll = 1
		s.state.WatchdogTagPrefix = "old-"
		c.torrents[0].Tags = "old-a, old-b"
		poll(t, s)
		if len(c.removes) != 1 || s.state.WatchdogTagPrefix != "old-" {
			t.Fatal("partial sweep lost old prefix", c.removes, s.state.WatchdogTagPrefix)
		}
	})
	t.Run("persisted prefix is not operator protection or match input", func(t *testing.T) {
		s, _, _, _ := fixture(t)
		s.c.TagSync.Enabled = false
		s.c.TagSync.Prefix = "new-"
		s.state.WatchdogTagPrefix = "old-"
		s.c.ExcludeTags = []string{"old-ignore"}
		protected := torrent(hashA)
		protected.Tags = "old-ignore"
		if s.protected(protected) {
			t.Fatal("old watchdog-prefixed exclude tag protected torrent")
		}
		policy := s.c.Policies[config.StoppedArrManaged]
		policy.MatchTags = []string{"old-stopped_arr_managed"}
		s.c.Policies[config.StoppedArrManaged] = policy
		stopped := qbt.Torrent{Hash: hashA, Name: "stopped", State: "stoppedDL", Progress: .5, Downloaded: 1, TotalSize: 2, Size: 2, Tags: "old-stopped_arr_managed"}
		if got := s.matchingPolicy(stopped); got != "" {
			t.Fatal("old watchdog-prefixed match tag classified torrent", got)
		}
	})
}
