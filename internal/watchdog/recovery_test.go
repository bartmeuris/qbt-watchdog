package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/arr"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

type fakeArr struct {
	items                                   []arr.QueueItem
	history                                 []arr.HistoryRecord
	queueErr, removeErr, searchErr, fileErr error
	imported                                bool
	removes                                 int
	searches                                [][]int64
	command                                 arr.Command
	onMutation                              func(store.RecoveryStage)
	onQueue                                 func(context.Context)
}

func (f *fakeArr) Queue(ctx context.Context) ([]arr.QueueItem, error) {
	if f.onQueue != nil {
		f.onQueue(ctx)
	}
	return slices.Clone(f.items), f.queueErr
}
func (f *fakeArr) Map(items []arr.QueueItem, hash string) (arr.Job, bool) {
	return arr.Match(items, hash)
}
func (f *fakeArr) RemoveJob(_ context.Context, _ arr.Job) error {
	if f.onMutation != nil {
		f.onMutation(store.BlocklistIntent)
	}
	f.removes++
	return f.removeErr
}
func (f *fakeArr) Search(_ context.Context, ids []int64) (arr.Command, error) {
	if f.onMutation != nil {
		f.onMutation(store.SearchIntent)
	}
	f.searches = append(f.searches, slices.Clone(ids))
	return arr.Command{ID: 44, Status: arr.StatusQueued}, f.searchErr
}
func (f *fakeArr) Status(context.Context, int64) (arr.Command, error) { return f.command, nil }
func (f *fakeArr) History(context.Context, string) ([]arr.HistoryRecord, error) {
	return f.history, nil
}
func (f *fakeArr) HasFile(context.Context, int64) (bool, error) { return f.imported, f.fileErr }
func (f *fakeArr) CloseIdleConnections()                        {}

func recoveryFixture(t *testing.T, kind config.ArrKind, mode config.ArrMode) (*Service, *fakeClient, *fakeClock, *memoryStore, *fakeArr) {
	t.Helper()
	s, q, clock, disk := fixture(t)
	id1, id2 := int64(11), int64(12)
	f := &fakeArr{items: []arr.QueueItem{{ID: 1, DownloadID: hashA, MediaID: &id1}, {ID: 2, DownloadID: hashA, MediaID: &id2}}, command: arr.Command{ID: 44, Status: arr.StatusCompleted}}
	s.arrFactory = func(config.ArrService) (ArrClient, error) { return f, nil }
	c := s.Config()
	c.DryRun = false
	u, _ := url.Parse("http://arr.invalid/base/")
	app := config.ArrService{Kind: kind, URL: u, APIKey: "test-secret-key", Enabled: true, Mode: mode, Timeout: time.Second}
	if kind == config.Sonarr {
		c.Integrations.Sonarr = app
	} else {
		c.Integrations.Radarr = app
		f.items = f.items[:1]
	}
	if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
		t.Fatal(err)
	}
	f.onMutation = func(stage store.RecoveryStage) {
		t.Helper()
		for _, j := range disk.state.RecoveryJobs {
			// The pre-delete blocklist is durable once BlocklistAt is set,
			// even though the torrent has not been confirmed deleted yet.
			if stage == store.BlocklistIntent && !j.BlocklistAt.IsZero() {
				return
			}
			if j.Stage == stage && !j.DeletedAt.IsZero() {
				return
			}
		}
		t.Fatal("mutation without durable confirmed-delete intent", stage)
	}
	return s, q, clock, disk, f
}

func acceptRecovery(t *testing.T, s *Service, clock *fakeClock) {
	t.Helper()
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
}

func cycle(s *Service, kind config.ArrKind) { s.recoveryCycle(context.Background(), kind) }

func TestRecoveryAcceptedThenConfirmedPackAndMovie(t *testing.T) {
	for _, kind := range []config.ArrKind{config.Sonarr, config.Radarr} {
		t.Run(string(kind), func(t *testing.T) {
			s, q, clock, disk, f := recoveryFixture(t, kind, config.BlocklistAndSearch)
			cycle(s, kind)
			acceptRecovery(t, s, clock)
			if len(q.deletes) != 1 || len(disk.state.RecoveryJobs) != 1 {
				t.Fatal("missing cleanup/job")
			}
			for range 3 {
				cycle(s, kind)
			}
			if f.removes != 1 || len(f.searches) != 0 {
				t.Fatal("blocklist must precede disappearance and search must wait")
			}
			q.torrents = nil
			poll(t, s)
			for range 4 {
				cycle(s, kind)
			}
			if f.removes != 1 || len(f.searches) != 1 || len(s.state.RecoveryJobs) != 0 {
				t.Fatalf("recovery incomplete: removes=%d searches=%v jobs=%v", f.removes, f.searches, s.Snapshot().RecoveryJobs)
			}
			if kind == config.Sonarr && !slices.Equal(f.searches[0], []int64{11, 12}) {
				t.Fatal("pack was not grouped")
			}
			if s.state.History[len(s.state.History)-1].Error != "search_completed" {
				t.Fatal("must distinguish search completion")
			}
		})
	}
}

