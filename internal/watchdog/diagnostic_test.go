package watchdog

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
)

// rejected builds a typed validation error shaped like the one a real
// torrents/info response produces.
func rejected() *qbt.ResponseError {
	return &qbt.ResponseError{
		Code:      qbt.CodeSizeExceedsTotal,
		Operation: "torrents/info",
		Index:     7,
		ShortHash: "abc123def456",
		Field:     "size",
		Value:     "123",
		Related:   "0",
	}
}

func serviceWithLog(t *testing.T, log *slog.Logger) (*Service, *fakeClient, *fakeClock) {
	t.Helper()
	c, err := config.Decode(config.YAML, []byte("qbt_url: 'http://localhost'\npoll_interval: '10s'\nmax_observation_gap: '30s'\ndelete_confirmation_timeout: '20s'"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range config.PolicyIDs() {
		policy := c.Policies[id]
		policy.Action = config.Delete
		policy.Threshold = 20 * time.Second
		c.Policies[id] = policy
	}
	clock := &fakeClock{time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}
	client := &fakeClient{torrents: []qbt.Torrent{torrent(hashA)}}
	disk := &memoryStore{}
	build := observability.NewBuild("test", "test", "test")
	service := New(c, client, disk, clock, log, observability.New(build), build)
	return service, client, clock
}

func TestRejectedInitialListPublishesDiagnosticWithoutAcceptedSnapshot(t *testing.T) {
	s, c, _, _ := fixture(t)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	snap := s.Snapshot()
	if snap.QBTUp || snap.PollError == "" {
		t.Fatal("failure not published")
	}
	if snap.LastTorrentListSuccess != nil {
		t.Fatal("rejected list recorded as an accepted snapshot")
	}
	if snap.TorrentDataStale {
		t.Fatal("stale without a prior accepted list")
	}
	d := snap.PollDiagnostic
	if d == nil || d.Kind != PollErrorResponseRejected || d.Stage != PollStageInitialList {
		t.Fatalf("diagnostic not published: %+v", d)
	}
	if d.Code != qbt.CodeSizeExceedsTotal || d.Field != "size" || d.Torrent != "abc123def456" {
		t.Fatalf("diagnostic fields lost: %+v", d)
	}
	if d.Index == nil || *d.Index != 7 {
		t.Fatalf("diagnostic index lost: %+v", d)
	}
}

func TestRejectedSubsequentListRetainsRowsAndTimestamp(t *testing.T) {
	s, c, clock, _ := fixture(t)
	poll(t, s)
	first := s.Snapshot()
	if first.LastTorrentListSuccess == nil {
		t.Fatal("accepted list did not record a timestamp")
	}
	rows := len(first.Torrents)
	clock.Advance(10 * time.Second)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	snap := s.Snapshot()
	if !snap.TorrentDataStale {
		t.Fatal("rejected subsequent list not marked stale")
	}
	if snap.LastTorrentListSuccess == nil || !snap.LastTorrentListSuccess.Equal(*first.LastTorrentListSuccess) {
		t.Fatal("accepted-list timestamp changed on rejection")
	}
	if len(snap.Torrents) != rows {
		t.Fatalf("rows lost on rejection: %d -> %d", rows, len(snap.Torrents))
	}
}

func TestRecoveryClearsDiagnosticAndRefreshes(t *testing.T) {
	s, c, clock, _ := fixture(t)
	poll(t, s)
	clock.Advance(10 * time.Second)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	c.listError = nil
	poll(t, s)
	snap := s.Snapshot()
	if snap.PollError != "" || snap.PollDiagnostic != nil || snap.TorrentDataStale || !snap.QBTUp {
		t.Fatalf("recovery did not clear the diagnostic: %+v", snap)
	}
	if snap.LastTorrentListSuccess == nil {
		t.Fatal("recovery did not refresh the accepted-list timestamp")
	}
}

func TestVersionsRecordedDespiteListFailure(t *testing.T) {
	s, c, _, _ := fixture(t)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	snap := s.Snapshot()
	if snap.QBTVersion != "5.0.1" || snap.WebAPIVersion != "2.11.2" {
		t.Fatalf("versions suppressed by list failure: %q %q", snap.QBTVersion, snap.WebAPIVersion)
	}
}

func TestEndpointChangeClearsAcceptedListAndDiagnostic(t *testing.T) {
	s, c, _, _ := fixture(t)
	poll(t, s)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	next := s.c.Clone()
	next.URL, _ = url.Parse("http://otherhost:8080")
	if err := s.Reload(next, func(config.Config) (Client, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.LastTorrentListSuccess != nil || snap.TorrentDataStale || snap.PollError != "" || snap.PollDiagnostic != nil {
		t.Fatalf("endpoint change retained old diagnostic: %+v", snap)
	}
	if snap.QBTVersion != "" || snap.WebAPIVersion != "" {
		t.Fatalf("endpoint change retained old versions: %q %q", snap.QBTVersion, snap.WebAPIVersion)
	}
	if len(snap.Torrents) != 0 {
		t.Fatalf("endpoint change retained old rows: %d", len(snap.Torrents))
	}
}

func TestRejectedInitialListPerformsNoMutation(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	s.c.TagSync.Enabled = true
	poll(t, s)
	clock.Advance(20 * time.Second)
	deletes, adds, removes := len(c.deletes), len(c.adds), len(c.removes)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	if len(c.deletes) != deletes || len(c.adds) != adds || len(c.removes) != removes {
		t.Fatalf("rejected list mutated qBittorrent: deletes=%v adds=%v removes=%v", c.deletes, c.adds, c.removes)
	}
	if s.state.Counters.Deletions != 0 {
		t.Fatal("rejected list produced a false removal confirmation")
	}
}

func TestRejectedListDoesNotConfirmPendingDeletion(t *testing.T) {
	s, c, clock, _ := fixture(t)
	s.c.DryRun = false
	poll(t, s)
	clock.Advance(20 * time.Second)
	poll(t, s)
	if s.state.Tracked[hashA].DeleteRequestedAt == nil {
		t.Fatal("no pending deletion to protect")
	}
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	if s.state.Counters.Deletions != 0 {
		t.Fatal("rejected list confirmed a pending deletion")
	}
	if s.state.Tracked[hashA].DeleteRequestedAt == nil {
		t.Fatal("rejected list dropped the pending deletion")
	}
}

func TestPollFailureLogCarriesStructuredDiagnostic(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	s, c, _ := serviceWithLog(t, log)
	c.listError = rejected()
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	out := buf.String()
	for _, want := range []string{
		"event=poll_error",
		"error_kind=response_rejected",
		"poll_stage=initial_list",
		"field=size",
		"torrent=abc123def456",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestGenericFailureIsNotClassifiedAsResponseRejected(t *testing.T) {
	s, c, _, _ := fixture(t)
	c.listError = context.DeadlineExceeded
	if s.Poll(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	d := s.Snapshot().PollDiagnostic
	if d == nil || d.Kind != PollErrorPollFailed || d.Stage != PollStageInitialList {
		t.Fatalf("generic failure misclassified: %+v", d)
	}
}
