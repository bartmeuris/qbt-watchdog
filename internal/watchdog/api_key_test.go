package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
)

func TestAPIKeyRotationRebuildsClientAndRestartsTimers(t *testing.T) {
	for _, source := range []string{"secret-file", "dotenv"} {
		t.Run(source, func(t *testing.T) { testAPIKeyRotationRebuildsClientAndRestartsTimers(t, source) })
	}
}

func testAPIKeyRotationRebuildsClientAndRestartsTimers(t *testing.T, source string) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	key := "SECRET_ORIGINAL_KEY"
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v2/app/version":
			io.WriteString(w, "v5.2.0")
		case "/api/v2/app/webapiVersion":
			io.WriteString(w, "2.14.1")
		case "/api/v2/torrents/info":
			json.NewEncoder(w).Encode([]qbt.Torrent{{Hash: hashA, State: "stalledDL", Progress: .5, Downloaded: 1024, NumSeeds: 1}})
		default:
			t.Error("unexpected endpoint during rotation")
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	keyFile, configFile := filepath.Join(dir, "key"), filepath.Join(dir, "config.yaml")
	keyContents := func(key string) string {
		if source == "dotenv" {
			return "QBTW_TEST_API_KEY='" + key + "'\nUNUSED_SECRET_METADATA=SECRET_UNUSED_VALUE\n"
		}
		return key + "\n"
	}
	if source == "dotenv" {
		keyFile = filepath.Join(dir, ".env")
	}
	if err := os.WriteFile(keyFile, []byte(keyContents(key)), 0600); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("qbt_url: %q\nqbt_api_key_file: %q\npoll_interval: 10s\n", server.URL, keyFile)
	if source == "dotenv" {
		data = fmt.Sprintf("qbt_url: %q\nqbt_api_key: '${QBTW_TEST_API_KEY}'\npoll_interval: 10s\n", server.URL)
	}
	if err := os.WriteFile(configFile, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadWithEnvironment(configFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(c config.Config) (Client, error) { return qbt.New(c, log) }
	client, err := prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Now().UTC()}
	disk := &memoryStore{}
	build := testBuild()
	metrics := observability.New(build)
	s := New(c, client, disk, clock, log, metrics, build)
	poll(t, s)
	clock.Advance(10 * time.Second)
	poll(t, s)
	if s.state.Tracked[hashA].Policy != config.StalledPartial {
		t.Fatal("partial payload was rejected")
	}
	requested := clock.Now()
	e := s.state.Tracked[hashA]
	e.Attempts, e.DeleteRequestedAt, e.DryRunNotified = 2, &requested, true
	s.state.Tracked[hashA] = e
	oldClient := s.client
	clock.Advance(time.Second)
	key = "SECRET_ROTATED_KEY"
	if err := os.WriteFile(keyFile, []byte(keyContents(key)), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := config.LoadWithEnvironment(configFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(next, prepare); err != nil {
		t.Fatal(err)
	}
	e = s.state.Tracked[hashA]
	if s.client == oldClient || !e.FirstSeen.Equal(clock.Now()) || e.Policy != "" || e.DryRunNotified || e.Attempts != 2 || e.DeleteRequestedAt == nil || !e.DeleteRequestedAt.Equal(requested) || !s.state.SeedObserved[hashA] {
		t.Fatal("rotation lost safety state or inherited elapsed time")
	}
	poll(t, s)
	if len(requests) != 9 {
		t.Fatal("unexpected requests", len(requests))
	}
	for i, authorization := range requests {
		want := "Bearer SECRET_ORIGINAL_KEY"
		if i >= 6 {
			want = "Bearer SECRET_ROTATED_KEY"
		}
		if authorization != want {
			t.Fatal("client did not adopt rotated key")
		}
	}
	restarted := next.Clone()
	restarted.APIKey = "SECRET_RESTARTED_KEY"
	clock.Advance(time.Second)
	resumed := New(restarted, s.client, disk, clock, log, observability.New(build), build)
	if !resumed.state.Tracked[hashA].FirstSeen.Equal(clock.Now()) || resumed.state.Tracked[hashA].DeleteRequestedAt == nil {
		t.Fatal("restart inherited credential-era timers or lost pending deletion")
	}
	for _, payload := range []any{s.Snapshot(), disk.state} {
		encoded, err := json.Marshal(payload)
		if err != nil || bytes.Contains(encoded, []byte("SECRET_")) {
			t.Fatal("public or persistent state leaked key", err)
		}
	}
	if strings.Contains(logs.String(), "SECRET_") {
		t.Fatal("logs leaked API key")
	}
}

func TestBearerPollAuthenticationFailureIsSanitized(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				io.WriteString(w, r.Header.Get("Authorization"))
			}))
			defer server.Close()
			c, err := config.Decode(config.YAML, []byte("qbt_url: '"+server.URL+"'\nqbt_api_key: SECRET_REJECTED_KEY\n"))
			if err != nil {
				t.Fatal(err)
			}
			client, err := qbt.New(c)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			var logs bytes.Buffer
			build := testBuild()
			disk := &memoryStore{}
			s := New(c, client, disk, RealClock{}, slog.New(slog.NewTextHandler(&logs, nil)), observability.New(build), build)
			err = s.Poll(context.Background())
			if err == nil || calls != 1 || s.Snapshot().QBTUp {
				t.Fatal("authentication failure accepted or retried")
			}
			encoded, _ := json.Marshal(s.Snapshot())
			stored, _ := json.Marshal(disk.state)
			if strings.Contains(err.Error()+logs.String()+string(encoded)+string(stored), "SECRET_REJECTED_KEY") {
				t.Fatal("authentication failure leaked key")
			}
		})
	}
}

func TestAuthenticationFailureUsesBoundedServiceBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, client, _, _ := fixture(t)
		s.clock = RealClock{}
		client.versionError = errors.New("qBittorrent API-key authentication failed")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx)
		synctest.Wait()
		for attempt := 1; attempt <= 10; attempt++ {
			snapshot := s.Snapshot()
			if snapshot.NextPoll == nil || snapshot.QBTUp {
				t.Fatal("failed poll did not schedule backoff")
			}
			delay := snapshot.NextPoll.Sub(time.Now())
			ceiling := min(10*time.Second*time.Duration(1<<min(attempt, 6)), 5*time.Minute)
			if delay < ceiling*3/4 || delay > ceiling {
				t.Fatal("authentication backoff out of bounds", delay, ceiling)
			}
			time.Sleep(delay)
			synctest.Wait()
		}
		cancel()
		synctest.Wait()
		if len(client.deletes) != 0 || s.state.Counters != (store.Counters{}) {
			t.Fatal("authentication failure mutated torrents")
		}
	})
}