func TestRecoveryNoMutationGates(t *testing.T) {
	for _, gate := range []string{"warn", "dry_run", "disabled", "qbt_failure", "list_failure", "final_get_abort"} {
		t.Run(gate, func(t *testing.T) {
			s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
			switch gate {
			case "warn":
				s.c.Policies[config.Metadata] = config.Policy{Action: config.Warn, Threshold: 20 * time.Second}
			case "dry_run":
				s.c.DryRun = true
			case "disabled":
				s.c.Integrations.Sonarr.Enabled = false
			case "qbt_failure":
				q.deleteError = errors.New("failure")
			}
			poll(t, s)
			clock.Advance(20 * time.Second)
			if gate == "list_failure" {
				q.listError = errors.New("failure")
			}
			if gate == "final_get_abort" {
				// A fresh queue snapshot would let a pre-recheck blocklist
				// attempt reach the media manager, so the no-mutation
				// assertion below actually exercises the blocklist path.
				cycle(s, config.Sonarr)
				q.fresh = func(string) *qbt.Torrent {
					if len(q.gets) == 2 {
						return nil
					}
					v := torrent(hashA)
					return &v
				}
			}
			_ = s.Poll(context.Background())
			q.torrents = nil
			q.listError = nil
			poll(t, s)
			for range 5 {
				cycle(s, config.Sonarr)
			}
			if f.removes != 0 || len(f.searches) != 0 {
				t.Fatal("unsafe mutation")
			}
		})
	}
}

func TestRecoveryPolicyArrModeNoneCreatesNoJobs(t *testing.T) {
	s, q, clock, disk, _ := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	policy := s.c.Policies[config.Metadata]
	policy.ArrMode = config.NoArrMode
	s.c.Policies[config.Metadata] = policy

	acceptRecovery(t, s, clock)
	if len(q.deletes) != 1 {
		t.Fatal("cleanup did not run")
	}
	if len(disk.state.RecoveryJobs) != 0 {
		t.Fatal("arr_mode none created recovery jobs")
	}
}

func TestRecoveryOutageDoesNotBlockCleanupOrReadiness(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	f.queueErr = &arr.Error{Outcome: arr.Unreachable}
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	if len(q.deletes) != 1 {
		t.Fatal("Arr outage blocked cleanup")
	}
	if ready, _ := Ready(s.Snapshot(), clock.Now(), time.Minute); !ready {
		t.Fatal("Arr outage degraded readiness")
	}
}

func TestRecoveryHistoryExactIdentityOnly(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	f.items = nil
	id, wrong := int64(51), int64(99)
	f.history = []arr.HistoryRecord{{ID: 1, DownloadID: hashB, MediaID: &wrong}, {ID: 2, DownloadID: strings.ToUpper(hashA), MediaID: &id}}
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	for range 3 {
		cycle(s, config.Sonarr)
	}
	if f.removes != 0 || len(f.searches) != 1 || !slices.Equal(f.searches[0], []int64{51}) {
		t.Fatal("history identity unsafe", f.searches)
	}
}

func TestRecoverySafetyAndUncertainNeverReplayed(t *testing.T) {
	for _, scenario := range []string{"vanished", "404", "incomplete", "replacement", "imported", "file_unknown", "ambiguous_delete", "ambiguous_search"} {
		t.Run(scenario, func(t *testing.T) {
			s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
			// A fresh queue snapshot lets the pre-delete blocklist attempt
			// actually reach the media manager, so the blocklist outcomes are
			// exercised rather than skipped.
			cycle(s, config.Sonarr)
			switch scenario {
			case "404":
				f.removeErr = &arr.Error{Outcome: arr.NotFound}
			case "ambiguous_delete":
				f.removeErr = errors.New("EOF")
			}
			acceptRecovery(t, s, clock)
			q.torrents = nil
			poll(t, s)
			switch scenario {
			case "vanished":
				f.items = nil
			case "incomplete":
				f.items[0].MediaID = nil
			case "replacement":
				id := int64(11)
				f.items = append(f.items, arr.QueueItem{ID: 3, DownloadID: hashB, MediaID: &id})
			case "imported":
				f.imported = true
			case "file_unknown":
				f.fileErr = &arr.Error{Outcome: arr.Rejected}
			case "ambiguous_search":
				f.searchErr = errors.New("EOF")
			}
			for range 10 {
				cycle(s, config.Sonarr)
				clock.Advance(time.Minute)
			}
			// A failed or ambiguous blocklist no longer suppresses the
			// replacement search: it runs once removal is confirmed.
			wantSearch := 0
			switch scenario {
			case "vanished", "404", "ambiguous_delete", "ambiguous_search":
				wantSearch = 1
			}
			if len(f.searches) != wantSearch || f.removes > 1 {
				t.Fatal("unsafe/replayed mutations", f.removes, f.searches)
			}
		})
	}
}

