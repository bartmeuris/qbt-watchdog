package web

import (
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// The multi-step action timeline. Every torrent that the engine has begun to
// act on walks the same fixed steps, and each step carries one closed status.
// The timeline is assembled entirely from the correlation fields added in
// earlier phases — ShortHash and CommandID — so a torrent row, a recovery job
// and the history rows that describe the same work all tell one story.

// Closed step-status vocabulary.
const (
	StatusWaiting   = "waiting"
	StatusRequested = "requested"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusSkipped   = "skipped"
	StatusFailed    = "failed"
	StatusUncertain = "uncertain"
)

// Step is one stage of the timeline, with a label fixed by the engine and a
// status from the closed vocabulary above.
type Step struct {
	Label  string `json:"label"`
	Status string `json:"status"`
}

// TimelineSteps returns the ordered labels of a full timeline. Keeping the order
// in one place means no template can reorder tagging before removal.
func TimelineSteps() []string {
	return []string{"Tagging", "Torrent removal", "Blocklist", "Search", "Replacement observed"}
}

// Timeline holds a per-entity timeline plus the highest-certainty status, used
// by the template to colour the row.
type Timeline struct {
	Steps  []Step `json:"steps"`
	Latest string `json:"latest"`
}

func (t Timeline) latest() string {
	for _, s := range t.Steps {
		if s.Status != StatusWaiting {
			return s.Status
		}
	}
	return StatusWaiting
}

// correlatedRecovery finds the recovery job for a torrent hash, preferring one
// with a matching command ID because it pins the same command run through the
// history. When none matches it returns the zero value (false).
func correlatedRecovery(jobs []watchdog.RecoveryStatus, shortHash string, commandID int64) (watchdog.RecoveryStatus, bool) {
	var fallback *watchdog.RecoveryStatus
	for i := range jobs {
		j := jobs[i]
		if j.ShortHash != shortHash {
			continue
		}
		if j.CommandID != 0 && j.CommandID == commandID {
			return j, true
		}
		if fallback == nil {
			fallback = &j
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return watchdog.RecoveryStatus{}, false
}

// historyOutcomeFor returns the audit outcome (and whether it exists) for a
// given audit action and short hash, newest first. It is how the timeline knows
// a removal was requested versus confirmed versus failed.
func historyAction(history []store.Event, shortHash string, action string) (store.Event, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		e := history[i]
		if e.ShortHash == shortHash && e.Action == action {
			return e, true
		}
	}
	return store.Event{}, false
}

// TorrentTimeline assembles the five-step timeline for a torrent row from its
// recovery jobs and the correlated history. Waiting steps render as such; a
// destructive flow only advances a step once the earlier one has an outcome.
func TorrentTimeline(row watchdog.Row, jobs []watchdog.RecoveryStatus, history []store.Event) Timeline {
	shortHash := row.ShortHash
	job, hasJob := correlatedRecovery(jobs, shortHash, 0)

	removal := StatusWaiting
	if _, ok := historyAction(history, shortHash, "action_confirmed"); ok {
		removal = StatusCompleted
	} else if _, ok := historyAction(history, shortHash, "action_failed"); ok {
		removal = StatusFailed
	} else if row.DeleteRequestedAt != nil {
		removal = StatusRequested
	} else if _, ok := historyAction(history, shortHash, "action_requested"); ok {
		removal = StatusRequested
	} else if _, ok := historyAction(history, shortHash, "action_skipped"); ok {
		removal = StatusSkipped
	}

	tagging := StatusWaiting
	if len(row.WatchdogTags) > 0 && len(row.DesiredTags) > 0 {
		match := true
		for _, want := range row.DesiredTags {
			if !hasTag(row.WatchdogTags, want) {
				match = false
				break
			}
		}
		if match {
			tagging = StatusCompleted
		}
	}

	blocklist, search, replacement := StatusWaiting, StatusWaiting, StatusWaiting
	if hasJob {
		switch job.Code {
		case "blocklist_completed", "search_completed", "replacement_queued", "already_imported":
			blocklist = StatusCompleted
		case "blocklist_failed", "command_failed":
			blocklist = StatusFailed
		}
		switch job.Stage {
		case store.Resolving:
			blocklist = StatusRequested
		case store.BlocklistIntent:
			blocklist = StatusRequested
		case store.BlocklistPending:
			blocklist = StatusRunning
		case store.SearchIntent:
			search = StatusRequested
		case store.SearchPending:
			search = StatusRunning
		case store.CommandPending:
			search = StatusRunning
		case store.Uncertain:
			replacement = StatusUncertain
			if blocklist == StatusWaiting {
				blocklist = StatusUncertain
			}
			if search == StatusWaiting {
				search = StatusUncertain
			}
		}
		switch job.Code {
		case "search_completed":
			search = StatusCompleted
			blocklist = StatusCompleted
			replacement = StatusRunning
		case "replacement_queued", "already_imported":
			search = StatusCompleted
			blocklist = StatusCompleted
			replacement = StatusCompleted
		case "blocklist_completed":
			blocklist = StatusCompleted
			search = StatusRequested
		}
		if job.Mode != config.SearchOnly && job.Mode != config.NoArrMode {
			// a blocklist-capable mode is in play; leave blocklist as derived.
			_ = job.Mode
		}
		if job.Mode == config.SearchOnly {
			blocklist = StatusSkipped
		}
		if job.Mode == config.BlocklistOnly {
			search = StatusSkipped
		}
		if job.CommandID > 0 && search == StatusWaiting && job.Stage != store.Uncertain {
			search = StatusRequested
		}
		if _, ok := historyAction(history, shortHash, "recovery"); ok && replacement == StatusWaiting {
			replacement = StatusRunning
		}
	}

	t := Timeline{Steps: []Step{
		{Label: "Tagging", Status: tagging},
		{Label: "Torrent removal", Status: removal},
		{Label: "Blocklist", Status: blocklist},
		{Label: "Search", Status: search},
		{Label: "Replacement observed", Status: replacement},
	}}
	t.Latest = t.latest()
	return t
}

// RecoveryTimeline assembles the timeline strictly from one recovery job, so a
// recovery row reads the same five steps without needing a torrent row.
func RecoveryTimeline(job watchdog.RecoveryStatus) Timeline {
	removal := StatusCompleted
	if job.Stage == store.AwaitDelete {
		removal = StatusRequested
	} else if job.Stage == store.Prepared {
		removal = StatusRequested
	}
	tagging := StatusCompleted

	blocklist, search, replacement := StatusWaiting, StatusWaiting, StatusWaiting
	switch job.Stage {
	case store.Resolving:
		blocklist, search = StatusRequested, StatusWaiting
	case store.BlocklistIntent:
		blocklist = StatusRequested
	case store.BlocklistPending, store.CommandPending:
		blocklist = StatusRunning
	case store.SearchIntent:
		search = StatusRequested
	case store.SearchPending:
		search = StatusRunning
	case store.Uncertain:
		blocklist, search, replacement = StatusUncertain, StatusUncertain, StatusUncertain
	}
	switch job.Code {
	case "blocklist_completed":
		blocklist, search = StatusCompleted, StatusRequested
	case "search_completed":
		blocklist, search = StatusCompleted, StatusCompleted
		replacement = StatusRunning
	case "replacement_queued", "already_imported":
		blocklist, search, replacement = StatusCompleted, StatusCompleted, StatusCompleted
	case "blocklist_failed", "command_failed":
		blocklist = StatusFailed
	}
	if job.Mode == config.SearchOnly {
		blocklist = StatusSkipped
	}
	if job.Mode == config.BlocklistOnly {
		search = StatusSkipped
	}

	t := Timeline{Steps: []Step{
		{Label: "Tagging", Status: tagging},
		{Label: "Torrent removal", Status: removal},
		{Label: "Blocklist", Status: blocklist},
		{Label: "Search", Status: search},
		{Label: "Replacement observed", Status: replacement},
	}}
	t.Latest = t.latest()
	return t
}
