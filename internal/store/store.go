package store

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/qbt"
)

// SchemaVersion is the on-disk layout this build writes.
//
//	1 — episodes only; no policy, no seeder observation.
//	2 — added the policy partition and the seeder observation flag.
//	3 — action-accurate history vocabulary (warn, action_requested,
//	    action_confirmed, action_skipped, action_failed).
//
// Older layouts are migrated pessimistically; anything newer is unreadable and
// is preserved as corrupt rather than guessed at. See migrate.
const SchemaVersion = 4
const MaxAttempts = 3

// HistoryLimit bounds event count; EventTextLimit bounds each name and error
// before JSON escaping, which can expand a byte to six bytes. The independent
// 32 MiB state-file ceiling includes history, tracking and all other state.
const EventTextLimit = 256
const EventHashLimit = 12

var ErrPreserveCorrupt = errors.New("invalid state; cannot preserve corrupt file")
var ErrCorruptRecovered = errors.New("invalid state preserved with timestamped corrupt suffix; tracking reset")

type Episode struct {
	Policy            config.PolicyID `json:"policy"`
	FirstSeen         time.Time       `json:"first_seen_meta"`
	LastSeen          time.Time       `json:"last_seen_meta"`
	DryRunNotified    bool            `json:"dry_run_notified"`
	DeleteRequestedAt *time.Time      `json:"delete_requested_at"`
	Attempts          int             `json:"delete_attempts"`
}
type Counters struct {
	Deletions      uint64 `json:"deletions"`
	WouldDeletions uint64 `json:"would_deletions"`
	DeleteRequests uint64 `json:"delete_requests"`
}
type Event struct {
	Integration     config.ArrKind  `json:"integration,omitempty"`
	Policy          config.PolicyID `json:"policy,omitempty"`
	EffectiveAction config.Action   `json:"effective_action,omitempty"`
	Time            time.Time       `json:"time"`
	Action          string          `json:"action"`
	Name            string          `json:"name"`
	ShortHash       string          `json:"short_hash"`
	DryRun          bool            `json:"dry_run"`
	Outcome         string          `json:"outcome"`
	Error           string          `json:"error,omitempty"`
}

// Bounded normalizes free text at audit creation and state load. Vocabulary
// fields are fixed by the service and validated on load, never truncated into
// a different valid action. The returned text does not retain oversized input.
func (e Event) Bounded() Event {
	e.Name = boundedText(e.Name, EventTextLimit)
	e.Error = boundedText(e.Error, EventTextLimit)
	e.ShortHash = boundedText(e.ShortHash, EventHashLimit)
	e.Time = e.Time.UTC()
	return e
}

func boundedText(text string, limit int) string {
	var result strings.Builder
	result.Grow(min(len(text), limit))
	for _, r := range text {
		if result.Len()+utf8.RuneLen(r) > limit {
			break
		}
		result.WriteRune(r)
	}
	return result.String()
}

type State struct {
	RecoveryJobs  map[string]RecoveryJob `json:"recovery_jobs"`
	SeedObserved  map[string]bool        `json:"seed_observed"`
	SafetyKey     string                 `json:"safety_key"`
	EndpointKey   string                 `json:"endpoint_key"`
	SchemaVersion int                    `json:"schema_version"`
	Tracked       map[string]Episode     `json:"tracked"`
	Counters      Counters               `json:"counters"`
	History       []Event                `json:"history"`
}

func Empty() State {
	return State{SchemaVersion: SchemaVersion, Tracked: map[string]Episode{}, SeedObserved: map[string]bool{}, History: []Event{}, RecoveryJobs: map[string]RecoveryJob{}}
}

type Store interface {
	Load(time.Time) (State, error)
	Save(State) error
}
type File struct {
	Path         string
	HistoryLimit int
}

// readable reports whether this build understands a layout well enough to
// migrate it. Everything else, including any future version, is corrupt.
func readable(version int) bool { return version >= 1 && version <= SchemaVersion }