func TestRecoveryReloadPausesAndInvalidates(t *testing.T) {
	for _, change := range []string{"key", "endpoint", "disabled", "dry_run", "arr_mode_none"} {
		t.Run(change, func(t *testing.T) {
			s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
			acceptRecovery(t, s, clock)
			q.torrents = nil
			poll(t, s)
			cycle(s, config.Sonarr)
			c := s.Config()
			switch change {
			case "key":
				c.Integrations.Sonarr.APIKey = "rotated-key"
			case "endpoint":
				c.Integrations.Sonarr.URL.Host = "other.invalid"
			case "disabled":
				c.Integrations.Sonarr.Enabled = false
			case "dry_run":
				c.DryRun = true
			case "arr_mode_none":
				policy := c.Policies[config.Metadata]
				policy.ArrMode = config.NoArrMode
				c.Policies[config.Metadata] = policy
			}
			if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				cycle(s, config.Sonarr)
			}
			want := 0
			if change == "key" {
				want = 1
			}
			if len(f.searches) != want {
				t.Fatal("reload gate", len(f.searches))
			}
		})
	}
}

func TestRecoveryRestartIntentBoundaries(t *testing.T) {
	for _, stage := range []store.RecoveryStage{store.Prepared, store.AwaitDelete, store.Resolving, store.BlocklistPending, store.BlocklistIntent, store.SearchPending, store.SearchIntent, store.CommandPending} {
		t.Run(string(stage), func(t *testing.T) {
			s, q, clock, disk, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
			acceptRecovery(t, s, clock)
			for id, j := range s.state.RecoveryJobs {
				j.Stage = stage
				j.MediaIDs = []int64{11, 12}
				j.QueueIDs = []int64{1, 2}
				if stage != store.Prepared && stage != store.AwaitDelete {
					j.DeletedAt = clock.Now()
				}
				if stage == store.CommandPending {
					j.CommandID = 44
				}
				s.state.RecoveryJobs[id] = j
			}
			s.persist()
			build := testBuild()
			restarted := New(s.Config(), q, disk, clock, s.log, observability.New(build), build)
			restarted.integrations[config.Sonarr].client.CloseIdleConnections()
			restarted.integrations[config.Sonarr].client = f
			q.torrents = nil
			poll(t, restarted)
			for range 4 {
				cycle(restarted, config.Sonarr)
			}
			if stage == store.Prepared || stage == store.SearchIntent || stage == store.BlocklistIntent || stage == store.CommandPending {
				if len(f.searches) != 0 || f.removes != 0 {
					t.Fatal("replayed intent")
				}
			} else if len(f.searches) != 1 {
				t.Fatal("safe recovery did not resume")
			}
		})
	}
}

func TestRecoveryBoundedRetryTTLAndRedaction(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	f.items = nil
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	for range store.MaxRecoveryAttempts {
		cycle(s, config.Sonarr)
		clock.Advance(time.Minute)
	}
	if len(s.state.RecoveryJobs) != 0 {
		t.Fatal("unbounded read attempts")
	}
	data, _ := json.Marshal(s.Snapshot())
	for _, secret := range []string{hashA, "test-secret-key", "arr.invalid"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("leak", secret)
		}
	}
	f.items = []arr.QueueItem{}
	q.torrents = []qbt.Torrent{torrent(hashA)}
	acceptRecovery(t, s, clock)
	clock.Advance(store.RecoveryTTL)
	cycle(s, config.Sonarr)
	if len(s.state.RecoveryJobs) != 0 {
		t.Fatal("TTL not enforced")
	}
}

