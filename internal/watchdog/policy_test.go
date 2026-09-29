package watchdog

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

func setPartition(c *fakeClient, id config.PolicyID) {
	c.torrents[0] = torrent(hashA)
	if id == config.Metadata {
		return
	}
	if id == config.CompletedNoData {
		c.torrents[0].State = "uploading"
		c.torrents[0].TotalSize = 1024
		return
	}
	if id == config.StoppedArrManaged {
		c.torrents[0].State = "stoppedDL"
		c.torrents[0].Progress = .25
		c.torrents[0].Downloaded = 1024
		c.torrents[0].Tags = "Sonarr"
		return
	}
	c.torrents[0].State = "stalledDL"
	if id == config.StalledSeedersSeen {
		c.torrents[0].NumSeeds = 1
	}
	if id == config.StalledPartial {
		c.torrents[0].Progress = .25
		c.torrents[0].Downloaded = 1024
	}
}

func TestEveryPolicyActionAndDryRun(t *testing.T) {
	for _, id := range config.PolicyIDs() {
		for _, action := range []config.Action{config.Warn, config.Delete, config.DeleteFile} {
			for _, dry := range []bool{true, false} {
				t.Run(string(id)+"/"+string(action)+map[bool]string{true: "/dry", false: "/active"}[dry], func(t *testing.T) {
					s, c, clock, _ := fixture(t)
					s.c.DryRun = dry
					policy := s.c.Policies[id]
					policy.Action = action
					policy.Threshold = 20 * time.Second
					s.c.Policies[id] = policy
					setPartition(c, id)
					poll(t, s)
					clock.Advance(19 * time.Second)
					poll(t, s)
					if len(c.deletes) != 0 || len(s.state.History) != 0 {
						t.Fatal("early action")
					}
					clock.Advance(time.Second)
					poll(t, s)
					if dry || action == config.Warn {
						if len(c.deletes) != 0 || len(s.state.History) != 1 || s.state.History[0].EffectiveAction != config.Warn {
							t.Fatal("Warn override failed")
						}
						poll(t, s)
						if len(s.state.History) != 1 {
							t.Fatal("warning duplicated")
						}
					} else if len(c.deletes) != 1 || c.files[0] != (action == config.DeleteFile) || len(c.gets) != 2 {
						t.Fatal("incorrect mutation", c.deletes, c.files, c.gets)
					}
					row := s.Snapshot().Torrents[0]
					if row.Policy != id || row.ThresholdSeconds != 20 || row.EffectiveAction != s.c.EffectiveAction(id) {
						t.Fatal(row)
					}
				})
			}
		}
	}
}

func TestCompletedNoDataPolicyIsNarrow(t *testing.T) {
	s, _, _, _ := fixture(t)

	base := qbt.Torrent{Hash: hashA, Name: "completed", State: "uploading", TotalSize: 1024, AmountLeft: 0, Downloaded: 0, Size: 0}
	if got := s.matchingPolicy(base); got != config.CompletedNoData {
		t.Fatal("zero-payload completed torrent not classified", got)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*qbt.Torrent)
	}{
		{"cross-seed recheck has size", func(t *qbt.Torrent) { t.Size = 1024 }},
		{"manual partial deselect has size", func(t *qbt.Torrent) { t.Size = 256 }},
		{"downloaded then deselected has size", func(t *qbt.Torrent) { t.Size, t.Downloaded = 1, 0 }},
		{"empty torrent has no total", func(t *qbt.Torrent) { t.TotalSize = 0 }},
		{"missing files state", func(t *qbt.Torrent) { t.State = "missingFiles" }},
		{"checking up state", func(t *qbt.Torrent) { t.State = "checkingUP" }},
		{"paused down state", func(t *qbt.Torrent) { t.State = "pausedDL" }},
		{"stopped down state", func(t *qbt.Torrent) { t.State = "stoppedDL" }},
		{"known hole downloaded bytes", func(t *qbt.Torrent) { t.State, t.Downloaded = "stalledUP", 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			tc.mutate(&candidate)
			if got := s.matchingPolicy(candidate); got != "" {
				t.Fatal("false positive", got, candidate)
			}
		})
	}
	for _, state := range []string{"pausedUP", "stoppedUP"} {
		t.Run(state+" zero payload", func(t *testing.T) {
			candidate := base
			candidate.State = state
			if got := s.matchingPolicy(candidate); got != config.CompletedNoData {
				t.Fatal("stopped completed no-data not classified", got)
			}
		})
	}
}

