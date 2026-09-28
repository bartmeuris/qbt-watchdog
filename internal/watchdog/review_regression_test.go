package watchdog

import (
	"context"
	"errors"
	"qbt-watchdog/internal/config"
	"slices"
	"testing"
	"time"

	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

type targetedErrorClient struct {
	*fakeClient
	failHash string
}

func (c *targetedErrorClient) Get(ctx context.Context, hash string) (*qbt.Torrent, error) {
	if hash == c.failHash {
		return nil, errors.New("targeted read unavailable")
	}
	return c.fakeClient.Get(ctx, hash)
}

func TestEpisodeExitSurvivesLaterPreflightFailure(t *testing.T) {
	for _, observation := range []string{"list exit", "list disappearance", "targeted exit", "targeted disappearance"} {
		t.Run(observation, func(t *testing.T) {
			s, c, clock, disk := fixture(t)
			s.c.DryRun = false
			s.c.MaxDeletions = 2
			c.torrents = []qbt.Torrent{torrent(hashA), torrent(hashB)}
			poll(t, s)
			clock.Advance(s.c.Policies[config.Metadata].Threshold)
			switch observation {
			case "list exit":
				c.torrents[0].State = "downloading"
			case "list disappearance":
				c.torrents = c.torrents[1:]
			case "targeted exit":
				c.fresh = func(hash string) *qbt.Torrent {
					fresh := torrent(hash)
					fresh.State = "downloading"
					return &fresh
				}
			case "targeted disappearance":
				c.fresh = func(string) *qbt.Torrent { return nil }
			}
			s.client = &targetedErrorClient{fakeClient: c, failHash: hashB}
			if s.Poll(context.Background()) == nil {
				t.Fatal("expected later preflight failure")
			}
			if len(c.deletes) != 0 {
				t.Fatal("failed preflight allowed an action")
			}
			if _, exists := s.state.Tracked[hashA]; exists {
				t.Fatal("negative observation discarded in memory")
			}
			if _, exists := disk.state.Tracked[hashA]; exists {
				t.Fatal("negative observation not persisted")
			}
			s.client = c
			c.fresh = nil
			c.torrents = []qbt.Torrent{torrent(hashA)}
			clock.Advance(time.Second)
			reentry := clock.Now()
			poll(t, s)
			if !s.state.Tracked[hashA].FirstSeen.Equal(reentry) {
				t.Fatal("reentry reused ended episode")
			}
			clock.Advance(s.c.Policies[config.Metadata].Threshold - time.Second)
			poll(t, s)
			if len(c.deletes) != 0 {
				t.Fatal("reentry did not require full timeout")
			}
			clock.Advance(time.Second)
			poll(t, s)
			if !slices.Equal(c.deletes, []string{hashA}) {
				t.Fatal("new full episode did not allow deletion", c.deletes)
			}
		})
	}
}

type saveHookStore struct {
	*memoryStore
	onSave func(store.State)
}

func (m *saveHookStore) Save(state store.State) error {
	if m.onSave != nil {
		m.onSave(state)
	}
	return m.memoryStore.Save(state)
}

func TestFinalSafetyCheckFollowsDurableReservation(t *testing.T) {
	for _, change := range []string{"save gap", "save state change", "final read gap"} {
		t.Run(change, func(t *testing.T) {
			s, c, clock, disk := fixture(t)
			s.c.DryRun = false
			poll(t, s)
			clock.Advance(s.c.Policies[config.Metadata].Threshold)
			hooked := &saveHookStore{memoryStore: disk}
			s.disk = hooked
			hooked.onSave = func(state store.State) {
				hooked.onSave = nil
				if state.Tracked[hashA].Attempts != 1 || len(c.gets) != 1 {
					t.Fatal("reservation must precede final targeted read")
				}
				switch change {
				case "save gap":
					clock.Advance(s.c.MaxObservationGap + time.Second)
				case "save state change":
					c.torrents[0].State = "downloading"
				}
			}
			c.fresh = func(string) *qbt.Torrent {
				if len(c.gets) == 2 {
					if disk.state.Tracked[hashA].Attempts != 1 {
						t.Fatal("final GET preceded durable reservation")
					}
					if change == "final read gap" {
						clock.Advance(s.c.MaxObservationGap + time.Second)
					}
				}
				fresh := c.torrents[0]
				return &fresh
			}
			poll(t, s)
			if len(c.gets) != 2 || len(c.deletes) != 0 {
				t.Fatal("stale final safety check allowed deletion", c.gets, c.deletes)
			}
			if s.state.Tracked[hashA].Attempts != 0 || disk.state.Tracked[hashA].Attempts != 0 {
				t.Fatal("known-unsent reservation retained")
			}
		})
	}
}

func TestFailedReservationsDoNotExhaustRequestBudget(t *testing.T) {
	s, c, clock, disk := fixture(t)
	s.c.DryRun = false
	poll(t, s)
	disk.err = errors.New("disk unavailable")
	for range store.MaxAttempts + 2 {
		clock.Advance(s.c.Policies[config.Metadata].Threshold)
		poll(t, s)
		if len(c.deletes) != 0 || s.state.Tracked[hashA].Attempts != 0 {
			t.Fatal("failed save consumed request budget")
		}
		if s.Snapshot().PersistenceError == "" {
			t.Fatal("failed save not reported")
		}
	}
	disk.err = nil
	poll(t, s)
	if len(c.deletes) != 1 || disk.state.Tracked[hashA].Attempts != 1 {
		t.Fatal("persistence recovery did not allow actual request")
	}
	for range store.MaxAttempts + 2 {
		clock.Advance(s.c.DeleteConfirmationTimeout)
		poll(t, s)
	}
	if len(c.deletes) != store.MaxAttempts || disk.state.Tracked[hashA].Attempts != store.MaxAttempts {
		t.Fatal("actual requests exceeded finite budget", c.deletes)
	}
}
