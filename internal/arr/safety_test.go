package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"qbt-watchdog/internal/config"
)

func TestQueueRejectsIncompleteAndUnstablePages(t *testing.T) {
	for name, response := range map[string]func(int) string{
		"missing envelope":    func(int) string { return `{}` },
		"null envelope":       func(int) string { return `null` },
		"null records":        func(int) string { return `{"records":null}` },
		"wrong page":          func(int) string { return `{"page":2,"records":[]}` },
		"wrong size":          func(int) string { return `{"pageSize":1,"records":[]}` },
		"negative total":      func(int) string { return `{"totalRecords":-1,"records":[]}` },
		"short page":          func(int) string { return queuePage(t, config.Sonarr, 1, 1, 2) },
		"oversized page":      func(int) string { return queuePage(t, config.Sonarr, 1, queuePageSize+1, queuePageSize+1) },
		"missing queue id":    func(int) string { return `{"records":[{"downloadId":"` + hash + `","episodeId":1}]}` },
		"missing download id": func(int) string { return `{"records":[{"id":1,"episodeId":1}]}` },
		"conflicting identity": func(int) string {
			return `{"records":[{"id":1,"downloadId":"` + hash + `"},{"id":1,"downloadId":"ffffffffffffffffffffffffffffffffffffffff"}]}`
		},
		"changed total": func(page int) string {
			if page == 1 {
				return queuePage(t, config.Sonarr, 1, queuePageSize, queuePageSize+1)
			}
			return queuePage(t, config.Sonarr, queuePageSize+1, 2, queuePageSize+2)
		},
		"lost total": func(page int) string {
			if page == 1 {
				return queuePage(t, config.Sonarr, 1, queuePageSize, queuePageSize+1)
			}
			return `{"page":2,"records":[]}`
		},
		"overlapping rows": func(page int) string {
			body := queuePage(t, config.Sonarr, 1, queuePageSize, queuePageSize*2)
			if page == 2 {
				body = strings.Replace(body, `"page":1`, `"page":2`, 1)
			}
			return body
		},
		"no terminal page": func(page int) string {
			body := queuePage(t, config.Sonarr, (page-1)*queuePageSize+1, queuePageSize, 0)
			return strings.Replace(body, `,"totalRecords":0`, "", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				io.WriteString(w, response(page))
			})
			items, err := c.Queue(context.Background())
			if OutcomeOf(err) != Rejected || items != nil {
				t.Fatalf("unsafe queue accepted: %v", err)
			}
			if calls.Load() > maxQueuePages {
				t.Fatal("page limit exceeded")
			}
		})
	}
}

func TestBoundedResponsesPreserveMutationAmbiguity(t *testing.T) {
	for _, operation := range []string{"queue", "search"} {
		t.Run(operation, func(t *testing.T) {
			c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, strings.Repeat(" ", maxResponse+1))
			})
			_, err := c.Queue(context.Background())
			want := Rejected
			if operation == "search" {
				_, err = c.Search(context.Background(), []int64{1})
				want = Ambiguous
			}
			if OutcomeOf(err) != want {
				t.Fatalf("outcome=%s want=%s", OutcomeOf(err), want)
			}
		})
	}
}

func TestDroppedMutationResponseIsAmbiguousWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	_, err := c.Search(context.Background(), []int64{1})
	if OutcomeOf(err) != Ambiguous || calls.Load() != 1 {
		t.Fatalf("POST was retried or classified as safe: %v", err)
	}
	err = c.Remove(context.Background(), 1)
	if OutcomeOf(err) != Ambiguous || calls.Load() != 2 {
		t.Fatalf("DELETE was retried or classified as safe: %v", err)
	}
}

func TestCommandsAcceptSuccessfulStatusesAndDeduplicateIDs(t *testing.T) {
	for _, status := range []int{200, 201, 202} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c := newClient(t, config.Radarr, func(w http.ResponseWriter, r *http.Request) {
				var body commandRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body.Name != "MoviesSearch" || !slices.Equal(body.MovieIDs, []int64{1, 3}) {
					t.Error("incorrect targeted IDs", body)
				}
				w.WriteHeader(status)
				io.WriteString(w, `{"id":12,"status":"queued","outputPath":"private","magnet":"private"}`)
			})
			ids := []int64{3, 1, 3}
			command, err := c.Search(context.Background(), ids)
			if err != nil || command.ID != 12 || !slices.Equal(ids, []int64{3, 1, 3}) {
				t.Fatalf("command=%v err=%v", command, err)
			}
		})
	}
}

func TestCommandIdentityAndStatusFailClosed(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"id":1}`, `{"id":1,"status":null}`, `{"id":-1,"status":"queued"}`} {
		c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
		if _, err := c.Search(context.Background(), []int64{1}); OutcomeOf(err) != Ambiguous {
			t.Fatalf("POST accepted unusable command: %v", err)
		}
		if _, err := c.Status(context.Background(), 1); OutcomeOf(err) != Rejected {
			t.Fatalf("GET accepted unusable command: %v", err)
		}
	}
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"id":2,"status":"completed"}`) })
	if _, err := c.Status(context.Background(), 1); OutcomeOf(err) != Rejected {
		t.Fatal("wrong command ID accepted")
	}
}

