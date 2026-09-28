package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"qbt-watchdog/internal/arr"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
)

func TestRecoveryHTTPPackAndMovie(t *testing.T) {
	for _, kind := range []config.ArrKind{config.Sonarr, config.Radarr} {
		t.Run(string(kind), func(t *testing.T) {
			s, q, clock, disk, _ := recoveryFixture(t, kind, config.BlocklistAndSearch)
			var removed, searches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("X-Api-Key") != "http-test-key" {
					t.Error("missing credential")
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == "GET" && req.URL.Path == "/base/api/v3/queue":
					rows := []map[string]any{}
					if removed.Load() == 0 {
						field := "episodeId"
						ids := []int{11, 12}
						if kind == config.Radarr {
							field, ids = "movieId", []int{11}
						}
						for i, id := range ids {
							rows = append(rows, map[string]any{"id": i + 1, "downloadId": strings.ToUpper(hashA), field: id})
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"records": rows, "totalRecords": len(rows), "page": 1, "pageSize": 200})
				case req.Method == "DELETE" && req.URL.Path == "/base/api/v3/queue/1":
					values := req.URL.Query()
					for key, want := range map[string]string{"removeFromClient": "false", "blocklist": "true", "skipRedownload": "true", "changeCategory": "false"} {
						if values.Get(key) != want {
							t.Errorf("unsafe %s", key)
						}
					}
					removed.Add(1)
					w.WriteHeader(http.StatusOK)
				case req.Method == "GET" && (strings.Contains(req.URL.Path, "/episode/") || strings.Contains(req.URL.Path, "/movie/")):
					id := 11
					if strings.HasSuffix(req.URL.Path, "/12") {
						id = 12
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "hasFile": false, "monitored": true})
				case req.Method == "POST" && req.URL.Path == "/base/api/v3/command":
					var body struct {
						Name                 string
						EpisodeIDs, MovieIDs []int64
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if kind == config.Sonarr && (body.Name != "EpisodeSearch" || !slices.Equal(body.EpisodeIDs, []int64{11, 12})) {
						t.Error("pack search not exact", body)
					}
					if kind == config.Radarr && (body.Name != "MoviesSearch" || !slices.Equal(body.MovieIDs, []int64{11})) {
						t.Error("movie search not exact", body)
					}
					searches.Add(1)
					_, _ = fmt.Fprint(w, `{"id":44,"status":"queued"}`)
				case req.Method == "GET" && req.URL.Path == "/base/api/v3/command/44":
					_, _ = fmt.Fprint(w, `{"id":44,"status":"completed"}`)
				default:
					t.Error("unexpected request", req.Method, req.URL.Path)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			c := s.Config()
			app := &c.Integrations.Sonarr
			if kind == config.Radarr {
				app = &c.Integrations.Radarr
			}
			app.URL, _ = url.Parse(server.URL + "/base/")
			app.APIKey = "http-test-key"
			s.arrFactory = func(c config.ArrService) (ArrClient, error) { return arr.New(c) }
			if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
				t.Fatal(err)
			}
			cycle(s, kind)
			acceptRecovery(t, s, clock)
			if len(disk.state.RecoveryJobs) != 1 {
				t.Fatal("job not durably prepared")
			}
			cycle(s, kind)
			if removed.Load() != 0 {
				t.Fatal("mutation before confirmation")
			}
			q.torrents = nil
			poll(t, s)
			for range 4 {
				cycle(s, kind)
			}
			if removed.Load() != 1 || searches.Load() != 1 || len(s.Snapshot().RecoveryJobs) != 0 {
				t.Fatal("recovery incomplete", removed.Load(), searches.Load(), s.Snapshot().RecoveryJobs)
			}
			if s.Snapshot().History[len(s.Snapshot().History)-1].Error != "search_completed" {
				t.Fatal("completion not reported")
			}
		})
	}
}

func TestRecoveryCaptureCannotExpandAfterDeletion(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	cycle(s, config.Sonarr)
	acceptRecovery(t, s, clock)
	id := int64(99)
	f.items = append(f.items, arr.QueueItem{ID: 3, DownloadID: hashA, MediaID: &id})
	q.torrents = nil
	poll(t, s)
	for range 4 {
		cycle(s, config.Sonarr)
	}
	if len(f.searches) != 0 {
		t.Fatal("search identity expanded")
	}
}

func TestRecoveryDuplicateReservationAndCapacity(t *testing.T) {
	s, _, clock, _, _ := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	e := store.Episode{Policy: config.Metadata, FirstSeen: clock.Now()}
	s.prepareRecovery(torrent(hashA), e)
	s.prepareRecovery(torrent(hashA), e)
	if len(s.state.RecoveryJobs) != 1 {
		t.Fatal("duplicate reservation")
	}
	for i := 1; i < store.MaxRecoveryJobs; i++ {
		torrent := torrent(fmt.Sprintf("%040x", i))
		s.prepareRecovery(torrent, e)
	}
	s.prepareRecovery(torrent(hashB), e)
	if len(s.state.RecoveryJobs) != store.MaxRecoveryJobs {
		t.Fatal("job cap not enforced")
	}
	clock.Advance(store.RecoveryTTL)
	cycle(s, config.Sonarr)
	if len(s.state.RecoveryJobs) != 0 {
		t.Fatal("job cap did not drain")
	}
}

