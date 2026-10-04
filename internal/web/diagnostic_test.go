package web

import (
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/watchdog"
)

func TestResponseRejectedIndicatorIsDistinctFromConnectionFailure(t *testing.T) {
	c, s, m := fixture(t)
	s.QBTUp = false
	s.PollError = `qBittorrent torrents/info rejected: entry 7, torrent abc123def456: size=123 exceeds total_size=0`
	s.PollDiagnostic = &watchdog.PollDiagnostic{
		Kind: watchdog.PollErrorResponseRejected, Stage: watchdog.PollStageInitialList,
		Code: "size_exceeds_total", Field: "size", Torrent: "abc123def456",
	}
	s.QBTVersion = "5.2.3"
	s.WebAPIVersion = "2.11.2"
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	page := body(t, h, "/partials/services")
	for _, want := range []string{
		"Response rejected",
		"Diagnostic",
		"Meaning",
		"qBittorrent responded, but the returned torrent data failed validation.",
		"5.2.3",
		"2.11.2",
		"Last successful poll",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("response-rejected popover missing %q:\n%s", want, page)
		}
	}

	// A generic connection failure must never claim the response was rejected.
	s.PollDiagnostic = &watchdog.PollDiagnostic{Kind: watchdog.PollErrorPollFailed, Stage: watchdog.PollStageInitialList}
	s.PollError = "qBittorrent request failed (network or TLS)"
	generic := body(t, h, "/partials/services")
	if strings.Contains(generic, "Response rejected") {
		t.Fatal("generic connection failure labelled as a rejected response")
	}
	if !strings.Contains(generic, "Failed") {
		t.Fatal("generic connection failure not labelled failed")
	}
}

func TestTorrentListStatesAreDistinct(t *testing.T) {
	c, s, m := fixture(t)
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	// Startup before any result.
	s.LastTorrentListSuccess = nil
	s.Torrents = nil
	waiting := body(t, h, "/partials/torrent-rows")
	if !strings.Contains(waiting, "Waiting for the first torrent snapshot.") {
		t.Fatalf("waiting state missing:\n%s", waiting)
	}
	if strings.Contains(waiting, "No torrents in the latest snapshot.") {
		t.Fatal("waiting state rendered the accepted-empty message")
	}

	// No accepted list; validation failed.
	s.PollDiagnostic = &watchdog.PollDiagnostic{Kind: watchdog.PollErrorResponseRejected, Stage: watchdog.PollStageInitialList}
	s.PollError = "qBittorrent torrents/info rejected: entry 0: hash is not a valid info-hash"
	rejected := body(t, h, "/partials/torrent-rows")
	if !strings.Contains(rejected, "No valid torrent snapshot yet — qBittorrent response rejected.") {
		t.Fatalf("rejected state missing:\n%s", rejected)
	}

	// Accepted list with zero torrents.
	s.PollDiagnostic = nil
	s.PollError = ""
	now := time.Now().UTC()
	s.LastTorrentListSuccess = &now
	empty := body(t, h, "/partials/torrent-rows")
	if !strings.Contains(empty, "No torrents in the latest snapshot.") {
		t.Fatalf("accepted-empty state missing:\n%s", empty)
	}

	// Earlier accepted data; current poll failed.
	s.Torrents = []watchdog.Row{{Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL", Decision: "tracking"}}
	s.TorrentDataStale = true
	s.PollError = "qBittorrent torrents/info rejected: entry 0, torrent aaaaaaaaaaaa: state is empty"
	stale := body(t, h, "/partials/torrent-rows")
	if !strings.Contains(stale, "Showing last accepted torrent data from") {
		t.Fatalf("stale state missing:\n%s", stale)
	}
	if !strings.Contains(stale, `data-key="torrent-aaaaaaaaaaaa"`) {
		t.Fatal("stale rows lost their reconciliation identity")
	}
	if !strings.Contains(stale, `data-key="torrent-notice"`) {
		t.Fatal("stale notice is not a keyed sibling of the rows")
	}
}

func TestDiagnosticsAreEscapedInPagesAndFragments(t *testing.T) {
	c, s, m := fixture(t)
	hostile := `</p><script>alert("x")</script>`
	s.PollError = hostile
	s.PollDiagnostic = &watchdog.PollDiagnostic{Kind: watchdog.PollErrorResponseRejected, Stage: watchdog.PollStageInitialList}
	s.TorrentDataStale = true
	s.Torrents = []watchdog.Row{{Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL", Decision: "tracking"}}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	for _, path := range []string{"/", "/partials/torrents", "/partials/torrent-rows", "/partials/services"} {
		page := body(t, h, path)
		if strings.Contains(page, "<script>alert") {
			t.Fatalf("diagnostic rendered as markup in %s:\n%s", path, page)
		}
		if !strings.Contains(page, "&lt;script&gt;") {
			t.Fatalf("diagnostic not escaped into text in %s", path)
		}
	}
}
