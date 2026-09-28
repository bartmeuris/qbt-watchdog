package arr

import (
	"encoding/json"
	"time"

	"qbt-watchdog/internal/config"
)

// This file is the boundary. Everything above it works with trusted domain
// values; everything below it assumes the media manager may add fields, may
// send null where the schema promises a value, and may change a scalar's
// representation between releases.
//
// The rules are uniform: unknown fields are ignored (encoding/json's default,
// deliberately not DisallowUnknownFields — a new Sonarr release must not take
// the watchdog down), absent and null fields decode to a zero value or a nil
// pointer, and a record that lacks an identity is dropped rather than guessed
// at. A payload that is not even JSON is a loud failure, because silently
// treating a broken response as an empty queue would look exactly like "this
// torrent is not managed by Sonarr".

// flexString accepts a JSON string or number and never fails. Arr history
// event types have been serialised both ways across major versions, and a
// representation change must not blind the whole lookup.
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	var text string
	if json.Unmarshal(data, &text) == nil {
		*f = flexString(text)
		return nil
	}
	var number json.Number
	if json.Unmarshal(data, &number) == nil {
		*f = flexString(number.String())
	}
	return nil
}

// flexTime accepts the timestamp shapes Arr emits and leaves the zero time in
// place for anything else. A timestamp is a diagnostic, never a decision
// input, so an unparseable one must not fail a lookup.
type flexTime struct{ time.Time }

func (f *flexTime) UnmarshalJSON(data []byte) error {
	var text string
	if json.Unmarshal(data, &text) != nil || text == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			f.Time = parsed.UTC()
			return nil
		}
	}
	return nil
}

// pagedResponse is the PagingResource envelope shared by the queue and history
// endpoints. The counters are pointers so that a server which omits them is
// handled by the page-shape stop conditions instead of by a zero that would
// look like "no records at all".
type pagedResponse[R any] struct {
	Page         *int `json:"page"`
	PageSize     *int `json:"pageSize"`
	TotalRecords *int `json:"totalRecords"`
	Records      []R  `json:"records"`
}

// queueRecord is the subset of QueueResource the watchdog relies on.
//
// It deliberately does not include the release title. Matching a queue row to
// a torrent by name is unsafe — names are rewritten, truncated and duplicated
// — so the field is simply never carried, which makes matching on it
// impossible rather than merely discouraged.
//
// Sonarr v4 exposes a single nullable episodeId per row, not a list: a season
// pack produces one queue row per episode, every row sharing the same
// downloadId. Grouping is therefore not an optimisation, it is the only way to
// learn which episodes a pack covered.
type queueRecord struct {
	ID         *int64 `json:"id"`
	DownloadID string `json:"downloadId"`
	EpisodeID  *int64 `json:"episodeId"`
	MovieID    *int64 `json:"movieId"`
}

// item projects a wire record onto the trusted domain type, reporting whether
// the row is usable at all. A row without a positive id cannot be deleted and
// a row without a download id can never be matched to a torrent, so both are
// dropped: they are ordinary Arr states (a queue entry for an unknown series,
// for instance), not errors.
func (r queueRecord) item(kind config.ArrKind) (QueueItem, bool) {
	downloadID := NormalizeDownloadID(string(r.DownloadID))
	if r.ID == nil || *r.ID <= 0 || downloadID == "" {
		return QueueItem{}, false
	}
	item := QueueItem{ID: *r.ID, DownloadID: downloadID}
	media := r.MovieID
	if kind == config.Sonarr {
		media = r.EpisodeID
	}
	// The media id is copied rather than aliased, and the field the kind
	// does not own is discarded, so an episode id can never be mistaken for
	// a movie id downstream.
	if media != nil && *media > 0 {
		value := *media
		item.MediaID = &value
	}
	return item, true
}

// historyRecord is the subset of HistoryResource used for read-only lookups.
type historyRecord struct {
	ID         *int64     `json:"id"`
	DownloadID string     `json:"downloadId"`
	EventType  flexString `json:"eventType"`
	EpisodeID  *int64     `json:"episodeId"`
	MovieID    *int64     `json:"movieId"`
	Date       flexTime   `json:"date"`
}

func (r historyRecord) record(kind config.ArrKind) (HistoryRecord, bool) {
	downloadID := NormalizeDownloadID(string(r.DownloadID))
	if downloadID == "" || r.ID == nil || *r.ID <= 0 {
		return HistoryRecord{}, false
	}
	entry := HistoryRecord{DownloadID: downloadID, EventType: safeEventType(string(r.EventType)), Date: r.Date.Time}
	if r.ID != nil {
		entry.ID = *r.ID
	}
	media := r.MovieID
	if kind == config.Sonarr {
		media = r.EpisodeID
	}
	if media != nil && *media > 0 {
		value := *media
		entry.MediaID = &value
	}
	return entry, true
}

// commandRequest is the CommandResource body accepted by POST /api/v3/command.
// Only the field belonging to this client's kind is ever emitted.
type commandRequest struct {
	Name       string  `json:"name"`
	EpisodeIDs []int64 `json:"episodeIds,omitempty"`
	MovieIDs   []int64 `json:"movieIds,omitempty"`
}

// commandResponse is the CommandResource returned by a command POST and by a
// command status GET.
type commandResponse struct {
	ID     *int64     `json:"id"`
	Name   flexString `json:"name"`
	Status string     `json:"status"`
}

func (r commandResponse) command() (Command, bool) {
	if r.ID == nil || *r.ID <= 0 || normalizeStatus(string(r.Status)) == "" {
		return Command{}, false
	}
	name := ""
	if r.Name == "EpisodeSearch" || r.Name == "MoviesSearch" {
		name = string(r.Name)
	}
	return Command{ID: *r.ID, Name: name, Status: normalizeStatus(string(r.Status))}, true
}

func safeEventType(event string) string {
	switch event {
	case "grabbed", "downloadFailed", "downloadFolderImported", "downloadIgnored", "episodeFileDeleted", "episodeFileRenamed", "movieFileDeleted", "movieFileRenamed", "downloadImported", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return event
	default:
		return "unknown"
	}
}