func TestSeasonPackRemovalMakesExactlyOneCall(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v3/queue/11" || r.Method != http.MethodDelete {
			t.Error("wrong representative")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	job, found := c.Map(rows(t, [2]int64{12, 8}, [2]int64{11, 9}, [2]int64{12, 8}), hash)
	if !found || !slices.Equal(job.MediaIDs, []int64{8, 9}) {
		t.Fatal("pack mapping failed")
	}
	if err := c.RemoveJob(context.Background(), job); err != nil || calls.Load() != 1 {
		t.Fatalf("removal failed: %v", err)
	}
	for _, unsafe := range []Job{
		{DownloadID: hash, ItemIDs: []int64{11}, MediaIDs: []int64{8}, Truncated: true},
		{DownloadID: hash, ItemIDs: []int64{11}, MediaIDs: []int64{8}, Incomplete: true},
		{DownloadID: hash, ItemIDs: []int64{11}, MediaIDs: []int64{-1}},
	} {
		if err := c.RemoveJob(context.Background(), unsafe); OutcomeOf(err) != Rejected {
			t.Fatal("unsafe job accepted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unsafe job sent a mutation")
	}
}

func TestMappingValidatesHashesAndMissingMedia(t *testing.T) {
	for _, invalid := range []string{"episode title", "magnet:?xt=urn:btih:" + hash, strings.Repeat("g", 40), "abc"} {
		if _, found := Match([]QueueItem{{ID: 1, DownloadID: invalid}}, invalid); found {
			t.Fatal("non-hash matched")
		}
	}
	v2 := strings.Repeat("AB", 32)
	if NormalizeDownloadID(v2) != strings.ToLower(v2) {
		t.Fatal("v2 hash rejected")
	}
	job, found := Match(rows(t, [2]int64{1, 8}, [2]int64{2, -1}, [2]int64{3, 0}), hash)
	if !found || !job.Incomplete || !slices.Equal(job.MediaIDs, []int64{8}) {
		t.Fatal("incomplete identity lost")
	}
	if _, ok := job.RemovalID(); ok {
		t.Fatal("incomplete job is actionable")
	}
}

func TestClientBoundaryRejectsUnsafeConfiguration(t *testing.T) {
	for _, endpoint := range []string{"http://user:secret@host/", "http://host/?apikey=secret", "ftp://host/", "http://host:99999/"} {
		u, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		s := service(t, config.Sonarr, "http://unused.invalid")
		s.URL = u
		if _, err = New(s); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe URL accepted or leaked")
		}
	}
	s := service(t, config.Sonarr, "http://unused.invalid")
	s.APIKey = "secret\r\nInjected: yes"
	if _, err := New(s); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("unsafe key accepted or leaked")
	}
}

func TestClientPreservesEscapedProxyPathWithoutTrailingSlash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/proxy%2Ftenant/sonarr/api/v3/queue" {
			t.Error("escaped base path lost", r.URL.EscapedPath())
		}
		io.WriteString(w, `{"records":[]}`)
	}))
	defer server.Close()
	s := service(t, config.Sonarr, server.URL)
	s.URL, _ = url.Parse(server.URL + "/proxy%2Ftenant/sonarr")
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	if _, err := c.Queue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientLogsAndReducedResultsNeverEchoServerSecrets(t *testing.T) {
	var logs bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/history" {
			io.WriteString(w, `{"records":[{"id":1,"downloadId":"`+hash+`","eventType":"`+apiKey+`","outputPath":"`+apiKey+`"}]}`)
			return
		}
		io.WriteString(w, `{"id":1,"name":"`+apiKey+`","status":"`+apiKey+`","body":{"magnet":"`+apiKey+`"}}`)
	}))
	defer server.Close()
	c, err := New(service(t, config.Sonarr, server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	command, err := c.Search(context.Background(), []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	history, err := c.History(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("client", "client", c)
	encoded, err := json.Marshal([]any{command, history, c})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded)+logs.String()+fmt.Sprintf("%+v %#v", c, *c), apiKey) {
		t.Fatal("secret escaped reduction boundary")
	}
}

func TestHistoryIsReadOnlyAndRejectsTruncation(t *testing.T) {
	c := newClient(t, config.Radarr, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/history" {
			t.Error("history mutation attempted")
		}
		io.WriteString(w, `{"page":1,"pageSize":50,"totalRecords":51,"records":[]}`)
	})
	if _, err := c.History(context.Background(), hash); OutcomeOf(err) != Rejected {
		t.Fatal("truncated history accepted")
	}
}

func TestWireIdentityFieldsDoNotCoerceNumbersToStrings(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/command" {
			io.WriteString(w, `{"id":1,"status":123}`)
			return
		}
		io.WriteString(w, `{"records":[{"id":1,"downloadId":1111111111111111111111111111111111111111,"episodeId":2}]}`)
	})
	if _, err := c.Queue(context.Background()); OutcomeOf(err) != Rejected {
		t.Fatal("numeric hash accepted")
	}
	if _, err := c.History(context.Background(), hash); OutcomeOf(err) != Rejected {
		t.Fatal("numeric history hash accepted")
	}
	if _, err := c.Search(context.Background(), []int64{1}); OutcomeOf(err) != Ambiguous {
		t.Fatal("numeric command status accepted")
	}
}
