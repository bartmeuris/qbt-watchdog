package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/watchdog"
)

// testBuild returns a deterministic Build for tests that only need the
// observability surface populated.
func testBuild() observability.Build {
	return observability.Build{Version: "test", Revision: "test", CommitTime: "test", GoVersion: runtime.Version()}
}

func fixture(t *testing.T) (config.Config, *watchdog.Snapshot, *observability.Metrics) {
	t.Helper()
	c, e := config.Decode(config.YAML, []byte("qbt_url: 'http://localhost'"))
	if e != nil {
		t.Fatal(e)
	}
	build := testBuild()
	now := time.Now().UTC()
	s := &watchdog.Snapshot{SchemaVersion: 1, Build: build, DryRun: true, LastTorrentListSuccess: &now, Torrents: []watchdog.Row{{Name: `<script>alert("x")</script>`, ShortHash: "aaaaaaaaaaaa", State: "metaDL", Decision: "tracking"}}, History: nil}
	return c, s, observability.New(build)
}
func request(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestHealthReadinessAndHardening(t *testing.T) {
	c, s, m := fixture(t)
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	for _, path := range []string{"/", "/api/v1/status"} {
		w := request(h, path)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(path, w.Code)
		}
		for key, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
			if w.Header().Get(key) != want {
				t.Fatal(key)
			}
		}
		if strings.Contains(w.Header().Get("Content-Security-Policy"), "unsafe-inline") || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("weak CSP")
		}
	}
	// /metrics is always public now that built-in web auth is gone.
	if request(h, "/healthz").Code != 200 || request(h, "/readyz").Code != 503 || request(h, "/metrics").Code != 200 {
		t.Fatal("endpoint defaults")
	}
	now := time.Now().UTC()
	s.LastSuccess = &now
	if request(h, "/readyz").Code != 200 {
		t.Fatal("not ready after successful poll")
	}
	s.PersistenceError = "unavailable"
	if request(h, "/readyz").Code != 503 || request(h, "/healthz").Code != 200 {
		t.Fatal("persistence health transitions")
	}
	s.PersistenceError = ""
	past := now.Add(-time.Hour)
	s.LastSuccess = &past
	if request(h, "/readyz").Code != 503 {
		t.Fatal("stale ready")
	}
}
func TestEscapingStatusAndEmbeddedAssets(t *testing.T) {
	c, s, m := fixture(t)
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	html := request(h, "/").Body.String()
	torrents := request(h, "/partials/torrents").Body.String()
	if strings.Contains(torrents, s.Torrents[0].Name) || !strings.Contains(torrents, "&lt;script&gt;") {
		t.Fatal("unescaped hostile name")
	}
	w := request(h, "/api/v1/status")
	var decoded watchdog.Snapshot
	if e := json.Unmarshal(w.Body.Bytes(), &decoded); e != nil {
		t.Fatal(e)
	}
	if decoded.SchemaVersion != 1 || decoded.Torrents[0].Name != s.Torrents[0].Name || strings.Contains(w.Body.String(), "SECRET_PASSWORD") {
		t.Fatal("bad status")
	}
	js := request(h, "/assets/app.js")
	css := request(h, "/assets/style.css")
	if js.Code != 200 || css.Code != 200 || strings.Contains(js.Body.String(), "innerHTML") || strings.Contains(html, "https://") {
		t.Fatal("unsafe/nonembedded assets")
	}
	r := httptest.NewRequest("POST", "/api/v1/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatal("mutation method accepted")
	}
}
func TestMetricsIsolationValuesAndBoundedLabels(t *testing.T) {
	c, s, m := fixture(t)
	m.Up.Set(1)
	m.Total.Set(5)
	m.Actions.WithLabelValues("would_delete", "success", "true").Inc()
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	body := request(h, "/metrics").Body.String()
	for _, want := range []string{"qbt_watchdog_qbt_up 1", "qbt_watchdog_torrents_total 5", `qbt_watchdog_actions_total{action="would_delete",dry_run="true",outcome="success"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatal("missing metric", want)
		}
	}
	for _, forbidden := range []string{"aaaaaaaaaaaa", "alert(", "SECRET_PASSWORD", "category=", "hash=", "name="} {
		if strings.Contains(body, forbidden) {
			t.Fatal("sensitive label", forbidden)
		}
	}
	other := observability.New(s.Build)
	families, e := other.Registry.Gather()
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range families {
		if f.GetName() == "qbt_watchdog_qbt_up" && f.Metric[0].Gauge.GetValue() != 0 {
			t.Fatal("global registry leaked")
		}
	}
}
func TestServerTimeouts(t *testing.T) {
	c, s, m := fixture(t)
	server := New(c, func() watchdog.Snapshot { return *s }, m)
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes > 65536 {
		t.Fatal("unhardened server")
	}
}