func TestRecoveryBlocklistOnlyFinishesWithoutSearch(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistOnly)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	for range 4 {
		cycle(s, config.Sonarr)
	}
	if f.removes != 1 || len(f.searches) != 0 || len(s.state.RecoveryJobs) != 0 {
		t.Fatalf("blocklist_only must blocklist once and never search: removes=%d searches=%v jobs=%v", f.removes, f.searches, s.Snapshot().RecoveryJobs)
	}
	if s.state.History[len(s.state.History)-1].Error != "blocklist_completed" {
		t.Fatal("blocklist_only must finish with blocklist_completed")
	}
}

func TestRecoveryVanishedQueueStillSearchesWhenMediaKnown(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	f.items = nil
	q.torrents = nil
	poll(t, s)
	for range 4 {
		cycle(s, config.Sonarr)
	}
	if f.removes != 1 || len(f.searches) != 1 {
		t.Fatalf("vanished queue item with known media must still search: removes=%d searches=%v", f.removes, f.searches)
	}
}

func TestRecoveryVanishedQueueDoesNotSearchWhenImported(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	f.items = nil
	f.imported = true
	q.torrents = nil
	poll(t, s)
	for range 4 {
		cycle(s, config.Sonarr)
	}
	if f.removes != 1 || len(f.searches) != 0 {
		t.Fatalf("imported media must not be searched: removes=%d searches=%v", f.removes, f.searches)
	}
}

func TestRecoveryEventsCarryTorrentIdentity(t *testing.T) {
	s, q, clock, _, _ := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	for range 4 {
		cycle(s, config.Sonarr)
	}
	last := s.state.History[len(s.state.History)-1]
	if last.Error != "search_completed" || last.ShortHash != qbt.ShortHash(hashA) || last.CommandID != 44 {
		t.Fatalf("recovery event lost torrent identity: %+v", last)
	}
}

func TestRecoveryStatusCarriesCorrelation(t *testing.T) {
	s, q, clock, _, _ := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	for range 2 {
		cycle(s, config.Sonarr)
	}
	jobs := s.Snapshot().RecoveryJobs
	if len(jobs) != 1 {
		t.Fatalf("expected one recovery job, got %d", len(jobs))
	}
	job := jobs[0]
	if job.ID == "" || job.ShortHash != qbt.ShortHash(hashA) || job.Policy != config.Metadata || job.CommandID != 44 {
		t.Fatalf("recovery status lost correlation: %+v", job)
	}
}

// TestRecoveryBlocklistPrecedesQBTDelete pins the new ordering: the one
// best-effort blocklist attempt happens while the torrent is still present,
// before the qBittorrent delete request.
func TestRecoveryBlocklistPrecedesQBTDelete(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	var order []string
	f.onMutation = func(stage store.RecoveryStage) {
		if stage == store.BlocklistIntent {
			order = append(order, "blocklist")
		}
	}
	q.onDelete = func(string) { order = append(order, "delete") }
	acceptRecovery(t, s, clock)
	if !slices.Equal(order, []string{"blocklist", "delete"}) {
		t.Fatalf("blocklist must precede qbt delete: %v", order)
	}
}

// TestRecoveryBlocklistSkippedWhenFinalRecheckIneligible proves the blocklist
// attempt runs only after the final eligibility recheck: when the recheck
// returns a torrent that no longer matches its policy, the release is not
// blocklisted and no qBittorrent delete is issued.
func TestRecoveryBlocklistSkippedWhenFinalRecheckIneligible(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	poll(t, s)
	clock.Advance(20 * time.Second)
	// A fresh queue snapshot would let a pre-recheck blocklist attempt reach
	// the media manager, so the assertions below are meaningful.
	cycle(s, config.Sonarr)
	q.fresh = func(string) *qbt.Torrent {
		v := torrent(hashA)
		if len(q.gets) == 2 {
			// The final recheck sees nonzero progress, so the metadata policy
			// no longer matches and the torrent is ineligible.
			v.Progress = 0.5
		}
		return &v
	}
	poll(t, s)
	if f.removes != 0 {
		t.Fatalf("blocklisted an ineligible release: removes=%d", f.removes)
	}
	if len(q.deletes) != 0 {
		t.Fatalf("deleted an ineligible release: deletes=%v", q.deletes)
	}
}

// TestRecoveryBlocklistUnavailableStillCleansUp proves an unavailable media
// manager cannot block the qBittorrent cleanup; the failed attempt is recorded
// and never retried.
func TestRecoveryBlocklistUnavailableStillCleansUp(t *testing.T) {
	s, q, clock, disk, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	s.integrations[config.Sonarr].client = nil
	acceptRecovery(t, s, clock)
	if len(q.deletes) != 1 {
		t.Fatal("unavailable arr blocked qbt cleanup")
	}
	if f.removes != 0 {
		t.Fatal("blocklist attempted without a client")
	}
	for _, j := range disk.state.RecoveryJobs {
		if j.BlocklistCode != string(arr.Unreachable) {
			t.Fatalf("unavailable blocklist not recorded: %+v", j)
		}
	}
}

