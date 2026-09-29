package watchdog

import (
	"context"
	"errors"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/qbt"
)

// stalled returns a torrent already in the stalledDL state, which is the entry
// condition for three of the five partitions.
func stalled(hash string, progress float64, seeds int) qbt.Torrent {
	t := torrent(hash)
	t.State, t.Progress, t.NumSeeds = "stalledDL", progress, seeds
	if progress > 0 {
		t.Downloaded = 1024
	}
	return t
}

func TestPartitionsAreExclusiveAndTotal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		progress float64
		seeds    int
		observed bool
		want     config.PolicyID
	}{
		{name: "metadata", state: "metaDL", want: config.Metadata},
		{name: "metadata keeps its partition even with a seeder", state: "metaDL", seeds: 3, want: config.Metadata},
		{name: "zero progress never seeded", state: "stalledDL", want: config.StalledNoSeeders},
		{name: "zero progress seeded right now", state: "stalledDL", seeds: 1, want: config.StalledSeedersSeen},
		{name: "zero progress seeded earlier", state: "stalledDL", observed: true, want: config.StalledSeedersSeen},
		{name: "partial progress", state: "stalledDL", progress: .5, want: config.StalledPartial},
		{name: "partial progress ignores seeder history", state: "stalledDL", progress: .5, observed: true, want: config.StalledPartial},
		{name: "downloading", state: "downloading"},
		{name: "queued", state: "queuedDL"},
		{name: "paused", state: "pausedDL"},
		{name: "seeding", state: "stalledUP"},
		{name: "checking", state: "checkingDL"},
		{name: "unknown", state: "somethingElse"},
		{name: "empty", state: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := fixture(t)
			torrent := qbt.Torrent{Hash: hashA, State: tc.state, Progress: tc.progress, NumSeeds: tc.seeds}
			s.state.SeedObserved[hashA] = tc.observed
			got := s.policy(torrent)
			if got != tc.want {
				t.Fatal("wrong partition", got, "want", tc.want)
			}
			// Exclusivity: no other partition may claim the same torrent.
			matches := 0
			for _, id := range config.PolicyIDs() {
				if id == got {
					matches++
				}
			}
			if tc.want == "" && matches != 0 {
				t.Fatal("state outside every partition was claimed")
			}
			if tc.want != "" && matches != 1 {
				t.Fatal("torrent matched more than one partition", matches)
			}
		})
	}
}

func TestNoSeedersToSeedersSeenResetsEpisodeAndMarkers(t *testing.T) {
	t.Run("clock and warn marker", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		c.torrents = []qbt.Torrent{stalled(hashA, 0, 0)}
		poll(t, s)
		clock.Advance(20 * time.Second)
		poll(t, s)
		if s.state.Tracked[hashA].Policy != config.StalledNoSeeders || len(s.state.History) != 1 {
			t.Fatal("no-seeders episode did not complete", s.state.Tracked[hashA])
		}

		// A seeder appears: this is a different decision, so the episode and
		// its per-episode notification marker start over.
		c.torrents[0].NumSeeds = 2
		clock.Advance(time.Second)
		poll(t, s)
		e := s.state.Tracked[hashA]
		if e.Policy != config.StalledSeedersSeen || !e.FirstSeen.Equal(clock.Now()) || e.DryRunNotified {
			t.Fatal("transition reused the previous episode", e)
		}
		clock.Advance(19 * time.Second)
		poll(t, s)
		if len(s.state.History) != 1 {
			t.Fatal("new partition warned before its own threshold elapsed")
		}
		clock.Advance(time.Second)
		poll(t, s)
		if len(s.state.History) != 2 || s.state.History[1].Policy != config.StalledSeedersSeen {
			t.Fatal("new partition never warned", s.state.History)
		}
	})
}

// pollIgnoringError runs a poll that is expected to fail, which is how an
// attempt is consumed without producing a confirmable request.
func pollIgnoringError(s *Service) { _ = s.Poll(context.Background()) }

