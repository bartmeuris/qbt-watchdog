package qbt

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
)

const testAPIKey = "SECRET_NATIVE_API_KEY"

func bearerClient(t *testing.T, server *httptest.Server, logs io.Writer) *Client {
	t.Helper()
	c, err := config.Decode(config.YAML, []byte("qbt_url: '"+server.URL+"/qbt/'\nqbt_api_key: "+testAPIKey+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := New(c, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.CloseIdleConnections)
	return cl
}

func TestBearerEveryEndpointWithoutSession(t *testing.T) {
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts[r.URL.Path]++
		if r.Header.Get("Authorization") != "Bearer "+testAPIKey || r.Header.Get("Cookie") != "" {
			t.Error("missing bearer or unexpected session credentials")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.String()+string(body), testAPIKey) {
			t.Error("API key leaked outside Authorization")
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "ignored", Path: "/"})
		switch r.URL.Path {
		case "/qbt/api/v2/app/version":
			io.WriteString(w, "v5.2.0")
		case "/qbt/api/v2/app/webapiVersion":
			io.WriteString(w, "2.14.1")
		case "/qbt/api/v2/torrents/info":
			io.WriteString(w, `[{"hash":"`+hash+`","state":"metaDL","progress":0,"downloaded":0,"num_seeds":0}]`)
		case "/qbt/api/v2/torrents/delete":
			if r.Method != http.MethodPost || !strings.Contains(string(body), "deleteFiles=false") {
				t.Error("incorrect deletion request")
			}
		default:
			t.Error("unexpected endpoint, including login")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := bearerClient(t, server, io.Discard)
	if _, _, err := c.Versions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), hash, false); err != nil {
		t.Fatal(err)
	}
	if c.http.Jar != nil || c.loggedIn || len(counts) != 4 || counts["/qbt/api/v2/torrents/info"] != 2 {
		t.Fatal("bearer requests unexpectedly used session state")
	}
}

func TestBearerAuthFailureNeverReplays(t *testing.T) {
	for _, status := range []int{401, 403} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(strconv.Itoa(status)+method, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if strings.HasSuffix(r.URL.Path, "login") {
						t.Error("bearer attempted login")
					}
					w.WriteHeader(status)
					io.WriteString(w, testAPIKey)
				}))
				defer server.Close()
				var logs bytes.Buffer
				c := bearerClient(t, server, &logs)
				var err error
				if method == http.MethodGet {
					_, err = c.List(context.Background())
				} else {
					err = c.Delete(context.Background(), hash, true)
				}
				if err == nil || calls != 1 || c.http.Jar != nil || strings.Contains(err.Error()+logs.String(), testAPIKey) {
					t.Fatal("authentication retried, used session state or leaked credentials", err)
				}
			})
		}
	}
}

func TestBearerRedirectNeverFollowed(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect leaked bearer") }))
			defer target.Close()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				http.Redirect(w, r, target.URL+"/"+testAPIKey, status)
			}))
			defer server.Close()
			c := bearerClient(t, server, io.Discard)
			err := c.Delete(context.Background(), hash, false)
			if err == nil || calls != 1 || strings.Contains(err.Error(), testAPIKey) {
				t.Fatal("redirect was accepted or leaked", err)
			}
		})
	}
}
