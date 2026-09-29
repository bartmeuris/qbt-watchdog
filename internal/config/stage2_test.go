package config

import (
	"testing"
	"time"
)

func TestIdenticalCandidateSkipsCommitWithoutAdvancingTheGeneration(t *testing.T) {
	m, _ := manager(t, minimal)
	commits := 0
	for range 5 {
		if err := m.Reload(m.Current(), func(Config) error { commits++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if commits != 0 {
		t.Fatal("identical candidate reached commit", commits)
	}
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

func TestIdenticalCandidateClearsReloadErrorWithoutCommit(t *testing.T) {
	m, _ := manager(t, minimal)
	failure := testingError("reload failed")
	m.Record(failure)
	commits := 0
	if err := m.Reload(m.Current(), func(Config) error { commits++; return nil }); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Fatal("unchanged recovery reached commit", commits)
	}
	if status := m.Status(); status.Generation != 1 || !status.Healthy() || status.LastReloadAt.IsZero() {
		t.Fatal("unchanged recovery did not clear reload error", status)
	}
}

type testingError string

func (e testingError) Error() string { return string(e) }