func TestFailedAttemptsResetWhenThePartitionChanges(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	c.torrents = []qbt.Torrent{stalled(hashA, 0, 0)}
	c.deleteError = errors.New("delete unavailable")
	poll(t, s)
	clock.Advance(20 * time.Second)
	pollIgnoringError(s)
	if s.state.Tracked[hashA].Attempts == 0 || s.state.Tracked[hashA].DeleteRequestedAt != nil {
		t.Fatal("expected a consumed but unconfirmed attempt", s.state.Tracked[hashA])
	}

	// Moving to another partition is a new decision and gets a fresh, finite
	// budget; it still has to re-prove the whole threshold first.
	c.torrents[0].NumSeeds = 1
	clock.Advance(time.Second)
	poll(t, s)
	e := s.state.Tracked[hashA]
	if e.Policy != config.StalledSeedersSeen || e.Attempts != 0 || !e.FirstSeen.Equal(clock.Now()) {
		t.Fatal("partition change did not reset the attempt budget", e)
	}
	if len(c.deletes) != 1 {
		t.Fatal("fresh budget acted before its own threshold", c.deletes)
	}
}

func TestPerPolicyThresholdsAreHonouredIndependently(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	s.c.Policies[config.Metadata] = config.Policy{Action: config.Delete, Threshold: 10 * time.Second}
	s.c.Policies[config.StalledNoSeeders] = config.Policy{Action: config.Delete, Threshold: 30 * time.Second}
	s.c.Policies[config.StalledPartial] = config.Policy{Action: config.DeleteFile, Threshold: 50 * time.Second}
	// The fake qBittorrent never makes a deleted torrent disappear, so pin the
	// confirmation window open: this test is about thresholds, not retries.
	s.c.DeleteConfirmationTimeout = time.Hour
	c.torrents = []qbt.Torrent{torrent(hashA), stalled(hashB, 0, 0), stalled(hashC, .5, 0)}
	poll(t, s)
	for _, step := range []struct {
		advance time.Duration
		want    []string
	}{
		{9 * time.Second, nil},
		{time.Second, []string{hashA}},
		{19 * time.Second, []string{hashA}},
		{time.Second, []string{hashA, hashB}},
		{19 * time.Second, []string{hashA, hashB}},
		{time.Second, []string{hashA, hashB, hashC}},
	} {
		clock.Advance(step.advance)
		poll(t, s)
		if len(c.deletes) != len(step.want) {
			t.Fatal("a policy acted on another policy's clock", c.deletes, "want", step.want)
		}
	}
	// Only the partial-progress policy was configured to remove payload.
	if c.files[0] || c.files[1] || !c.files[2] {
		t.Fatal("delete_files flag not per policy", c.files)
	}
}

func TestStalledPartialActsOnPayloadButExclusionsStillProtect(t *testing.T) {
	t.Run("acts despite progress and payload", func(t *testing.T) {
		s, c, clock, _ := fixture(t)
		s.c.DryRun = false
		s.c.Policies[config.StalledPartial] = config.Policy{Action: config.DeleteFile, Threshold: 20 * time.Second}
		c.torrents = []qbt.Torrent{stalled(hashA, .75, 0)}
		c.torrents[0].Downloaded = 8 << 20
		poll(t, s)
		clock.Advance(20 * time.Second)
		poll(t, s)
		if len(c.deletes) != 1 || !c.files[0] {
			t.Fatal("partial policy blocked by its own progress", c.deletes, c.files)
		}
	})
	for _, exclusion := range []string{"tag", "category", "include"} {
		t.Run("protected by "+exclusion, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.DryRun = false
			s.c.Policies[config.StalledPartial] = config.Policy{Action: config.DeleteFile, Threshold: 20 * time.Second}
			c.torrents = []qbt.Torrent{stalled(hashA, .75, 0)}
			switch exclusion {
			case "tag":
				c.torrents[0].Tags = "misc, keep"
			case "category":
				s.c.ExcludeCategories = []string{"archive"}
				c.torrents[0].Category = "archive"
			case "include":
				s.c.IncludeCategories = []string{"allowed"}
				c.torrents[0].Category = "other"
			}
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
			if len(c.deletes) != 0 || len(s.state.Tracked) != 0 {
				t.Fatal("exclusion did not protect a partial torrent", c.deletes)
			}
			if s.Snapshot().Torrents[0].Decision != "protected" {
				t.Fatal("protection not visible", s.Snapshot().Torrents[0].Decision)
			}
		})
	}
}

