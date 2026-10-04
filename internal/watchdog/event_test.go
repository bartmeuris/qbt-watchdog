package watchdog

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

func TestAuditBoundsDoNotTruncateLiveTorrentNames(t *testing.T) {
	s, client, clock, disk := fixture(t)
	name := strings.Repeat("界<\x00", 1000000)
	client.torrents[0].Name = name
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	v := s.Snapshot()
	if len(v.History) != 1 || len(disk.state.History) != 1 {
		t.Fatal("audit not created and persisted")
	}
	for _, e := range []store.Event{v.History[0], disk.state.History[0]} {
		if len(e.Name) > store.EventTextLimit || !utf8.ValidString(e.Name) || !strings.HasPrefix(name, e.Name) {
			t.Fatal("hostile name not bounded")
		}
	}
	if v.Torrents[0].Name != name || client.torrents[0].Name != name {
		t.Fatal("live torrent name changed")
	}
	s.event("action_failed", "failed", client.torrents[0], strings.Repeat("界", 1000000))
	e := s.state.History[len(s.state.History)-1]
	if len(e.Error) > store.EventTextLimit || !utf8.ValidString(e.Error) {
		t.Fatal("audit error not bounded")
	}
}

// requestDeletion drives one torrent through the threshold to a durable delete
// request, with dry run disabled so the request actually happens.
func requestDeletion(t *testing.T, s *Service, c *fakeClient, clock *fakeClock, name string) {
	t.Helper()
	s.c.DryRun = false
	c.torrents[0].Name = name
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	if len(c.deletes) != 1 {
		t.Fatal("delete was not requested")
	}
}

func TestDeletionRequestAndConfirmationKeepTorrentName(t *testing.T) {
	s, c, clock, _ := fixture(t)
	const name = "The Torrent Name"
	requestDeletion(t, s, c, clock, name)

	requested := s.state.History[len(s.state.History)-1]
	if requested.Action != "action_requested" || requested.Name != name {
		t.Fatalf("request event lost the name: %+v", requested)
	}

	c.torrents = nil
	clock.Advance(10 * time.Second)
	poll(t, s)
	confirmed := s.state.History[len(s.state.History)-1]
	if confirmed.Action != "action_confirmed" || confirmed.Name != name {
		t.Fatalf("confirmation event lost the name: %+v", confirmed)
	}
}

func TestPendingDeletionNameSurvivesRestart(t *testing.T) {
	s, c, clock, disk := fixture(t)
	const name = "Persisted Across Restart"
	requestDeletion(t, s, c, clock, name)

	build := s.view.Build
	restarted := New(s.c, c, disk, clock, s.log, observability.New(build), build)
	if got := restarted.state.Tracked[hashA].Name; got != name {
		t.Fatalf("persisted name not reloaded: %q", got)
	}

	c.torrents = nil
	clock.Advance(5 * time.Second)
	poll(t, restarted)
	confirmed := restarted.state.History[len(restarted.state.History)-1]
	if confirmed.Action != "action_confirmed" || confirmed.Name != name {
		t.Fatalf("restart lost the pending name: %+v", confirmed)
	}
}

func TestPendingNameSurvivesRequestEventEviction(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.HistoryLimit = 1
	const name = "Survives Eviction"
	requestDeletion(t, s, c, clock, name)

	// A later retained action pushes the request event out of the bounded
	// history while the pending deletion record, which carries the name,
	// lives on.
	s.event("warn", "success", qbt.Torrent{Hash: hashB, Name: "noise"}, "")
	for _, e := range s.state.History {
		if e.Action == "action_requested" {
			t.Fatal("request event was not evicted")
		}
	}

	c.torrents = nil
	clock.Advance(10 * time.Second)
	poll(t, s)
	confirmed := s.state.History[len(s.state.History)-1]
	if confirmed.Action != "action_confirmed" || confirmed.Name != name {
		t.Fatalf("eviction lost the pending name: %+v", confirmed)
	}
}

func TestConfirmationWithoutPersistedNameStaysEmpty(t *testing.T) {
	s, c, clock, disk := fixture(t)
	requestDeletion(t, s, c, clock, "Will Be Forgotten")

	// Simulate a record written before the name field existed: the pending
	// deletion survives, but no name was captured. The engine records no
	// invented name; the UI supplies the hash-based fallback.
	e := s.state.Tracked[hashA]
	e.Name = ""
	s.state.Tracked[hashA] = e
	disk.state.Tracked[hashA] = e

	c.torrents = nil
	clock.Advance(10 * time.Second)
	poll(t, s)
	confirmed := s.state.History[len(s.state.History)-1]
	if confirmed.Action != "action_confirmed" || confirmed.Name != "" {
		t.Fatalf("legacy record should confirm with no name: %+v", confirmed)
	}
}