func TestRecoveryBlockedQueueDoesNotHoldPollOrReload(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	acceptRecovery(t, s, clock)
	entered := make(chan struct{})
	f.onQueue = func(ctx context.Context) { close(entered); <-ctx.Done() }
	done := make(chan struct{})
	go func() { defer close(done); cycle(s, config.Sonarr) }()
	<-entered
	q.torrents = nil
	poll(t, s)
	c := s.Config()
	c.Integrations.Sonarr.Enabled = false
	if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reload failed to cancel queue read")
	}
	if len(f.searches) != 0 {
		t.Fatal("stale generation mutated")
	}
}

func TestRecoveryRunCancellationAndConcurrentSnapshots(t *testing.T) {
	s, _, _, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	entered := make(chan struct{})
	var once sync.Once
	f.onQueue = func(ctx context.Context) { once.Do(func() { close(entered) }); <-ctx.Done() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	<-entered
	for range 20 {
		_ = s.Snapshot()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker shutdown leaked")
	}
}

func TestRecoveryPersistenceFailurePreventsDispatch(t *testing.T) {
	s, q, clock, disk, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	cycle(s, config.Sonarr)
	disk.err = fmt.Errorf("synthetic persistence failure")
	cycle(s, config.Sonarr)
	if len(f.searches) != 0 {
		t.Fatal("mutation without persisted intent")
	}
	disk.err = nil
	for range 3 {
		cycle(s, config.Sonarr)
	}
	if len(f.searches) != 0 {
		t.Fatal("failed intent replayed")
	}
}

func TestRecoveryStaleCaptureAndPolicyPause(t *testing.T) {
	for _, gate := range []string{"warn", "cap"} {
		t.Run(gate, func(t *testing.T) {
			s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
			cycle(s, config.Sonarr)
			clock.Advance(queueFreshness + time.Second)
			acceptRecovery(t, s, clock)
			for _, job := range s.state.RecoveryJobs {
				if len(job.MediaIDs) != 0 {
					t.Fatal("stale queue captured")
				}
			}
			q.torrents = nil
			poll(t, s)
			cycle(s, config.Sonarr)
			c := s.Config()
			if gate == "warn" {
				c.Policies[config.Metadata] = config.Policy{Action: config.Warn, Threshold: 20 * time.Second}
			} else {
				c.MaxDeletions = 0
			}
			if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				cycle(s, config.Sonarr)
			}
			if len(f.searches) != 0 {
				t.Fatal("pending mutation bypassed policy gate")
			}
		})
	}
}

func TestRecoveryCredentialRotationRebuildsClientWithoutResettingClock(t *testing.T) {
	s, q, _, _, _ := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	var keys []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"records":[],"totalRecords":0}`)
	}))
	defer server.Close()
	s.arrFactory = func(c config.ArrService) (ArrClient, error) { return arr.New(c) }
	c := s.Config()
	c.Integrations.Sonarr.URL, _ = url.Parse(server.URL + "/")
	c.Integrations.Sonarr.APIKey = "first-key"
	if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
		t.Fatal(err)
	}
	poll(t, s)
	first := s.state.Tracked[hashA]
	old := s.integrations[config.Sonarr]
	cycle(s, config.Sonarr)
	c.Integrations.Sonarr.APIKey = "second-key"
	if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
		t.Fatal(err)
	}
	cycle(s, config.Sonarr)
	if old.client == s.integrations[config.Sonarr].client || old.ctx.Err() == nil {
		t.Fatal("client not replaced and fenced")
	}
	if s.state.Tracked[hashA] != first {
		t.Fatal("Arr-only reload reset qbt clocks")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(keys, []string{"first-key", "second-key"}) {
		t.Fatal("credentials not rotated")
	}
}

func TestRecoveryReloadDuringMutationNeverReplays(t *testing.T) {
	s, q, clock, _, f := recoveryFixture(t, config.Sonarr, config.SearchOnly)
	acceptRecovery(t, s, clock)
	q.torrents = nil
	poll(t, s)
	cycle(s, config.Sonarr)
	entered, release := make(chan struct{}), make(chan struct{})
	f.onMutation = func(store.RecoveryStage) { close(entered); <-release }
	done := make(chan struct{})
	go func() { defer close(done); cycle(s, config.Sonarr) }()
	<-entered
	c := s.Config()
	c.Integrations.Sonarr.APIKey = "new-key"
	if err := s.Reload(c, func(config.Config) (Client, error) { return q, nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	for range 3 {
		cycle(s, config.Sonarr)
	}
	if len(f.searches) != 1 || len(s.Snapshot().RecoveryJobs) != 1 || s.Snapshot().RecoveryJobs[0].Stage != store.Uncertain {
		t.Fatal("ambiguous old-generation mutation replayed")
	}
}
