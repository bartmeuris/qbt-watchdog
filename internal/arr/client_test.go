package arr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
)

// apiKey is deliberately distinctive: every test that produces an error also
// asserts this string is absent from it.
const apiKey = "SUPERSECRET-API-KEY"

const hash = "0123456789ABCDEF0123456789abcdef01234567"

func service(t *testing.T, kind config.ArrKind, endpoint string) config.ArrService {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	return config.ArrService{
		URL: u, APIKey: apiKey, Kind: kind, Mode: config.BlocklistAndSearch,
		Timeout: 2 * time.Second, Enabled: true,
	}
}

func newClient(t *testing.T, kind config.ArrKind, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(service(t, kind, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// queuePage renders one PagingResource page of the requested size, giving
// every row its own id and its kind's own media id.
func queuePage(t *testing.T, kind config.ArrKind, first, count, total int) string {
	t.Helper()
	mediaField := "movieId"
	if kind == config.Sonarr {
		mediaField = "episodeId"
	}
	records := make([]map[string]any, 0, count)
	for i := range count {
		records = append(records, map[string]any{
			"id": first + i, "downloadId": hash, mediaField: 1000 + first + i,
			"title": "must never be used for matching",
		})
	}
	body, err := json.Marshal(map[string]any{
		"page": (first-1)/queuePageSize + 1, "pageSize": queuePageSize, "totalRecords": total, "records": records,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestQueueAuthenticatesWithHeaderAndWalksEveryPage(t *testing.T) {
	for _, kind := range []config.ArrKind{config.Sonarr, config.Radarr} {
		t.Run(string(kind), func(t *testing.T) {
			pages := []string{}
			c := newClient(t, kind, func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Api-Key"); got != apiKey {
					t.Errorf("API key header is %q", got)
				}
				if r.Header.Get("User-Agent") != userAgent {
					t.Error("missing or generic user agent")
				}
				if r.URL.Path != "/api/v3/queue" || r.Method != http.MethodGet {
					t.Error("unexpected queue request", r.Method, r.URL.Path)
				}
				unknown := "includeUnknownMovieItems"
				if kind == config.Sonarr {
					unknown = "includeUnknownSeriesItems"
				}
				if r.URL.Query().Get(unknown) != "true" {
					t.Error("queue walk did not ask for unattributed rows")
				}
				if r.URL.Query().Get("pageSize") != fmt.Sprint(queuePageSize) {
					t.Error("unexpected page size")
				}
				pages = append(pages, r.URL.Query().Get("page"))
				if r.URL.Query().Get("page") == "1" {
					io.WriteString(w, queuePage(t, kind, 1, queuePageSize, queuePageSize+50))
					return
				}
				io.WriteString(w, queuePage(t, kind, queuePageSize+1, 50, queuePageSize+50))
			})
			items, err := c.Queue(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != queuePageSize+50 {
				t.Fatal("incomplete queue walk", len(items))
			}
			if strings.Join(pages, ",") != "1,2" {
				t.Fatal("unexpected pagination", pages)
			}
			if items[0].MediaID == nil || *items[0].MediaID != 1001 {
				t.Fatal("media id was not resolved for", kind)
			}
		})
	}
}

func TestQueueLargerThanThePageLimitFailsLoudly(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, queuePage(t, config.Sonarr, 1, queuePageSize, maxQueuePages*queuePageSize+1))
	})
	_, err := c.Queue(context.Background())
	if err == nil || OutcomeOf(err) != Rejected {
		t.Fatal("an unreadable queue must not look like an empty one", err)
	}
}

func TestReverseProxyBasePathIsPreserved(t *testing.T) {
	path := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		io.WriteString(w, `{"records":[]}`)
	}))
	defer server.Close()
	c, err := New(service(t, config.Sonarr, server.URL+"/sonarr"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Queue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/sonarr/api/v3/queue" {
		t.Fatal("base path was lost", path)
	}
}

