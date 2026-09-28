package config

import (
	"testing"
	"time"
)

func TestIdenticalCandidateCommitsWithoutAdvancingTheGeneration(t *testing.T) {
	m, _ := manager(t, minimal)
	commits := 0
	for range 5 {
		if err := m.Reload(m.Current(), func(Config) error { commits++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// The committer still sees every attempt, because only it knows whether
	// its own view has drifted from the file.
	if commits != 5 {
		t.Fatal("identical candidate never reached commit", commits)
	}
	// But an unchanged file must not make the generation counter meaningless.
	if status := m.Status(); status.Generation != 1 || !status.Healthy() || status.LastReloadAt.IsZero() {
		t.Fatal("unchanged reload advanced the generation", status)
	}

	changed := m.Current()
	changed.PollInterval = 42 * time.Second
	if err := m.Reload(changed, func(Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if m.Status().Generation != 2 {
		t.Fatal("a real change did not advance the generation", m.Status())
	}
}
