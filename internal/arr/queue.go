package arr

import (
	"encoding/hex"
	"slices"
	"strings"
)

// MaxIDsPerJob bounds how much work a single recovery job may carry.
//
// The numbers come from the shape of the data: a queue row exists per episode,
// so a full-series pack can legitimately be hundreds of rows, while a runaway
// or hostile response could be unbounded. Oversized jobs are marked Truncated
// and must not be acted on using their partial identity.
const MaxIDsPerJob = 200

// QueueItem is one row of a media manager's queue, reduced to the three facts
// the watchdog acts on. It carries no title: see queueRecord.
type QueueItem struct {
	// ID is the queue row, and the only thing DELETE /queue/{id} accepts.
	ID int64
	// DownloadID is the torrent hash the media manager handed to
	// qBittorrent, normalised to lower case at the boundary so that every
	// comparison in this package is already case insensitive.
	DownloadID string
	// MediaID is the Sonarr episode id or the Radarr movie id, resolved by
	// kind when the row was parsed. It is nil for a row the media manager
	// has not attributed to anything.
	MediaID *int64
}

// Job is everything a caller needs to recover one torrent: which queue rows to
// remove, and which episodes or movies to search for afterwards.
//
// A season pack collapses into exactly one Job with many ItemIDs and many
// MediaIDs, which is why the search is a single command rather than one per
// episode.
type Job struct {
	DownloadID string
	// ItemIDs are matching queue rows, unique and ascending. Do not DELETE
	// each row: removing one tracked download may clear the whole pack.
	ItemIDs []int64
	// MediaIDs are the episode or movie ids to search for, unique and
	// ascending. Missing attribution marks the job Incomplete.
	MediaIDs []int64
	// Truncated reports that the release exceeded MaxIDsPerJob and this
	// job therefore covers only part of it.
	Truncated  bool
	Incomplete bool
}

// NormalizeDownloadID is the single definition of download-id identity. Media
// managers echo the hash in whatever case the tracker used, so every
// comparison must go through here and no comparison may use a name.
func NormalizeDownloadID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) != 40 && len(id) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	return strings.ToLower(id)
}

// RemovalID selects a single representative, never a list of DELETE targets.
func (j Job) RemovalID() (int64, bool) {
	if j.Truncated || j.Incomplete || len(j.ItemIDs) == 0 || len(j.MediaIDs) == 0 || len(j.ItemIDs) > MaxIDsPerJob || len(j.MediaIDs) > MaxIDsPerJob || NormalizeDownloadID(j.DownloadID) == "" {
		return 0, false
	}
	for _, id := range j.MediaIDs {
		if id <= 0 {
			return 0, false
		}
	}
	chosen := j.ItemIDs[0]
	for _, id := range j.ItemIDs {
		if id <= 0 {
			return 0, false
		}
		if id < chosen {
			chosen = id
		}
	}
	return chosen, true
}

// Group collapses queue rows into one job per download id, in ascending
// download-id order so that repeated runs log identically. Rows without a
// download id are skipped: they can never correspond to a torrent.
func Group(items []QueueItem) []Job {
	order := []string{}
	jobs := map[string]*Job{}
	for _, item := range items {
		downloadID := NormalizeDownloadID(item.DownloadID)
		if downloadID == "" || item.ID <= 0 {
			continue
		}
		job, known := jobs[downloadID]
		if !known {
			job = &Job{DownloadID: downloadID}
			jobs[downloadID] = job
			order = append(order, downloadID)
		}
		job.ItemIDs = append(job.ItemIDs, item.ID)
		if item.MediaID != nil && *item.MediaID > 0 {
			job.MediaIDs = append(job.MediaIDs, *item.MediaID)
		} else {
			job.Incomplete = true
		}
	}
	slices.Sort(order)
	grouped := make([]Job, 0, len(order))
	for _, downloadID := range order {
		grouped = append(grouped, jobs[downloadID].sealed())
	}
	return grouped
}

// Match finds the job for one qBittorrent hash. Matching is by download id
// only, never by name, and is case insensitive in both directions.
func Match(items []QueueItem, hash string) (Job, bool) {
	downloadID := NormalizeDownloadID(hash)
	if downloadID == "" {
		return Job{}, false
	}
	for _, job := range Group(items) {
		if job.DownloadID == downloadID {
			return job, true
		}
	}
	return Job{}, false
}

// sealed returns the finished job: ids deduplicated, ordered and bounded.
func (j *Job) sealed() Job {
	items, itemsTruncated := bound(j.ItemIDs)
	media, mediaTruncated := bound(j.MediaIDs)
	return Job{
		DownloadID: j.DownloadID,
		ItemIDs:    items,
		MediaIDs:   media,
		Truncated:  itemsTruncated || mediaTruncated,
		Incomplete: j.Incomplete,
	}
}

// bound sorts, deduplicates and caps a list of ids, reporting whether anything
// was left out.
func bound(ids []int64) ([]int64, bool) {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) <= MaxIDsPerJob {
		return ids, false
	}
	return ids[:MaxIDsPerJob], true
}