// A season pack is one queue row per episode, every row carrying the same
// download id in whatever case the tracker used.
func TestSeasonPackCollapsesIntoOneJobOfUniqueEpisodeIDs(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"page":1,"pageSize":200,"totalRecords":5,"records":[
			{"id":11,"downloadId":"`+strings.ToUpper(hash)+`","episodeId":501,"title":"S01E01"},
			{"id":12,"downloadId":"`+strings.ToLower(hash)+`","episodeId":502,"title":"S01E02"},
			{"id":13,"downloadId":"`+strings.ToLower(hash)+`","episodeId":502,"title":"duplicate"},
			{"id":14,"downloadId":"`+strings.ToLower(hash)+`","title":"unattributed"},
			{"id":21,"downloadId":"ffffffffffffffffffffffffffffffffffffffff","episodeId":900,"title":"other"}
		]}`)
	})
	items, err := c.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	job, found := Match(items, strings.ToUpper(hash))
	if !found {
		t.Fatal("case-insensitive download id did not match")
	}
	if fmt.Sprint(job.ItemIDs) != "[11 12 13 14]" {
		t.Fatal("season pack rows were not grouped", job.ItemIDs)
	}
	if fmt.Sprint(job.MediaIDs) != "[501 502]" {
		t.Fatal("episode ids were not deduplicated", job.MediaIDs)
	}
	if job.Truncated {
		t.Fatal("a small pack must not be reported as truncated")
	}
}

func TestDecodingToleratesUnknownMissingAndNullFields(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"page":1,"pageSize":200,"totalRecords":3,"anUnknownEnvelopeField":{"a":1},"records":[
			{"id":1,"downloadId":"`+hash+`","episodeId":10,"aBrandNewFieldFromAFutureRelease":[1,2]},
			{"id":2,"downloadId":"`+hash+`","episodeId":null},
			{"id":6,"downloadId":"`+hash+`","episodeId":0}
		]}`)
	})
	items, err := c.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatal("unidentifiable rows were not dropped", len(items))
	}
	if items[0].MediaID == nil || *items[0].MediaID != 10 {
		t.Fatal("unknown sibling fields disturbed decoding")
	}
	if items[1].MediaID != nil || items[2].MediaID != nil {
		t.Fatal("null and non-positive media ids must decode as absent")
	}
}

func TestMalformedQueueResponseIsNeverAnEmptyQueue(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>not json</html>")
	})
	items, err := c.Queue(context.Background())
	if err == nil || items != nil || OutcomeOf(err) != Rejected {
		t.Fatal("a broken response must not read as an empty queue", err)
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	followed := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		io.WriteString(w, `{"records":[]}`)
	}))
	defer elsewhere.Close()
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v3/queue", http.StatusFound)
	})
	_, err := c.Queue(context.Background())
	if err == nil || OutcomeOf(err) != Rejected {
		t.Fatal("a redirect must be refused, not followed", err)
	}
	if followed {
		t.Fatal("the API key was replayed at a redirect target")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatal("refusal was not explained", err)
	}
}

// A mutation that never answers is ambiguous; a read that never answers is
// merely unreachable, because asking changed nothing.
func TestTimeoutIsAmbiguousOnlyForMutations(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)
	s := service(t, config.Sonarr, server.URL)
	s.Timeout = 40 * time.Millisecond
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Queue(context.Background()); OutcomeOf(err) != Unreachable {
		t.Fatal("a timed-out read is not ambiguous", err)
	}
	if err = c.Remove(context.Background(), 7); OutcomeOf(err) != Ambiguous {
		t.Fatal("a timed-out removal may already have blocklisted a release", err)
	}
	if _, err = c.Search(context.Background(), []int64{1}); OutcomeOf(err) != Ambiguous {
		t.Fatal("a timed-out search may already be running", err)
	}
}

func TestCancellationDuringAMutationIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := c.Remove(ctx, 7); OutcomeOf(err) != Ambiguous {
		t.Fatal("shutdown mid-removal must not be recorded as untouched", err)
	}
}

