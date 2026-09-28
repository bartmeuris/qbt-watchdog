package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const hash = "0123456789abcdef0123456789abcdef01234567"

func TestMissingAtomicRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	f := File{Path: filepath.Join(dir, "data", "state.json"), HistoryLimit: 10}
	now := time.Now().UTC()
	s, e := f.Load(now)
	if e != nil || len(s.Tracked) != 0 {
		t.Fatal(s, e)
	}
	s.Tracked[hash] = Episode{FirstSeen: now.Add(-time.Minute), LastSeen: now, DryRunNotified: true, DeleteRequestedAt: &now, Attempts: 1}
	s.Counters.WouldDeletions = 1
	if e = f.Save(s); e != nil {
		t.Fatal(e)
	}
	loaded, e := f.Load(now)
	if e != nil || loaded.Counters.WouldDeletions != 1 || !loaded.Tracked[hash].DryRunNotified || loaded.Tracked[hash].DeleteRequestedAt == nil {
		t.Fatal(loaded, e)
	}
	info, e := os.Stat(f.Path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	if e = f.Save(Empty()); e != nil {
		t.Fatal(e)
	}
	loaded, e = f.Load(now)
	if e != nil || len(loaded.Tracked) != 0 {
		t.Fatal(loaded, e)
	}
	files, e := os.ReadDir(filepath.Dir(f.Path))
	if e != nil || len(files) != 1 {
		t.Fatal(files, e)
	}
}
func TestCorruptAndInvalidStatePreserved(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string][]byte{"malformed": []byte("{BROKEN"), "unknown schema": []byte(`{"schema_version":2,"tracked":{}}`), "missing schema": []byte(`{"tracked":{}}`), "missing tracked": []byte(`{"schema_version":1}`), "null": []byte(`null`)}
	for name, episode := range map[string]Episode{"future": {FirstSeen: now.Add(time.Minute), LastSeen: now.Add(time.Minute)}, "reverse": {FirstSeen: now, LastSeen: now.Add(-time.Second)}, "attempts": {FirstSeen: now, LastSeen: now, Attempts: 4}, "pending without attempts": {FirstSeen: now, LastSeen: now, DeleteRequestedAt: &now}} {
		s := Empty()
		s.Tracked[hash] = episode
		cases[name], _ = json.Marshal(s)
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			f := File{Path: filepath.Join(dir, "state.json"), HistoryLimit: 10}
			if e := os.WriteFile(f.Path, data, 0600); e != nil {
				t.Fatal(e)
			}
			s, e := f.Load(now)
			if e == nil || len(s.Tracked) != 0 {
				t.Fatal(s, e)
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 1 || !strings.HasSuffix(files[0].Name(), ".corrupt") {
				t.Fatal("corruption not preserved", files)
			}
			saved, _ := os.ReadFile(filepath.Join(dir, files[0].Name()))
			if string(saved) != string(data) {
				t.Fatal("corrupt data changed")
			}
			if e = f.Save(s); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestPendingBeforeGapResetRemainsValid(t *testing.T) {
	now := time.Now().UTC()
	requested := now.Add(-time.Minute)
	s := Empty()
	s.Tracked[hash] = Episode{FirstSeen: now, LastSeen: now, Attempts: 1, DeleteRequestedAt: &requested}
	if !valid(s, now) {
		t.Fatal("pending request before gap reset rejected")
	}
}
func TestWriteFailureSanitized(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "PRIVATE_PATH")
	if e := os.WriteFile(parent, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	f := File{Path: filepath.Join(parent, "state.json"), HistoryLimit: 1}
	if e := f.Save(Empty()); e == nil || strings.Contains(e.Error(), dir) || strings.Contains(e.Error(), "PRIVATE_PATH") {
		t.Fatal("unsafe write error", e)
	}
}
func TestHistoryTruncated(t *testing.T) {
	now := time.Now().UTC()
	f := File{Path: filepath.Join(t.TempDir(), "state.json"), HistoryLimit: 1}
	s := Empty()
	for range 3 {
		s.History = append(s.History, Event{Time: now, Action: "warn", Outcome: "success"})
	}
	if e := f.Save(s); e != nil {
		t.Fatal(e)
	}
	loaded, e := f.Load(now)
	if e != nil || len(loaded.History) != 1 {
		t.Fatal(loaded, e)
	}
}
