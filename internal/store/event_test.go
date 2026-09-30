package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"qbt-watchdog/internal/config"
)

func TestEventTextBounds(t *testing.T) {
	for _, input := range []string{"", "ordinary name", strings.Repeat("a", 256), strings.Repeat("界", 1000000), strings.Repeat("a", 255) + "界", strings.Repeat("\xff", 1000000), strings.Repeat("<\x00&\"\\", 1000000)} {
		e := (Event{Name: input, Error: input, ShortHash: input}).Bounded()
		for field, value := range map[string]string{"name": e.Name, "error": e.Error, "hash": e.ShortHash} {
			limit := EventTextLimit
			if field == "hash" {
				limit = EventHashLimit
			}
			if len(value) > limit || !utf8.ValidString(value) {
				t.Fatalf("%s: invalid UTF-8 or over limit: %d", field, len(value))
			}
			if utf8.ValidString(input) && !strings.HasPrefix(input, value) {
				t.Fatalf("%s: not a prefix", field)
			}
			if utf8.ValidString(input) && len(input) <= limit && input != value {
				t.Fatalf("%s: short text changed", field)
			}
		}
		if e != e.Bounded() {
			t.Fatal("normalization is not idempotent")
		}
	}
	e := (Event{Name: strings.Repeat("a", 255) + "界"}).Bounded()
	if len(e.Name) != 255 {
		t.Fatal("split rune retained")
	}
}

func TestEventCommandIDRoundTrips(t *testing.T) {
	now := time.Now().UTC()
	s := Empty()
	s.History = []Event{{Time: now, Action: "recovery", Outcome: "success", CommandID: 44, ShortHash: "aaaaaaaaaaaa", Error: "search_completed"}}
	f := write(t, s)
	loaded, err := f.Load(now)
	if err != nil || len(loaded.History) != 1 {
		t.Fatalf("event with command_id rejected: %v %v", err, loaded.History)
	}
	if loaded.History[0].CommandID != 44 {
		t.Fatal("command_id not preserved")
	}
}

func TestLoadedHistoryNormalizesHostileText(t *testing.T) {
	now := time.Now().UTC()
	s := Empty()
	s.Tracked[hash] = Episode{FirstSeen: now, LastSeen: now, Attempts: 1, DeleteRequestedAt: &now}
	s.Counters.DeleteRequests = 1
	for range 3 {
		s.History = append(s.History, Event{Time: now, Action: "warn", Outcome: "success", Name: strings.Repeat("界", 100000), Error: strings.Repeat("<", 100000), ShortHash: strings.Repeat("a", 100000)})
	}
	f := write(t, s)
	f.HistoryLimit = 2
	loaded, err := f.Load(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.History) != 2 || loaded.Counters != s.Counters || loaded.Tracked[hash].Attempts != 1 || loaded.Tracked[hash].DeleteRequestedAt == nil {
		t.Fatal("normalization lost history or safety state")
	}
	for _, e := range loaded.History {
		if e != s.History[0].Bounded() {
			t.Fatal("loaded history not normalized")
		}
	}
	if err := f.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(now); err != nil {
		t.Fatal("normalized history does not round-trip", err)
	}
}

func TestEscapedEventSizeBound(t *testing.T) {
	e := (Event{Policy: config.StalledSeedersSeen, EffectiveAction: config.DeleteFile, Time: time.Date(2026, 9, 13, 12, 0, 0, 123456789, time.UTC), Action: "action_confirmed", Outcome: "accepted", Name: strings.Repeat("<", EventTextLimit), Error: strings.Repeat("\x00", EventTextLimit), ShortHash: strings.Repeat("&", EventHashLimit)}).Bounded()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	// 524 free-text bytes can expand sixfold; fixed fields add <256 bytes.
	const maxEventBytes = 6*(2*EventTextLimit+EventHashLimit) + 256
	if len(data) <= 6*(2*EventTextLimit+EventHashLimit) || len(data) > maxEventBytes {
		t.Fatalf("unexpected escaped event size: %d", len(data))
	}
	t.Logf("worst-case fixture: %d bytes; conservative event bound: %d bytes", len(data), maxEventBytes)
}
