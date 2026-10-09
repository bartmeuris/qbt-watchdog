package store

import (
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/qbt"
)

const MaxRecoveryJobs = 500
const MaxRecoveryIDs = 200
const RecoveryTTL = 24 * time.Hour
const MaxRecoveryAttempts = 5

type RecoveryStage string

const (
	Prepared         RecoveryStage = "prepared"
	AwaitDelete      RecoveryStage = "await_delete_confirmation"
	Resolving        RecoveryStage = "resolving_identity"
	BlocklistPending RecoveryStage = "blocklist_pending"
	BlocklistIntent  RecoveryStage = "blocklist_intent"
	SearchPending    RecoveryStage = "search_pending"
	SearchIntent     RecoveryStage = "search_intent"
	CommandPending   RecoveryStage = "command_pending"
	Uncertain        RecoveryStage = "uncertain"
)

type RecoveryJob struct {
	ID         string          `json:"id"`
	Kind       config.ArrKind  `json:"kind"`
	Endpoint   string          `json:"endpoint_fingerprint"`
	Hash       string          `json:"hash"`
	Name       string          `json:"name,omitempty"`
	Policy     config.PolicyID `json:"policy"`
	Action     config.Action   `json:"action"`
	EpisodeAt  time.Time       `json:"episode_at"`
	CapturedAt time.Time       `json:"captured_at"`
	AcceptedAt time.Time       `json:"accepted_at"`
	DeletedAt  time.Time       `json:"deleted_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	NextAt     time.Time       `json:"next_at"`
	QueueIDs   []int64         `json:"queue_ids"`
	HistoryIDs []int64         `json:"history_ids"`
	MediaIDs   []int64         `json:"media_ids"`
	Mode       config.ArrMode  `json:"mode"`
	Stage      RecoveryStage   `json:"stage"`
	Attempts   int             `json:"attempts"`
	CommandID  int64           `json:"command_id"`
	LastCode   string          `json:"last_code"`
	// BlocklistAt records the durable intent to make the one best-effort
	// blocklist attempt before qBittorrent deletion; BlocklistCode records its
	// outcome. Both are written before the call so a crash cannot replay it.
	BlocklistAt   time.Time `json:"blocklist_at,omitempty"`
	BlocklistCode string    `json:"blocklist_code,omitempty"`
}

func (j RecoveryJob) Clone() RecoveryJob {
	j.QueueIDs = slices.Clone(j.QueueIDs)
	j.HistoryIDs = slices.Clone(j.HistoryIDs)
	j.MediaIDs = slices.Clone(j.MediaIDs)
	return j
}

func (j RecoveryJob) Valid() bool {
	if !digest(j.ID) || !digest(j.Endpoint) || !qbt.ValidHash(j.Hash) || j.Hash != strings.ToLower(j.Hash) ||
		(j.Kind != config.Sonarr && j.Kind != config.Radarr) || !j.Policy.Valid() || !j.Action.Destructive() || !j.Mode.Valid() ||
		j.CapturedAt.IsZero() || j.EpisodeAt.IsZero() || j.ExpiresAt.Before(j.CapturedAt) || j.ExpiresAt.Sub(j.CapturedAt) > RecoveryTTL ||
		j.Attempts < 0 || j.Attempts > MaxRecoveryAttempts || j.CommandID < 0 || !RecoveryCode(j.LastCode) || !RecoveryCode(j.BlocklistCode) {
		return false
	}
	for _, ids := range [][]int64{j.QueueIDs, j.HistoryIDs, j.MediaIDs} {
		if len(ids) > MaxRecoveryIDs {
			return false
		}
		for i, id := range ids {
			if id <= 0 || (i > 0 && ids[i-1] >= id) {
				return false
			}
		}
	}
	switch j.Stage {
	case Prepared, Uncertain:
		return true
	case AwaitDelete:
		return !j.AcceptedAt.IsZero()
	case Resolving, BlocklistPending, BlocklistIntent, SearchPending, SearchIntent:
		return !j.AcceptedAt.IsZero() && !j.DeletedAt.IsZero()
	case CommandPending:
		return !j.AcceptedAt.IsZero() && !j.DeletedAt.IsZero() && j.CommandID > 0
	}
	return false
}

func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func RecoveryCode(code string) bool {
	switch code {
	case "", "accepted", "not_found", "rejected", "transport_failure", "ambiguous_timeout", "identity_missing", "identity_incomplete", "queue_vanished", "safety_unavailable", "replacement_queued", "already_imported", "search_completed", "blocklist_completed", "command_failed", "expired", "cancelled", "restart_uncertain", "endpoint_changed", "capacity", "attempts_exhausted", "qbt_unaccepted", "confirmation_aborted", "own_search_pending":
		return true
	}
	return false
}
