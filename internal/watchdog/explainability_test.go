package watchdog

import (
	"errors"
	"slices"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

// rowFor finds the published row for a hash, so assertions never depend on the
// snapshot's overdue-first ordering.
func rowFor(t *testing.T, s *Service, hash string) Row {
	t.Helper()
	short := qbt.ShortHash(hash)
	for _, row := range s.Snapshot().Torrents {
		if row.ShortHash == short {
			return row
		}
	}
	t.Fatal("no published row for", short)
	return Row{}
}

func TestRowCarriesConfiguredAndEffectiveActionFromTheSamePolicyModel(t *testing.T) {
	s, c, _, _ := fixture(t)
	c.torrents = []qbt.Torrent{torrent(hashA)}
	poll(t, s)

	row := rowFor(t, s, hashA)
	if row.Policy != config.Metadata {
		t.Fatal("unexpected classification", row.Policy)
	}
	// dry_run is an override, so the row must show both what the operator
	// configured and what the engine may actually do.
	if row.ConfiguredAction != config.Delete || row.EffectiveAction != config.Warn {
		t.Fatal("dry-run override not visible per row", row.ConfiguredAction, row.EffectiveAction)
	}

	// The row and the per-policy summary have exactly one source between them.
	view := slices.IndexFunc(s.Snapshot().Policies, func(p PolicyView) bool { return p.Policy == row.Policy })
	if p := s.Snapshot().Policies[view]; p.Action != row.ConfiguredAction ||
		p.EffectiveAction != row.EffectiveAction || p.ThresholdSeconds != row.ThresholdSeconds {
		t.Fatal("row disagrees with the published policy model", p, row)
	}

	// Without the override the two agree, which is what makes a difference
	// between them meaningful.
	s.c.DryRun = false
	poll(t, s)
	if row := rowFor(t, s, hashA); row.ConfiguredAction != config.Delete || row.EffectiveAction != config.Delete {
		t.Fatal("actions diverged without an override", row.ConfiguredAction, row.EffectiveAction)
	}
}

func TestUnclassifiedRowCarriesNoClockThresholdOrAction(t *testing.T) {
	s, c, clock, _ := fixture(t)
	c.torrents = []qbt.Torrent{{Hash: hashB, Name: "ordinary download", State: "downloading"}}
	poll(t, s)
	clock.Advance(time.Hour)
	poll(t, s)

	row := rowFor(t, s, hashB)
	if row.Decision != DecisionNotApplicable || row.Policy != "" {
		t.Fatal("a normal download was classified", row.Policy, row.Decision)
	}
	// No hypothetical partition is previewed, so there is nothing to count
	// down and no action to advertise.
	if row.FirstSeen != nil || row.Elapsed != 0 || row.Remaining != 0 || row.ThresholdSeconds != 0 {
		t.Fatal("unclassified row published a timer", row)
	}
	if row.ConfiguredAction != "" || row.EffectiveAction != "" {
		t.Fatal("unclassified row published an action", row.ConfiguredAction, row.EffectiveAction)
	}
}

func TestOverdueAndEligibleInAreDrivenBySignOfRemaining(t *testing.T) {
	s, c, clock, _ := fixture(t)
	c.torrents = []qbt.Torrent{torrent(hashA)}
	poll(t, s)

	clock.Advance(5 * time.Second)
	poll(t, s)
	row := rowFor(t, s, hashA)
	if row.Elapsed != 5 || row.Remaining != 15 || s.Snapshot().Summary.Overdue != 0 {
		t.Fatal("counting-down row misreported", row.Elapsed, row.Remaining)
	}

	clock.Advance(25 * time.Second)
	poll(t, s)
	row = rowFor(t, s, hashA)
	if row.Elapsed != 30 || row.Remaining != -10 || s.Snapshot().Summary.Overdue != 1 {
		t.Fatal("overdue row misreported", row.Elapsed, row.Remaining)
	}
}

func TestEveryBlockedRowNamesWhatPreventsAction(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		run  func(*testing.T, *Service, *fakeClient, *fakeClock)
	}{
		{name: "outside every policy", want: DecisionNotApplicable, run: func(t *testing.T, s *Service, c *fakeClient, _ *fakeClock) {
			c.torrents = []qbt.Torrent{{Hash: hashA, Name: "ordinary", State: "downloading"}}
			poll(t, s)
		}},
		{name: "counting down", want: DecisionTracking, run: func(t *testing.T, s *Service, c *fakeClient, _ *fakeClock) {
			poll(t, s)
		}},
		{name: "threshold met", want: DecisionEligible, run: func(t *testing.T, s *Service, c *fakeClient, clock *fakeClock) {
			poll(t, s)
			// Publish between polls: the threshold is met but the engine has
			// not yet had its turn to act.
			clock.Advance(20 * time.Second)
			s.publish(nil)
		}},
		{name: "excluded by tag", want: DecisionProtected, run: func(t *testing.T, s *Service, c *fakeClient, _ *fakeClock) {
			c.torrents = []qbt.Torrent{stalled(hashA, 0, 0)}
			c.torrents[0].Tags = "misc, keep"
			poll(t, s)
		}},
		{name: "metadata with progress", want: DecisionNonzeroProgress, run: func(t *testing.T, s *Service, c *fakeClient, _ *fakeClock) {
			c.torrents = []qbt.Torrent{torrent(hashA)}
			c.torrents[0].Progress = .5
			poll(t, s)
		}},
		{name: "zero-progress policy with payload", want: DecisionNonzeroDownloaded, run: func(t *testing.T, s *Service, c *fakeClient, _ *fakeClock) {
			c.torrents = []qbt.Torrent{stalled(hashA, 0, 0)}
			c.torrents[0].Downloaded = 4096
			poll(t, s)
		}},
		{name: "actions disabled", want: DecisionActionsDisabled, run: func(t *testing.T, s *Service, c *fakeClient, clock *fakeClock) {
			s.c.MaxDeletions = 0
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
		}},
		{name: "already warned", want: DecisionWarned, run: func(t *testing.T, s *Service, c *fakeClient, clock *fakeClock) {
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
		}},
		{name: "retry budget exhausted", want: DecisionRetryLimitReached, run: func(t *testing.T, s *Service, c *fakeClient, clock *fakeClock) {
			s.c.DryRun = false
			c.deleteError = errors.New("delete unavailable")
			poll(t, s)
			clock.Advance(20 * time.Second)
			for range store.MaxAttempts {
				pollIgnoringError(s)
			}
		}},
		{name: "delete awaiting confirmation", want: DecisionDeleteRequested, run: func(t *testing.T, s *Service, c *fakeClient, clock *fakeClock) {
			s.c.DryRun = false
			s.c.DeleteConfirmationTimeout = time.Hour
			poll(t, s)
			clock.Advance(20 * time.Second)
			poll(t, s)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, clock, _ := fixture(t)
			tc.run(t, s, c, clock)
			if got := rowFor(t, s, hashA).Decision; got != tc.want {
				t.Fatal("wrong explanation", got, "want", tc.want)
			}
		})
	}
}

func TestCompletedNoDataPayloadPresentDecision(t *testing.T) {
	s, _, clock, _ := fixture(t)
	now := clock.Now()
	s.state.Tracked[hashA] = store.Episode{Policy: config.CompletedNoData, FirstSeen: now, LastSeen: now}
	s.torrents = []qbt.Torrent{{Hash: hashA, Name: "payload appeared", State: "uploading", Size: 1, TotalSize: 1024, AmountLeft: 0, Downloaded: 0}}
	s.publish(nil)

	if got := rowFor(t, s, hashA).Decision; got != DecisionPayloadPresent {
		t.Fatal("wrong explanation", got)
	}
}

// TestDecisionVocabularyIsClosed keeps Decisions honest: it is the list the
// user interface is checked against, so an unlisted decision would reach an
// operator with no explanation at all.
func TestDecisionVocabularyIsClosed(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Decisions() {
		if seen[d] {
			t.Fatal("duplicate decision", d)
		}
		seen[d] = true
	}
	if len(seen) != 11 {
		t.Fatal("decision vocabulary changed without updating its consumers", len(seen))
	}
}
