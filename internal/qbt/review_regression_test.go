package qbt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDeleteReauthenticationDoesNotReplayMutation(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var logins, deletes atomic.Int32
			var downloading atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					if logins.Add(1) == 2 {
						downloading.Store(true)
					}
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session", Path: "/"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/info":
					state := "metaDL"
					if downloading.Load() {
						state = "downloading"
					}
					_ = json.NewEncoder(w).Encode([]Torrent{{Hash: hash, State: state}})
				case "/api/v2/torrents/delete":
					if r.Method != http.MethodPost {
						t.Error("mutation is not POST")
					}
					if deletes.Add(1) == 1 {
						w.WriteHeader(status)
					}
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c := client(t, server, true)
			before, err := c.Get(context.Background(), hash)
			if err != nil || before == nil || before.State != "metaDL" {
				t.Fatal("initial safety read failed", before, err)
			}
			if err := c.Delete(context.Background(), hash, false); err == nil {
				t.Fatal("mutation was replayed instead of returning for revalidation")
			}
			if logins.Load() != 2 || deletes.Load() != 1 {
				t.Fatal("unbounded authentication or stale mutation replay", logins.Load(), deletes.Load())
			}
			after, err := c.Get(context.Background(), hash)
			if err != nil || after == nil || after.State != "downloading" || logins.Load() != 2 {
				t.Fatal("renewed session did not expose changed state", after, err)
			}
		})
	}
}