func TestStoppedArrManagedMatchingAndPrecedence(t *testing.T) {
	s, _, _, _ := fixture(t)
	s.c.Policies[config.StoppedArrManaged] = config.Policy{Action: config.Warn, Threshold: time.Minute, ArrMode: config.InheritArrMode, MatchTags: []string{"Sonarr"}}
	partial := qbt.Torrent{Hash: hashA, Name: "stopped", State: "stoppedDL", Progress: .5, Downloaded: 1234, TotalSize: 2048, Size: 1024, Tags: "Sonarr"}
	if got := s.matchingPolicy(partial); got != config.StoppedArrManaged {
		t.Fatal("stopped Arr torrent not classified", got)
	}
	for _, state := range []string{"pausedUP", "stoppedUP"} {
		t.Run(state+" payload", func(t *testing.T) {
			candidate := partial
			candidate.State = state
			if got := s.matchingPolicy(candidate); got != config.StoppedArrManaged {
				t.Fatal("stopped Arr completed-state torrent with payload not classified", got)
			}
		})
	}
	partial.Tags = "qbtw-Sonarr"
	if got := s.matchingPolicy(partial); got != "" {
		t.Fatal("watchdog tag affected classification", got)
	}
	partial.Tags = "Other"
	if got := s.matchingPolicy(partial); got != "" {
		t.Fatal("unmatched tag classified", got)
	}
	emptyTagged := qbt.Torrent{Hash: hashA, Name: "empty", State: "stoppedUP", TotalSize: 1024, AmountLeft: 0, Downloaded: 0, Size: 0, Tags: "Sonarr"}
	if got := s.matchingPolicy(emptyTagged); got != config.CompletedNoData {
		t.Fatal("completed_no_data did not take precedence", got)
	}
}

func TestSeedObservationPersistsAcrossStatesRestartAndRemoval(t *testing.T) {
	for _, state := range []string{"metaDL", "downloading", "forcedDL", "queuedDL", "pausedDL", "stoppedDL", "stalledDL", "checkingDL"} {
		t.Run(state, func(t *testing.T) {
			s, c, clock, disk := fixture(t)
			c.torrents[0].State = state
			c.torrents[0].NumSeeds = 2
			poll(t, s)
			if !disk.state.SeedObserved[hashA] {
				t.Fatal("seed observation not durable")
			}
			c.torrents[0].NumSeeds = 0
			c.torrents[0].State = "stalledDL"
			s = New(s.c, c, disk, clock, s.log, observability.New(s.view.Build), s.view.Build)
			poll(t, s)
			if s.state.Tracked[hashA].Policy != config.StalledSeedersSeen {
				t.Fatal("seed history lost")
			}
			c.torrents = nil
			poll(t, s)
			if len(disk.state.SeedObserved) != 0 || len(disk.state.Tracked) != 0 {
				t.Fatal("removed torrent retained")
			}
			c.torrents = []qbt.Torrent{torrent(hashA)}
			c.torrents[0].State = "stalledDL"
			poll(t, s)
			if s.state.Tracked[hashA].Policy != config.StalledNoSeeders {
				t.Fatal("inferred history before observation")
			}
		})
	}
}

func TestPolicyTransitionsGapsAndExclusionsResetFullThreshold(t *testing.T) {
	for _, id := range config.PolicyIDs() {
		for _, interruption := range []string{"gap", "excluded tag", "excluded category", "outside include", "state exit"} {
			t.Run(string(id)+"/"+interruption, func(t *testing.T) {
				s, c, clock, _ := fixture(t)
				setPartition(c, id)
				s.c.DryRun = false
				poll(t, s)
				clock.Advance(19 * time.Second)
				poll(t, s)
				switch interruption {
				case "gap":
					clock.Advance(s.c.MaxObservationGap + time.Second)
				case "excluded tag":
					c.torrents[0].Tags = "keep"
				case "excluded category":
					s.c.ExcludeCategories = []string{"excluded"}
					c.torrents[0].Category = "excluded"
				case "outside include":
					s.c.IncludeCategories = []string{"allowed"}
					c.torrents[0].Category = "other"
				case "state exit":
					c.torrents[0].State = "downloading"
				}
				poll(t, s)
				setPartition(c, id)
				if id != config.StoppedArrManaged {
					c.torrents[0].Tags = ""
				}
				c.torrents[0].Category = "allowed"
				poll(t, s)
				clock.Advance(19 * time.Second)
				poll(t, s)
				if len(c.deletes) != 0 {
					t.Fatal("interruption inherited time")
				}
				clock.Advance(time.Second)
				poll(t, s)
				if len(c.deletes) != 1 {
					t.Fatal("full episode failed")
				}
			})
		}
	}
	t.Run("partition transitions", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		s.c.DryRun = false
		poll(t, s)
		for _, id := range []config.PolicyID{config.StalledNoSeeders, config.StalledSeedersSeen, config.StalledPartial, config.Metadata} {
			clock.Advance(20 * time.Second)
			setPartition(c, id)
			poll(t, s)
			if len(c.deletes) != 0 || !s.state.Tracked[hashA].FirstSeen.Equal(clock.Now()) {
				t.Fatal("policy reused elapsed time", id)
			}
		}
	})
}