func TestRemoveSendsTheAgreedParameters(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v3/queue/42" {
			t.Error("unexpected removal request", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("removeFromClient") != "false" {
			t.Error("removal must leave the torrent to the watchdog")
		}
		if query.Get("changeCategory") != "false" {
			t.Error("removal must not change the torrent category")
		}
		if query.Get("blocklist") != "true" || query.Get("skipRedownload") != "true" {
			t.Error("removal must blocklist and suppress the instance's own search")
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Remove(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if OutcomeOf(nil) != Accepted || !Accepted.Settled() {
		t.Fatal("a successful removal must be a settled, accepted outcome")
	}
}

func TestVanishedQueueItemIsNotAFailure(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
	})
	err := c.Remove(context.Background(), 42)
	if err == nil {
		t.Fatal("a vanished item must still be reported")
	}
	if OutcomeOf(err) != NotFound || !NotFound.Settled() {
		t.Fatal("a vanished item is a distinct, settled outcome", OutcomeOf(err))
	}
	if OutcomeOf(err) == Rejected {
		t.Fatal("a vanished item must not be a generic failure")
	}
}

func TestStatusCodesMapToOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status        int
		read, mutated Outcome
	}{
		{http.StatusUnauthorized, Rejected, Rejected},
		{http.StatusForbidden, Rejected, Rejected},
		{http.StatusBadRequest, Rejected, Rejected},
		{http.StatusConflict, Rejected, Rejected},
		{http.StatusNotFound, NotFound, NotFound},
		{http.StatusTooManyRequests, Unreachable, Ambiguous},
		{http.StatusInternalServerError, Unreachable, Ambiguous},
		{http.StatusBadGateway, Unreachable, Ambiguous},
		{http.StatusGatewayTimeout, Unreachable, Ambiguous},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := c.Queue(context.Background())
			if OutcomeOf(err) != tc.read {
				t.Error("read outcome", OutcomeOf(err))
			}
			if got := OutcomeOf(c.Remove(context.Background(), 1)); got != tc.mutated {
				t.Error("mutation outcome", got)
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Status != tc.status {
				t.Error("status was not preserved for the operator")
			}
		})
	}
}

func TestSearchCapturesCommandIdentityPerKind(t *testing.T) {
	for _, tc := range []struct {
		kind              config.ArrKind
		name              string
		field, otherField string
	}{
		{config.Sonarr, "EpisodeSearch", "episodeIds", "movieIds"},
		{config.Radarr, "MoviesSearch", "movieIds", "episodeIds"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			c := newClient(t, tc.kind, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v3/command" {
					t.Error("unexpected command request", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["name"] != tc.name {
					t.Error("wrong command name", body["name"])
				}
				ids, ok := body[tc.field].([]any)
				if !ok || len(ids) != 2 {
					t.Error("wrong id field for", tc.kind, body)
				}
				if _, wrong := body[tc.otherField]; wrong {
					t.Error("the other kind's id field was emitted")
				}
				io.WriteString(w, `{"id":91,"name":"`+tc.name+`","status":"Queued","unknown":true}`)
			})
			command, err := c.Search(context.Background(), []int64{501, 502})
			if err != nil {
				t.Fatal(err)
			}
			if command.ID != 91 || command.Name != tc.name || command.Status != StatusQueued {
				t.Fatal("command identity was not captured", command)
			}
			if command.Finished() || command.Succeeded() {
				t.Fatal("a queued command is not finished")
			}
		})
	}
}

func TestSearchWithoutAUsableCommandIdIsAmbiguous(t *testing.T) {
	for name, body := range map[string]string{
		"unreadable":  "not json at all",
		"missing id":  `{"name":"EpisodeSearch","status":"queued"}`,
		"unusable id": `{"id":0,"status":"queued"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, body)
			})
			_, err := c.Search(context.Background(), []int64{1})
			if OutcomeOf(err) != Ambiguous {
				t.Fatal("an accepted but unidentifiable search must not be retried blindly", err)
			}
		})
	}
}

func TestSearchRefusesUnusableIdLists(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unusable search must never reach the instance")
	})
	oversized := make([]int64, MaxIDsPerJob+1)
	for i := range oversized {
		oversized[i] = int64(i + 1)
	}
	for name, ids := range map[string][]int64{
		"empty":     {},
		"zero":      {0},
		"negative":  {-1},
		"oversized": oversized,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Search(context.Background(), ids); OutcomeOf(err) != Rejected {
				t.Fatal("expected a local refusal", err)
			}
		})
	}
}

func TestCommandStatusIsReadOnly(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/command/91" {
			t.Error("unexpected status request", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"id":91,"name":"EpisodeSearch","status":"COMPLETED"}`)
	})
	command, err := c.Status(context.Background(), 91)
	if err != nil {
		t.Fatal(err)
	}
	if command.ID != 91 || command.Status != StatusCompleted || !command.Finished() || !command.Succeeded() {
		t.Fatal("command status was not captured", command)
	}
	if _, err = c.Status(context.Background(), 0); OutcomeOf(err) != Rejected {
		t.Fatal("an invalid command id must not be sent")
	}
}