func TestRejectedGenerationNeverActsAndKeepsLastKnownGood(t *testing.T) {
	for _, rejection := range []string{"restart only", "client rebuild"} {
		t.Run(rejection, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.DryRun = false
			s.c.Policies[config.Metadata] = config.Policy{Action: config.Delete, Threshold: 20 * time.Second}
			poll(t, s)
			generation := s.Snapshot().ConfigStatus.Generation

			// The candidate would both remove payload and fire immediately.
			next := s.Config()
			next.DryRun = false
			next.Policies[config.Metadata] = config.Policy{Action: config.DeleteFile, Threshold: time.Nanosecond}
			prepare := func(config.Config) (Client, error) { return c, nil }
			if rejection == "restart only" {
				next.Listen = ":9999"
			} else {
				prepare = func(config.Config) (Client, error) { return nil, errors.New("invalid client TLS configuration") }
			}
			err := s.Reload(next, prepare)
			if err == nil {
				t.Fatal("invalid generation accepted")
			}
			s.ReportReload(err)

			if s.Config().Policies[config.Metadata].Action == config.DeleteFile {
				t.Fatal("rejected generation became the running configuration")
			}
			status := s.Snapshot().ConfigStatus
			if status.Generation != generation || status.Healthy() || status.LastReloadError == "" {
				t.Fatal("rejected generation not reported", status)
			}
			if ready, _ := Ready(s.Snapshot(), clock.Now(), time.Hour); ready {
				t.Fatal("rejected reload left the process ready")
			}

			// The last known good settings, not the rejected ones, decide.
			clock.Advance(20 * time.Second)
			poll(t, s)
			if len(c.deletes) != 1 || c.files[0] {
				t.Fatal("acted on a generation that failed validation", c.deletes, c.files)
			}
		})
	}
}

func TestLoweredThresholdAndEscalatedActionRequireAFreshEpisode(t *testing.T) {
	for _, change := range []string{"lowered threshold", "warn to delete_file", "dry run disabled"} {
		t.Run(change, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			s.c.DryRun = change != "dry run disabled"
			s.c.Policies[config.Metadata] = config.Policy{Action: config.Warn, Threshold: 60 * time.Second}
			if change == "lowered threshold" {
				s.c.DryRun = false
				s.c.Policies[config.Metadata] = config.Policy{Action: config.Delete, Threshold: 60 * time.Second}
			}
			poll(t, s)
			clock.Advance(59 * time.Second)
			poll(t, s)
			if len(c.deletes) != 0 {
				t.Fatal("acted before the original threshold")
			}

			next := s.c.Clone()
			switch change {
			case "lowered threshold":
				next.Policies[config.Metadata] = config.Policy{Action: config.Delete, Threshold: 20 * time.Second}
			case "warn to delete_file":
				next.DryRun = false
				next.Policies[config.Metadata] = config.Policy{Action: config.DeleteFile, Threshold: 20 * time.Second}
			case "dry run disabled":
				next.DryRun = false
				next.Policies[config.Metadata] = config.Policy{Action: config.Delete, Threshold: 20 * time.Second}
			}
			if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err != nil {
				t.Fatal(err)
			}

			// 59 seconds of accrued time must not satisfy the new 20-second
			// threshold: the clock restarted with the new settings.
			poll(t, s)
			clock.Advance(19 * time.Second)
			poll(t, s)
			if len(c.deletes) != 0 {
				t.Fatal("reload escalated using time accrued under the old settings", c.deletes)
			}
			clock.Advance(time.Second)
			poll(t, s)
			if len(c.deletes) != 1 {
				t.Fatal("new settings never took effect", c.deletes)
			}
		})
	}
}