// TestRecoveryBlocklistVanishedStillCleansUp proves a media manager that has
// already dropped its queue row does not block cleanup; the attempt is recorded
// as not found.
func TestRecoveryBlocklistVanishedStillCleansUp(t *testing.T) {
	s, q, clock, disk, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	f.items = nil
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	if len(q.deletes) != 1 {
		t.Fatal("vanished arr row blocked qbt cleanup")
	}
	if f.removes != 0 {
		t.Fatal("blocklist attempted without a queue identity")
	}
	for _, j := range disk.state.RecoveryJobs {
		if j.BlocklistCode != string(arr.NotFound) {
			t.Fatalf("vanished blocklist not recorded: %+v", j)
		}
	}
}

// TestRecoveryBlocklistAttemptedOnce proves the durable BlocklistAt marker
// makes the best-effort attempt idempotent across a delete retry and a restart.
func TestRecoveryBlocklistAttemptedOnce(t *testing.T) {
	t.Run("delete_retry", func(t *testing.T) {
		s, _, _, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
		cycle(s, config.Sonarr)
		e := store.Episode{Policy: config.Metadata, FirstSeen: s.clock.Now()}
		s.mu.Lock()
		s.prepareRecovery(torrent(hashA), e)
		s.blocklistBeforeDelete(context.Background(), torrent(hashA))
		s.blocklistBeforeDelete(context.Background(), torrent(hashA))
		s.mu.Unlock()
		if f.removes != 1 {
			t.Fatalf("blocklist replayed on retry: %d", f.removes)
		}
	})
	t.Run("restart", func(t *testing.T) {
		s, q, clock, disk, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
		cycle(s, config.Sonarr)
		e := store.Episode{Policy: config.Metadata, FirstSeen: clock.Now()}
		s.mu.Lock()
		s.prepareRecovery(torrent(hashA), e)
		s.blocklistBeforeDelete(context.Background(), torrent(hashA))
		s.persist()
		s.mu.Unlock()
		if f.removes != 1 {
			t.Fatalf("expected one blocklist, got %d", f.removes)
		}
		build := testBuild()
		restarted := New(s.Config(), q, disk, clock, s.log, observability.New(build), build)
		restarted.integrations[config.Sonarr].client.CloseIdleConnections()
		restarted.integrations[config.Sonarr].client = f
		for _, j := range restarted.state.RecoveryJobs {
			if j.Stage != store.Uncertain {
				t.Fatalf("prepared job not fenced on restart: %+v", j)
			}
		}
		cycle(restarted, config.Sonarr)
		if f.removes != 1 {
			t.Fatalf("blocklist replayed after restart: %d", f.removes)
		}
	})
}

// TestRecoveryBlocklistNotRepeatedOnDeleteRetry proves that when an accepted
// deletion does not disappear within the confirmation timeout, the retried
// delete does not blocklist the release a second time.
func TestRecoveryBlocklistNotRepeatedOnDeleteRetry(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	if f.removes != 1 || len(q.deletes) != 1 {
		t.Fatalf("expected one blocklist and one delete, got removes=%d deletes=%d", f.removes, len(q.deletes))
	}
	clock.Advance(s.c.DeleteConfirmationTimeout + time.Second)
	poll(t, s)
	if f.removes != 1 {
		t.Fatalf("blocklist replayed on delete retry: %d", f.removes)
	}
	if len(q.deletes) != 2 {
		t.Fatalf("delete was not retried: %d", len(q.deletes))
	}
}

// TestRecoverySearchRunsAfterFailedBlocklist proves the replacement search is
// no longer suppressed when the pre-delete blocklist attempt fails.
func TestRecoverySearchRunsAfterFailedBlocklist(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.BlocklistAndSearch)
	cycle(s, config.Sonarr)
	f.removeErr = &arr.Error{Outcome: arr.NotFound}
	acceptRecovery(t, s, clock)
	if f.removes != 1 {
		t.Fatalf("expected one blocklist attempt, got %d", f.removes)
	}
	q.torrents = nil
	poll(t, s)
	for range 4 {
		cycle(s, config.Sonarr)
	}
	if len(f.searches) != 1 || len(s.state.RecoveryJobs) != 0 {
		t.Fatalf("search must run after confirmed removal despite failed blocklist: searches=%v jobs=%v", f.searches, s.Snapshot().RecoveryJobs)
	}
}
