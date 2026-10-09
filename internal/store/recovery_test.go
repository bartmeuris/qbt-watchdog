package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
)

func TestSchemaThreePreservesAllExistingState(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	s := Empty()
	s.SchemaVersion = 3
	s.RecoveryJobs = nil
	s.SafetyKey, s.EndpointKey = "safety", "endpoint"
	for i, policy := range config.PolicyIDs() {
		hash := strings.Repeat(string(rune('a'+i)), 40)
		accepted := now.Add(-time.Duration(i+1) * time.Minute)
		s.Tracked[hash] = Episode{
			Policy: policy, FirstSeen: now.Add(-time.Duration(i+1) * time.Hour),
			LastSeen: now.Add(-time.Duration(i) * time.Second), DryRunNotified: i%2 == 0,
			DeleteRequestedAt: &accepted, Attempts: min(i+1, MaxAttempts),
		}
		s.SeedObserved[hash] = i%2 == 0
	}
	s.Tracked[strings.Repeat("e", 40)] = Episode{Policy: config.Metadata, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now, Attempts: MaxAttempts}
	s.SeedObserved[strings.Repeat("f", 40)] = true
	s.Counters = Counters{Deletions: 8, WouldDeletions: 9, DeleteRequests: 10}
	s.History = []Event{
		{Time: now.Add(-4 * time.Minute), Action: "warn", Policy: config.Metadata, EffectiveAction: config.Warn, Outcome: "success", ShortHash: "aaaaaaaaaaaa", Name: "warning", DryRun: true},
		{Time: now.Add(-3 * time.Minute), Action: "action_requested", Policy: config.StalledPartial, EffectiveAction: config.DeleteFile, Outcome: "accepted", ShortHash: "bbbbbbbbbbbb", Name: "pending"},
		{Time: now.Add(-2 * time.Minute), Action: "action_confirmed", Policy: config.StalledNoSeeders, EffectiveAction: config.Delete, Outcome: "success", ShortHash: "cccccccccccc", Name: "confirmed"},
		{Time: now.Add(-time.Minute), Action: "action_skipped", Policy: config.StalledSeedersSeen, EffectiveAction: config.Delete, Outcome: "skipped", ShortHash: "dddddddddddd", Name: "skipped", Error: "protected"},
		{Time: now, Action: "action_failed", Policy: config.StalledPartial, EffectiveAction: config.DeleteFile, Outcome: "failed", ShortHash: "eeeeeeeeeeee", Name: "failed", Error: "request failed"},
	}
	if !valid(s, now) {
		t.Fatal("invalid schema 3 fixture")
	}
	file := File{Path: filepath.Join(t.TempDir(), "state.json"), HistoryLimit: 100}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := file.Load(now)
	if err != nil {
		t.Fatal(err)
	}
	s.SchemaVersion = 4
	s.RecoveryJobs = map[string]RecoveryJob{}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("schema 3 migration changed state:\ngot: %#v\nwant: %#v", got, s)
	}
	if !valid(got, now) {
		t.Fatal("migration produced invalid schema 4 state")
	}
	if err := file.Save(got); err != nil {
		t.Fatal(err)
	}
	roundtrip, err := file.Load(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !valid(roundtrip, now.Add(time.Hour)) || !reflect.DeepEqual(roundtrip, s) {
		t.Fatalf("schema 4 roundtrip changed migrated state:\ngot: %#v\nwant: %#v", roundtrip, s)
	}
}

func TestRecoveryJobNewFieldsRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	s := Empty()
	job := RecoveryJob{
		ID: strings.Repeat("a", 64), Kind: config.Sonarr,
		Endpoint: strings.Repeat("b", 64), Hash: strings.Repeat("c", 40),
		Name: "The Example Episode", Policy: config.Metadata, Action: config.Delete,
		EpisodeAt: now.Add(-2 * time.Hour), CapturedAt: now.Add(-time.Hour),
		AcceptedAt: now.Add(-50 * time.Minute), DeletedAt: now.Add(-30 * time.Minute), ExpiresAt: now.Add(23 * time.Hour),
		Mode: config.BlocklistOnly, Stage: CommandPending, CommandID: 44,
		QueueIDs: []int64{1}, MediaIDs: []int64{11},
	}
	s.RecoveryJobs[job.ID] = job
	f := write(t, s)
	loaded, err := f.Load(now)
	if err != nil {
		t.Fatal("recovery job with new fields rejected", err)
	}
	got := loaded.RecoveryJobs[job.ID]
	if got.Name != "The Example Episode" || got.Mode != config.BlocklistOnly || !RecoveryCode("blocklist_completed") {
		t.Fatalf("recovery job fields did not round-trip: %+v", got)
	}
}

func TestRecoveryJobBlocklistFieldsRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	s := Empty()
	job := RecoveryJob{
		ID: strings.Repeat("a", 64), Kind: config.Sonarr,
		Endpoint: strings.Repeat("b", 64), Hash: strings.Repeat("c", 40),
		Name: "The Example Episode", Policy: config.Metadata, Action: config.Delete,
		EpisodeAt: now.Add(-2 * time.Hour), CapturedAt: now.Add(-time.Hour),
		AcceptedAt: now.Add(-50 * time.Minute), DeletedAt: now.Add(-30 * time.Minute), ExpiresAt: now.Add(23 * time.Hour),
		Mode: config.BlocklistAndSearch, Stage: Resolving,
		QueueIDs: []int64{1}, MediaIDs: []int64{11},
		BlocklistAt: now.Add(-45 * time.Minute), BlocklistCode: "not_found",
	}
	s.RecoveryJobs[job.ID] = job
	f := write(t, s)
	loaded, err := f.Load(now)
	if err != nil {
		t.Fatal("recovery job with blocklist fields rejected", err)
	}
	got := loaded.RecoveryJobs[job.ID]
	if !got.BlocklistAt.Equal(job.BlocklistAt) || got.BlocklistCode != "not_found" {
		t.Fatalf("blocklist fields did not round-trip: %+v", got)
	}
}