func TestReloadHealthHasASingleSource(t *testing.T) {
	s, c, clock, _ := fixture(t)
	poll(t, s)
	manager := s.ConfigManager()
	if manager == nil || manager.Status() != s.Snapshot().ConfigStatus {
		t.Fatal("snapshot reload health diverged from the manager")
	}

	next := s.Config()
	next.UIRefreshInterval = 11 * time.Second
	if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	if manager.Status().Generation != 2 || s.Snapshot().ConfigStatus != manager.Status() {
		t.Fatal("accepted reload not reflected once", manager.Status(), s.Snapshot().ConfigStatus)
	}

	broken := s.Config()
	broken.UIRefreshInterval = 12 * time.Second
	failure := errors.New("invalid client TLS configuration")
	if err := s.Reload(broken, func(config.Config) (Client, error) { return nil, failure }); err == nil {
		t.Fatal("broken reload accepted")
	}
	status := s.Snapshot().ConfigStatus
	if status != manager.Status() || status.Healthy() || status.Generation != 2 {
		t.Fatal("failed reload reported inconsistently", status, manager.Status())
	}

	// Recovery is visible everywhere at once, because there is only one copy.
	recovered := s.Config()
	recovered.UIRefreshInterval = 13 * time.Second
	if err := s.Reload(recovered, func(config.Config) (Client, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	if !s.Snapshot().ConfigStatus.Healthy() || s.Snapshot().ConfigStatus != manager.Status() {
		t.Fatal("recovery not reflected once", s.Snapshot().ConfigStatus)
	}
	// A reload clears the last successful poll, so readiness returns only
	// after the process has proven itself against the new configuration.
	if ready, _ := Ready(s.Snapshot(), clock.Now(), time.Hour); ready {
		t.Fatal("ready before polling the new configuration")
	}
	poll(t, s)
	if ready, reason := Ready(s.Snapshot(), clock.Now(), time.Hour); !ready {
		t.Fatal("recovered process not ready", reason)
	}
}

func TestTrackedStateAndHistoryStayBounded(t *testing.T) {
	s, c, clock, disk := fixture(t)
	s.c.HistoryLimit = 3
	c.torrents = []qbt.Torrent{torrent(hashA), stalled(hashB, 0, 1), stalled(hashC, .5, 0)}
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	if len(s.state.Tracked) != 3 || len(s.state.SeedObserved) != 1 {
		t.Fatal("unexpected tracking", len(s.state.Tracked), len(s.state.SeedObserved))
	}

	// Every torrent warns once per episode, and repeated episodes must never
	// grow history past the configured limit.
	for range 10 {
		c.torrents[0].State = "downloading"
		poll(t, s)
		c.torrents[0].State = "metaDL"
		poll(t, s)
		clock.Advance(20 * time.Second)
		poll(t, s)
	}
	if len(s.state.History) != 3 || len(disk.state.History) != 3 {
		t.Fatal("history exceeded its limit", len(s.state.History), len(disk.state.History))
	}

	// Entries exist only for torrents qBittorrent still reports.
	c.torrents = []qbt.Torrent{torrent(hashA)}
	poll(t, s)
	if len(s.state.Tracked) != 1 || len(s.state.SeedObserved) != 0 {
		t.Fatal("state retained departed torrents", s.state.Tracked, s.state.SeedObserved)
	}
	c.torrents = nil
	poll(t, s)
	if len(s.state.Tracked) != 0 || len(disk.state.Tracked) != 0 {
		t.Fatal("state not bounded by presence", s.state.Tracked)
	}
}