func TestFinalReadRechecksAllPolicyRules(t *testing.T) {
	for _, id := range config.PolicyIDs() {
		for _, change := range []string{"state", "progress", "payload", "tag", "category", "include", "seeds", "gap", "missing"} {
			if change == "seeds" && id != config.StalledNoSeeders || change == "payload" && (id == config.StalledPartial || id == config.StoppedArrManaged) || change == "progress" && (id == config.CompletedNoData || id == config.StoppedArrManaged) {
				continue
			}
			t.Run(string(id)+"/"+change, func(t *testing.T) {
				s, c, clock, _ := fixture(t)
				s.c.DryRun = false
				setPartition(c, id)
				s.c.ExcludeCategories = []string{"excluded"}
				s.c.IncludeCategories = []string{"allowed"}
				c.torrents[0].Category = "allowed"
				poll(t, s)
				clock.Advance(20 * time.Second)
				c.fresh = func(string) *qbt.Torrent {
					fresh := c.torrents[0]
					if len(c.gets) != 2 {
						return &fresh
					}
					switch change {
					case "state":
						fresh.State = "downloading"
					case "progress":
						if fresh.Progress > 0 {
							fresh.Progress = 0
						} else {
							fresh.Progress = .5
						}
					case "payload":
						fresh.Downloaded = 1
					case "tag":
						fresh.Tags = "keep"
					case "category":
						fresh.Category = "excluded"
					case "include":
						fresh.Category = "other"
					case "seeds":
						fresh.NumSeeds = 1
					case "gap":
						clock.Advance(s.c.MaxObservationGap + time.Second)
					case "missing":
						return nil
					}
					return &fresh
				}
				poll(t, s)
				if len(c.gets) != 2 || len(c.deletes) != 0 {
					t.Fatal("final rule bypassed", c.gets, c.deletes)
				}
				if s.state.Tracked[hashA].Attempts != 0 {
					t.Fatal("known-unsent reservation retained")
				}
				if change == "seeds" && !s.state.SeedObserved[hashA] {
					t.Fatal("fresh seeder lost")
				}
			})
		}
	}
}

func TestSeederAndNegativeObservationSurviveFailedBatch(t *testing.T) {
	s, c, clock, disk := fixture(t)
	s.c.DryRun = false
	c.torrents = []qbt.Torrent{torrent(hashA), torrent(hashB)}
	c.torrents[0].State = "stalledDL"
	poll(t, s)
	clock.Advance(20 * time.Second)
	c.fresh = func(hash string) *qbt.Torrent { fresh := c.torrents[0]; fresh.NumSeeds = 1; return &fresh }
	s.client = &targetedErrorClient{fakeClient: c, failHash: hashB}
	if s.Poll(context.Background()) == nil || len(c.deletes) != 0 {
		t.Fatal("failed batch mutated")
	}
	if !disk.state.SeedObserved[hashA] || disk.state.Tracked[hashA].Policy != config.StalledSeedersSeen || !disk.state.Tracked[hashA].FirstSeen.Equal(clock.Now()) {
		t.Fatal("transition lost on failure")
	}
}