func TestCommandStatusDistinguishesFailureFromCompletion(t *testing.T) {
	for status, finished := range map[string]bool{
		StatusQueued: false, StatusStarted: false,
		StatusCompleted: true, StatusFailed: true, StatusAborted: true,
		"somethingNewInAFutureRelease": false,
	} {
		c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":1,"status":"`+status+`"}`)
		})
		command, err := c.Status(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if command.Finished() != finished || command.Succeeded() != (status == StatusCompleted) {
			t.Fatal("unexpected terminal classification for", status)
		}
	}
}

func TestHistoryFiltersByDownloadIDLocallyAsWell(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("downloadId") != strings.ToLower(hash) {
			t.Error("the server-side filter was not requested")
		}
		// An instance that ignores the filter must not be able to hand
		// back another download's history.
		io.WriteString(w, `{"page":1,"records":[
			{"id":1,"downloadId":"`+strings.ToUpper(hash)+`","eventType":"grabbed","episodeId":501,"date":"2026-09-15T10:00:00Z"},
			{"id":2,"downloadId":"ffffffffffffffffffffffffffffffffffffffff","eventType":"grabbed"},
			{"id":3,"downloadId":"`+hash+`","eventType":3,"date":"not a date","newField":1},
			{"id":4,"eventType":"grabbed"}
		]}`)
	})
	records, err := c.History(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatal("history was not filtered to the requested download", len(records))
	}
	if records[0].EventType != "grabbed" || records[0].MediaID == nil || *records[0].MediaID != 501 {
		t.Fatal("history record was not decoded", records[0])
	}
	if records[0].Date.IsZero() || !records[1].Date.IsZero() {
		t.Fatal("timestamps must decode when parseable and stay zero otherwise")
	}
	if records[1].EventType != "3" {
		t.Fatal("a numeric event type must not break the lookup", records[1].EventType)
	}
	if _, err = c.History(context.Background(), "  "); OutcomeOf(err) != Rejected {
		t.Fatal("an empty download id must not be sent")
	}
}

func TestNewRefusesAnIncompleteService(t *testing.T) {
	good := service(t, config.Sonarr, "http://sonarr.invalid")
	for name, mutate := range map[string]func(*config.ArrService){
		"no kind":    func(s *config.ArrService) { s.Kind = "" },
		"wrong kind": func(s *config.ArrService) { s.Kind = "lidarr" },
		"no url":     func(s *config.ArrService) { s.URL = nil },
		"no key":     func(s *config.ArrService) { s.APIKey = "" },
		"no timeout": func(s *config.ArrService) { s.Timeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := good.Clone()
			mutate(&candidate)
			c, err := New(candidate)
			if err == nil || c != nil {
				t.Fatal("an unusable service produced a client")
			}
			if strings.Contains(err.Error(), apiKey) {
				t.Fatal("construction leaked the API key")
			}
		})
	}
	c, err := New(good)
	if err != nil || c.Kind() != config.Sonarr || c.Mode() != config.BlocklistAndSearch {
		t.Fatal("a complete service was refused", err)
	}
	c.CloseIdleConnections()
}

// The client is immutable, so a reload rebuilds it rather than reconfiguring
// it; a client must never observe a later edit of the service it was built
// from.
func TestClientDoesNotAliasItsConfiguration(t *testing.T) {
	requested := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		io.WriteString(w, `{"records":[]}`)
	}))
	defer server.Close()
	s := service(t, config.Sonarr, server.URL)
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	s.URL.Path = "/moved/"
	if _, err = c.Queue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requested != "/api/v3/queue" {
		t.Fatal("the client followed a later edit of its configuration", requested)
	}
}
