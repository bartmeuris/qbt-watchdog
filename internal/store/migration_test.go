package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
)

// legacy builds an on-disk record in an older layout, using the field names the
// current struct still carries so the fixture stays honest about the wire form.
func legacy(version int, now time.Time, withSeeds bool) map[string]any {
	requested := now.Add(-time.Minute)
	record := map[string]any{
		"schema_version": version,
		"safety_key":     "stale-key-from-a-previous-policy",
		"endpoint_key":   "endpoint",
		"counters":       Counters{Deletions: 7, WouldDeletions: 5, DeleteRequests: 9},
		"tracked": map[string]any{
			hash: map[string]any{
				"policy":               string(config.StalledNoSeeders),
				"first_seen_meta":      now.Add(-99 * time.Hour),
				"last_seen_meta":       now.Add(-98 * time.Hour),
				"dry_run_notified":     true,
				"delete_requested_at":  requested,
				"delete_attempts":      2,
				"unused_legacy_column": nil,
			},
		},
		"history": []map[string]any{{
			"time": now.Add(-time.Hour), "action": "delete_requested",
			"short_hash": "0123456789ab", "outcome": "accepted",
		}},
	}
	if withSeeds {
		record["seed_observed"] = map[string]bool{hash: true}
	}
	return record
}

func write(t *testing.T, value any) File {
	t.Helper()
	f := File{Path: filepath.Join(t.TempDir(), "state.json"), HistoryLimit: 10}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMigrationFromOlderSchemasResetsRatherThanTrusts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, version := range []int{1, 2} {
		t.Run("schema", func(t *testing.T) {
			f := write(t, legacy(version, now, version >= 2))
			s, err := f.Load(now)
			if err != nil {
				t.Fatal("readable legacy record rejected", err)
			}
			if s.SchemaVersion != SchemaVersion {
				t.Fatal("schema not upgraded", s.SchemaVersion)
			}
			e := s.Tracked[hash]
			// Re-proven from scratch: the partition, the episode clock and the
			// per-episode notification marker may not be inherited.
			if e.Policy != "" || e.DryRunNotified || !e.FirstSeen.Equal(now) || !e.LastSeen.Equal(now) {
				t.Fatal("migrated episode trusted stale fields", e)
			}
			// Never lost: the finite attempt budget and the in-flight request.
			if e.Attempts != 2 || e.DeleteRequestedAt == nil {
				t.Fatal("migration dropped safety state", e)
			}
			if s.Counters.Deletions != 7 || s.Counters.DeleteRequests != 9 {
				t.Fatal("migration lost lifetime counters", s.Counters)
			}
			// The old event vocabulary is dropped, not relabelled.
			if len(s.History) != 0 {
				t.Fatal("legacy history retained", s.History)
			}
			// An empty safety key makes the service reset its timers too.
			if s.SafetyKey != "" {
				t.Fatal("stale safety key trusted")
			}
			if s.SeedObserved == nil {
				t.Fatal("seeder map missing after migration")
			}
			if version == 1 && len(s.SeedObserved) != 0 {
				t.Fatal("seeders inferred for a schema that never recorded them")
			}
			if version == 2 && !s.SeedObserved[hash] {
				t.Fatal("identically defined seeder observation discarded")
			}
			if err = f.Save(s); err != nil {
				t.Fatal(err)
			}
			again, err := f.Load(now)
			if err != nil || again.SchemaVersion != SchemaVersion || len(again.History) != 0 {
				t.Fatal("migrated state does not round-trip", again, err)
			}
		})
	}
}

func TestUnknownAndFutureSchemasAreNeverTrusted(t *testing.T) {
	now := time.Now().UTC()
	for _, version := range []int{0, SchemaVersion + 1, 99} {
		f := write(t, legacy(version, now, true))
		s, err := f.Load(now)
		if err == nil || len(s.Tracked) != 0 {
			t.Fatal("unreadable schema trusted", version, s, err)
		}
	}
	// A legacy record that lacks the seeder map it was defined to carry is
	// corrupt rather than migratable.
	f := write(t, legacy(2, now, false))
	if s, err := f.Load(now); err == nil || len(s.Tracked) != 0 {
		t.Fatal("incomplete schema 2 record accepted", s, err)
	}
}

func TestCurrentSchemaRejectsUnknownActionVocabulary(t *testing.T) {
	now := time.Now().UTC()
	s := Empty()
	s.History = []Event{{Time: now, Action: "delete_requested", Outcome: "accepted"}}
	f := write(t, s)
	if loaded, err := f.Load(now); err == nil || len(loaded.History) != 0 {
		t.Fatal("stale vocabulary accepted at the current schema", loaded, err)
	}
	for _, action := range []string{"warn", "action_requested", "action_confirmed", "action_skipped", "action_failed"} {
		s := Empty()
		s.History = []Event{{Time: now, Action: action, Outcome: "success", Policy: config.StalledPartial, EffectiveAction: config.DeleteFile}}
		f := write(t, s)
		loaded, err := f.Load(now)
		if err != nil || len(loaded.History) != 1 {
			t.Fatal("current vocabulary rejected", action, err)
		}
	}
}

func TestHistoryStaysBoundedOnLoadForEveryLimit(t *testing.T) {
	now := time.Now().UTC()
	for _, limit := range []int{1, 10, 100} {
		s := Empty()
		for range 500 {
			s.History = append(s.History, Event{Time: now, Action: "warn", Outcome: "success"})
		}
		f := write(t, s)
		f.HistoryLimit = limit
		loaded, err := f.Load(now)
		if err != nil || len(loaded.History) != limit {
			t.Fatal("history not bounded on load", limit, len(loaded.History), err)
		}
	}
}