func TestReloadResetsSafetyTimersWithoutResettingRequestBudget(t *testing.T) {
	for _, change := range []string{"dry run", "action", "threshold", "exclusions", "poll interval", "observation gap", "http timeout", "confirmation timeout", "cap", "auth", "tls"} {
		t.Run(change, func(t *testing.T) {
			s, c, clock, disk := fixture(t)
			s.c.DryRun = false
			s.manager = config.NewManager(s.c.ConfigFile, s.c)
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
			before := s.state.Tracked[hashA]
			if before.Attempts != 1 || before.DeleteRequestedAt == nil {
				t.Fatal("missing pending request")
			}
			next := s.c.Clone()
			p := next.Policies[config.Metadata]
			switch change {
			case "dry run":
				next.DryRun = true
			case "action":
				p.Action = config.DeleteFile
				next.Policies[config.Metadata] = p
			case "threshold":
				p.Threshold = time.Second
				next.Policies[config.Metadata] = p
			case "exclusions":
				next.ExcludeTags = append(next.ExcludeTags, "extra")
			case "poll interval":
				next.PollInterval = time.Second
			case "observation gap":
				next.MaxObservationGap = time.Minute
			case "http timeout":
				next.HTTPTimeout = time.Second
			case "confirmation timeout":
				next.DeleteConfirmationTimeout = time.Second
			case "cap":
				next.MaxDeletions = 2
			case "auth":
				next.Username = "new"
				next.Password = "SECRET"
			case "tls":
				next.TLSInsecure = true
			}
			clock.Advance(time.Second)
			if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err != nil {
				t.Fatal(err)
			}
			e := s.state.Tracked[hashA]
			if e.Policy != "" || e.DryRunNotified || e.Attempts != 1 || e.DeleteRequestedAt == nil || !e.FirstSeen.Equal(clock.Now()) {
				t.Fatal("unsafe reload", e)
			}
			poll(t, s)
			if len(c.deletes) != 1 {
				t.Fatal("instant destructive escalation")
			}
			if disk.state.Tracked[hashA].Attempts != 1 {
				t.Fatal("retry budget not durable")
			}
		})
	}
}

func TestReloadWarnToDeleteFileWaitsFullEpisodeAndPendingRetriesFinite(t *testing.T) {
	s, c, clock, disk := fixture(t)
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	next := s.c.Clone()
	next.DryRun = false
	next.Policies[config.Metadata] = config.Policy{Action: config.DeleteFile, Threshold: 20 * time.Second}
	if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	poll(t, s)
	clock.Advance(19 * time.Second)
	poll(t, s)
	if len(c.deletes) != 0 {
		t.Fatal("reload escalated immediately")
	}
	clock.Advance(time.Second)
	poll(t, s)
	for range 5 {
		clock.Advance(20 * time.Second)
		poll(t, s)
	}
	if len(c.deletes) != store.MaxAttempts || !c.files[0] {
		t.Fatal("pending retries unbounded")
	}
	restarted := New(s.c, c, disk, clock, s.log, observability.New(s.view.Build), s.view.Build)
	poll(t, restarted)
	if len(c.deletes) != store.MaxAttempts {
		t.Fatal("restart reset attempts")
	}
}

func TestInvalidReloadLastKnownGoodAndConcurrentSnapshots(t *testing.T) {
	s, c, _, _ := fixture(t)
	poll(t, s)
	before := s.Config()
	for _, field := range []string{"listen", "state", "level", "format", "client"} {
		next := before.Clone()
		switch field {
		case "listen":
			next.Listen = ":9090"
		case "state":
			next.StateFile = "other"
		case "level":
			next.LogLevel = "debug"
		case "format":
			next.LogFormat = "text"
		case "client":
			next.Password = "new"
		}
		err := s.Reload(next, func(config.Config) (Client, error) { return nil, errors.New("invalid client TLS configuration") })
		if err == nil {
			t.Fatal("invalid reload accepted")
		}
		s.ReportReload(err)
		if !reflect.DeepEqual(s.Config(), before) || s.Snapshot().ConfigStatus.LastReloadError == "" {
			t.Fatal("last good lost")
		}
		if ready, _ := Ready(s.Snapshot(), time.Now(), time.Hour); ready {
			t.Fatal("invalid reload ready")
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				snapshot := s.Snapshot()
				snapshot.Policies[0].Action = config.DeleteFile
				cfg := s.Config()
				cfg.Policies[config.Metadata] = config.Policy{Action: config.DeleteFile, Threshold: time.Second}
				cfg.URL.Host = "MUTATED"
			}
		}()
	}
	for i := range 20 {
		next := before.Clone()
		next.UIRefreshInterval = time.Duration(i+1) * time.Second
		if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if s.Config().URL.Host == "MUTATED" || s.Config().Policies[config.Metadata].Action != config.Delete {
		t.Fatal("snapshot mutated active config")
	}
	s.ReportReload(nil)
	if s.Snapshot().ConfigStatus.LastReloadError != "" {
		t.Fatal("reload recovery not visible")
	}
}

func TestUnchangedFailedPollDoesNotRewriteState(t *testing.T) {
	s, c, _, disk := fixture(t)
	poll(t, s)
	before := disk.saves
	c.listError = errors.New("offline")
	for range 5 {
		_ = s.Poll(context.Background())
	}
	if disk.saves != before {
		t.Fatal("unchanged state rewritten")
	}
}
