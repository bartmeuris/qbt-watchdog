package arr

import (
	"fmt"
	"strings"
	"testing"
)

func rows(t *testing.T, spec ...[2]int64) []QueueItem {
	t.Helper()
	items := make([]QueueItem, 0, len(spec))
	for _, entry := range spec {
		item := QueueItem{ID: entry[0], DownloadID: hash}
		if entry[1] != 0 {
			media := entry[1]
			item.MediaID = &media
		}
		items = append(items, item)
	}
	return items
}

func TestMatchIsCaseInsensitiveInBothDirections(t *testing.T) {
	items := []QueueItem{{ID: 1, DownloadID: strings.ToUpper(hash), MediaID: nil}}
	for _, query := range []string{strings.ToLower(hash), strings.ToUpper(hash), "  " + hash + "  "} {
		if _, found := Match(items, query); !found {
			t.Fatal("download id matching is case or whitespace sensitive", query)
		}
	}
	if _, found := Match(items, "ffffffffffffffffffffffffffffffffffffffff"); found {
		t.Fatal("a different download id matched")
	}
	if _, found := Match(items, ""); found {
		t.Fatal("an empty hash matched something")
	}
	if _, found := Match(nil, hash); found {
		t.Fatal("an empty queue matched something")
	}
}

// The only identity is the download id. A row that shares a name but not a
// hash belongs to a different torrent, and acting on it would blocklist
// somebody else's release.
func TestMatchingNeverConsidersAName(t *testing.T) {
	if strings.Contains(fmt.Sprintf("%+v", QueueItem{}), "Name") {
		t.Fatal("QueueItem carries a name, which makes name matching possible")
	}
	if strings.Contains(fmt.Sprintf("%+v", Job{}), "Name") {
		t.Fatal("Job carries a name, which makes name matching possible")
	}
}

func TestGroupIsDeterministicAndDropsUnusableRows(t *testing.T) {
	other := "ffffffffffffffffffffffffffffffffffffffff"
	media := int64(7)
	items := []QueueItem{
		{ID: 3, DownloadID: other, MediaID: &media},
		{ID: 1, DownloadID: strings.ToUpper(hash), MediaID: &media},
		{ID: 0, DownloadID: hash},
		{ID: 5, DownloadID: "   "},
		{ID: 2, DownloadID: hash},
	}
	first := Group(items)
	if len(first) != 2 {
		t.Fatal("unusable rows were not dropped", len(first))
	}
	if first[0].DownloadID != strings.ToLower(hash) || first[1].DownloadID != other {
		t.Fatal("jobs are not in a stable order", first[0].DownloadID)
	}
	if fmt.Sprint(first[0].ItemIDs) != "[1 2]" {
		t.Fatal("rows were not grouped and ordered", first[0].ItemIDs)
	}
	if fmt.Sprint(Group(items)) != fmt.Sprint(first) {
		t.Fatal("grouping is not deterministic")
	}
}

func TestJobIsBoundedAndReportsTruncation(t *testing.T) {
	spec := make([][2]int64, 0, MaxIDsPerJob+10)
	for i := range MaxIDsPerJob + 10 {
		spec = append(spec, [2]int64{int64(i + 1), int64(i + 1000)})
	}
	job, found := Match(rows(t, spec...), hash)
	if !found {
		t.Fatal("a large pack did not match")
	}
	if len(job.ItemIDs) != MaxIDsPerJob || len(job.MediaIDs) != MaxIDsPerJob {
		t.Fatal("a job exceeded the bound", len(job.ItemIDs), len(job.MediaIDs))
	}
	if !job.Truncated {
		t.Fatal("a partially handled release was not flagged")
	}
	small, _ := Match(rows(t, [2]int64{1, 10}), hash)
	if small.Truncated {
		t.Fatal("a small release was flagged as truncated")
	}
}

// A pack whose rows the media manager never attributed still has rows worth
// removing, so an empty media list must not suppress the job.
func TestUnattributedRowsStillFormAJob(t *testing.T) {
	job, found := Match(rows(t, [2]int64{1, 0}, [2]int64{2, 0}), hash)
	if !found || len(job.ItemIDs) != 2 || len(job.MediaIDs) != 0 {
		t.Fatal("unattributed rows were discarded", job)
	}
}

func TestNormalizeDownloadID(t *testing.T) {
	for input, want := range map[string]string{
		strings.ToUpper(hash): strings.ToLower(hash),
		" " + hash + "\n":     strings.ToLower(hash),
		"":                    "",
		"   ":                 "",
	} {
		if got := NormalizeDownloadID(input); got != want {
			t.Fatal("unexpected normalisation", got)
		}
	}
}
