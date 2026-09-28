package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

func TestAPIKeyAbsentFromPublicSurfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	c, err := config.Decode(config.YAML, []byte("qbt_url: '"+server.URL+"'\nqbt_api_key: SECRET_API_KEY\n"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := qbt.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	build := observability.NewBuild("test", "test", "test")
	metrics := observability.New(build)
	s := watchdog.New(c, client, store.File{Path: filepath.Join(t.TempDir(), "state.json"), HistoryLimit: 100}, watchdog.RealClock{}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics, build)
	if err := s.Poll(context.Background()); err == nil {
		t.Fatal("expected authentication failure")
	}
	handler := DynamicHandler(s.Config, s.Snapshot, metrics)
	for _, path := range []string{"/", "/api/v1/status", "/metrics"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "SECRET_API_KEY") {
			t.Fatal("public response failed or leaked key", path, response.Code)
		}
	}
}
