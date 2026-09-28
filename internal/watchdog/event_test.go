package watchdog

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