func valid(s State, now time.Time) bool {
	if len(s.RecoveryJobs) > MaxRecoveryJobs {
		return false
	}
	for id, job := range s.RecoveryJobs {
		if id != job.ID || !job.Valid() {
			return false
		}
	}
	if !readable(s.SchemaVersion) || s.Tracked == nil {
		return false
	}
	if s.SchemaVersion >= 2 && s.SeedObserved == nil {
		return false
	}
	for hash := range s.SeedObserved {
		if !qbt.ValidHash(hash) || hash != strings.ToLower(hash) {
			return false
		}
	}
	for hash, e := range s.Tracked {
		if e.Policy != "" && !e.Policy.Valid() {
			return false
		}
		if !qbt.ValidHash(hash) || hash != strings.ToLower(hash) || e.FirstSeen.IsZero() || e.LastSeen.Before(e.FirstSeen) || e.LastSeen.After(now) || e.Attempts < 0 || e.Attempts > MaxAttempts {
			return false
		}
		if e.DeleteRequestedAt != nil && (e.Attempts == 0 || e.DeleteRequestedAt.IsZero() || e.DeleteRequestedAt.After(now)) {
			return false
		}
	}
	for _, e := range s.History {
		if e.Integration != "" && e.Integration != config.Sonarr && e.Integration != config.Radarr {
			return false
		}
		if e.Policy != "" && !e.Policy.Valid() {
			return false
		}
		if e.EffectiveAction != "" && e.EffectiveAction != config.Warn && e.EffectiveAction != config.Delete && e.EffectiveAction != config.DeleteFile {
			return false
		}
		if e.Time.IsZero() || e.Time.After(now) || len(e.ShortHash) > EventHashLimit || !validOutcome(e.Outcome) || len(e.Error) > EventTextLimit || len(e.Name) > EventTextLimit {
			return false
		}
		// Older layouts named their events after deletion rather than after
		// the policy action. migrate drops that history wholesale, so it is
		// pointless — and wrong — to hold it to the current vocabulary.
		if s.SchemaVersion >= 3 && !validAction(e.Action) {
			return false
		}
	}
	return true
}
func validAction(s string) bool {
	switch s {
	case "warn", "action_requested", "action_confirmed", "action_skipped", "action_failed", "recovery":
		return true
	}
	return false
}
func validOutcome(s string) bool {
	switch s {
	case "success", "accepted", "skipped", "failed":
		return true
	}
	return false
}

// migrate brings an already-validated older record up to the current schema.
// It is deliberately pessimistic: every field whose meaning could have changed,
// and every fact the current engine can re-derive, is reset rather than
// trusted. Exactly three things survive, because losing them would be unsafe
// rather than merely inconvenient: the lifetime counters, the finite
// per-episode attempt budget, and a delete request still awaiting confirmation.
func migrate(s State, now time.Time) State {
	if s.SchemaVersion < 4 {
		s.RecoveryJobs = map[string]RecoveryJob{}
	}
	if s.RecoveryJobs == nil {
		s.RecoveryJobs = map[string]RecoveryJob{}
	}
	if s.SchemaVersion == 3 {
		s.SchemaVersion = SchemaVersion
		return s
	}
	if s.SchemaVersion == SchemaVersion {
		return s
	}
	if s.SchemaVersion < 2 {
		// Schema 1 predates seeder observation entirely, and a seeder is
		// never inferred before it has actually been seen.
		s.SeedObserved = map[string]bool{}
	}
	// Schema 2 named events after deletion rather than after the policy
	// action, so the old vocabulary is dropped instead of relabelled.
	s.History = []Event{}
	// The partition and the episode clock are re-proven from scratch under
	// whatever rules this build applies.
	for hash, e := range s.Tracked {
		e.Policy, e.DryRunNotified = "", false
		e.FirstSeen, e.LastSeen = now, now
		s.Tracked[hash] = e
	}
	// An empty safety key forces the service to reset its timers too.
	s.SafetyKey = ""
	s.SchemaVersion = SchemaVersion
	return s
}

func (f File) Load(now time.Time) (State, error) {
	file, err := os.Open(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Empty(), nil
	}
	if err != nil {
		return Empty(), errors.New("cannot open state file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 32*1024*1024+1))
	file.Close()
	if err != nil {
		return Empty(), errors.New("cannot read state file")
	}
	var s State
	decoded := len(data) <= 32*1024*1024 && json.Unmarshal(data, &s) == nil
	if decoded {
		for i := range s.History {
			s.History[i] = s.History[i].Bounded()
		}
	}
	if !decoded || !valid(s, now) {
		backup := f.Path + "." + now.UTC().Format("20060102T150405.000000000") + ".corrupt"
		if err := os.Rename(f.Path, backup); err != nil {
			return Empty(), ErrPreserveCorrupt
		}
		return Empty(), ErrCorruptRecovered
	}
	s = migrate(s, now)
	for hash, e := range s.Tracked {
		e.FirstSeen = e.FirstSeen.UTC()
		e.LastSeen = e.LastSeen.UTC()
		if e.DeleteRequestedAt != nil {
			utc := e.DeleteRequestedAt.UTC()
			e.DeleteRequestedAt = &utc
		}
		s.Tracked[hash] = e
	}
	if len(s.History) > f.HistoryLimit {
		s.History = s.History[len(s.History)-f.HistoryLimit:]
	}
	return s, nil
}

func (f File) Save(s State) error {
	data, err := json.Marshal(s)
	if err != nil || len(data) > 32*1024*1024 {
		return errors.New("cannot encode state")
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create state directory")
	}
	file, err := os.CreateTemp(dir, ".qbt-watchdog-*")
	if err != nil {
		return errors.New("cannot create temporary state file")
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return errors.New("cannot restrict state permissions")
	}
	if _, err = file.Write(data); err != nil {
		return errors.New("cannot write state file")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot flush state file")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close state file")
	}
	if err = os.Rename(tmp, f.Path); err != nil {
		return errors.New("cannot atomically replace state file")
	}
	d, err := os.Open(dir)
	if err != nil {
		return errors.New("cannot open state directory for sync")
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return errors.New("cannot sync state directory")
	}
	return nil
}
