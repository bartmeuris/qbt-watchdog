package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/watchdog"
)

//go:embed assets/*
var assets embed.FS

//go:embed templates/*
var templates embed.FS

func New(c config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics) *http.Server {
	return &http.Server{Addr: c.Listen, Handler: Handler(c, snapshot, metrics), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
}

func Handler(c config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics) http.Handler {
	return DynamicHandler(func() config.Config { return c }, snapshot, metrics)
}

func DynamicHandler(configuration func() config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics) http.Handler {
	return DynamicHandlerWithConfig(configuration, snapshot, metrics, nil)
}

func DynamicHandlerWithConfig(configuration func() config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics, saver ConfigSaver) http.Handler {
	return DynamicHandlerWithActions(configuration, snapshot, metrics, saver, nil)
}

func DynamicHandlerWithActions(configuration func() config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics, saver ConfigSaver, actions ManualActions) http.Handler {
	page := template.Must(template.New("base").Funcs(newViewFuncs()).ParseFS(templates, "templates/*.html"))
	mux := http.NewServeMux()
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := configuration()
			if c.WebUsername != "" {
				user, pass, ok := r.BasicAuth()
				u, p := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
				expectedU, expectedP := sha256.Sum256([]byte(c.WebUsername)), sha256.Sum256([]byte(c.WebPassword))
				matched := subtle.ConstantTimeCompare(u[:], expectedU[:]) & subtle.ConstantTimeCompare(p[:], expectedP[:])
				if !ok || matched != 1 {
					w.Header().Set("WWW-Authenticate", `Basic realm="qbt-watchdog", charset="UTF-8"`)
					jsonResponse(w, 401, map[string]string{"error": "authentication required"})
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	renderPage := func(name string) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_ = page.ExecuteTemplate(w, "base", buildPageView(snapshot(), name))
		}))
	}
	mux.Handle("GET /{$}", renderPage("overview"))
	mux.Handle("GET /policies", renderPage("policies"))
	mux.Handle("GET /activity", renderPage("activity"))
	mux.Handle("GET /settings", renderPage("settings"))
	// Section partials: full-fragment responses used by tests and any direct
	// navigation; the client's live refresh does not poll these (it polls the
	// inner fragments below instead).
	for _, name := range []string{"overview", "policies", "torrents", "recovery", "history", "settings", "recent"} {
		mux.Handle("GET /partials/"+name, auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_ = page.ExecuteTemplate(w, name, buildView(snapshot()))
		})))
	}
	// Live inner fragments: these are the only surfaces the client's refresh
	// coordinator polls. Each swaps just its list/counter container, so section
	// chrome keeps node identity. When the snapshot has not advanced since the
	// client's last poll, the handler answers 204 (no body) and htmx leaves the
	// DOM untouched — the change-suppression path that spares every repaint.
	for _, name := range []string{"overview-counters", "torrent-rows", "history-rows", "recovery-rows", "recent-rows", "policy-items"} {
		mux.Handle("GET /partials/"+name, auth(liveFragment(name, page, snapshot)))
	}
	// The header's service indicators swap innerHTML into the #services wrapper,
	// so its partial renders only the indicator list, not the wrapper itself.
	mux.Handle("GET /partials/services", auth(liveFragment("services-inner", page, snapshot)))
	mux.Handle("GET /api/v1/status", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, 200, snapshot())
	})))
	mux.Handle("GET /api/v1/config", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if saver == nil {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		raw, stamp, err := saver.Read()
		if err != nil {
			jsonResponse(w, 500, map[string]string{"error": "cannot read configuration"})
			return
		}
		jsonResponse(w, 200, map[string]string{"raw": string(raw), "stamp": stamp})
	})))
	mux.Handle("POST /api/v1/config", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if saver == nil {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if configuration().WebUsername == "" {
			jsonResponse(w, 403, map[string]string{"error": "configuration editing requires authentication"})
			return
		}
		if !csrfAllowed(r) {
			jsonResponse(w, 403, map[string]string{"error": "cross-site request rejected"})
			return
		}
		raw, stamp, err := readConfigBody(r)
		if err != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		_, status, err := saver.Save([]byte(raw), stamp)
		if errors.Is(err, config.ErrConflict) {
			jsonResponse(w, 409, map[string]string{"status": "rejected", "error": err.Error()})
			return
		}
		if err != nil {
			jsonResponse(w, 422, map[string]string{"status": "rejected", "error": err.Error()})
			return
		}
		jsonResponse(w, 200, map[string]any{"status": "applied", "generation": status.Generation, "last_reload_error": status.LastReloadError})
	})))
	mux.Handle("POST /api/v1/actions", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actions == nil {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if configuration().WebUsername == "" {
			jsonResponse(w, 403, map[string]string{"error": "manual actions require authentication"})
			return
		}
		if !csrfAllowed(r) {
			jsonResponse(w, 403, map[string]string{"error": "cross-site request rejected"})
			return
		}
		var body struct {
			ShortHash string `json:"short_hash"`
			Action    string `json:"action"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		fullHash, ok := resolveShortHash(snapshot().Torrents, body.ShortHash)
		if !ok {
			jsonResponse(w, 404, map[string]string{"error": "torrent not found"})
			return
		}
		decision, err := actions.Force(fullHash, config.Action(body.Action), body.Reason)
		if err != nil {
			jsonResponse(w, 422, map[string]string{"status": "rejected", "error": err.Error()})
			return
		}
		jsonResponse(w, 202, map[string]any{"accepted": true, "decision": decision})
	})))
	mux.Handle("GET /partials/settings-editor", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if saver == nil {
			_ = page.ExecuteTemplate(w, "settings-editor-disabled", nil)
			return
		}
		raw, stamp, err := saver.Read()
		if err != nil {
			_ = page.ExecuteTemplate(w, "settings-editor-disabled", nil)
			return
		}
		_ = page.ExecuteTemplate(w, "settings-editor", settingsEditorView{Raw: string(raw), Stamp: stamp})
	})))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, reason := watchdog.Ready(snapshot(), time.Now(), configuration().ReadinessMaxAge)
		code := 200
		if !ready {
			code = 503
		}
		jsonResponse(w, code, map[string]string{"status": reason})
	})
	metricHandler := promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
	privateMetrics := auth(metricHandler)
	mux.Handle("GET /metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if configuration().MetricsPublic {
			metricHandler.ServeHTTP(w, r)
			return
		}
		privateMetrics.ServeHTTP(w, r)
	}))
	static, _ := fs.Sub(assets, "assets")
	for _, name := range []string{"app.js", "style.css", "htmx.min.js"} {
		mux.Handle("GET /assets/"+name, http.StripPrefix("/assets/", http.FileServer(http.FS(static))))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	})
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// liveFragment serves one live inner fragment for the refresh coordinator. It
// keys change suppression on the snapshot's UpdatedAt stamp: when the client's
// `digest` query parameter equals the current stamp, the snapshot has not
// advanced and the response is 204 with no body, so htmx performs no swap and
// the existing DOM (its open rows, focus, selection) is left intact. Otherwise
// it renders the fragment and stamps it with the current stamp for the next
// round trip. The torrent rows fragment additionally re-opens whatever short
// hashes the client reported as expanded via the `open` query parameter, so an
// open <details> survives a refresh without a collapse flash.
func liveFragment(name string, page *template.Template, snapshot func() watchdog.Snapshot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := snapshot()
		v := buildView(snap)
		if name == "torrent-rows" {
			v = v.markTorrentsOpen(parseOpen(r))
		}
		stamp := stampOf(snap)
		if digest := r.URL.Query().Get("digest"); digest != "" && digest == stamp {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Snapshot-Stamp", stamp)
		_ = page.ExecuteTemplate(w, name, v)
	}
}

// stampOf returns the snapshot generation stamp used for change suppression.
// It is the engine's own publish time, so it advances exactly when the
// snapshot advances and is identical across every fragment the snapshot feeds.
func stampOf(s watchdog.Snapshot) string {
	return s.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

// parseOpen reads the comma-separated list of short hashes the client has
// expanded; the client only ever names rows by their stable id, never touching
// content, so the parsed set is used purely to echo the `open` attribute back.
func parseOpen(r *http.Request) map[string]bool {
	open := map[string]bool{}
	for _, id := range strings.Split(r.URL.Query().Get("open"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			open[id] = true
		}
	}
	return open
}

// settingsEditorView feeds the settings-editor template fragment.
type settingsEditorView struct {
	Raw   string
	Stamp string
}

// readConfigBody extracts the edited YAML and its stamp from either an
// application/x-www-form-urlencoded (HTMX form post) or JSON request body, so
// the editor form and any JSON client stay interchangeable.
func readConfigBody(r *http.Request) (raw, stamp string, err error) {
	if ctype := r.Header.Get("Content-Type"); strings.HasPrefix(ctype, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return "", "", errors.New("invalid form body")
		}
		return r.FormValue("raw"), r.FormValue("stamp"), nil
	}
	var body struct {
		Stamp string `json:"stamp"`
		Raw   string `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", "", err
	}
	return body.Raw, body.Stamp, nil
}

// resolveShortHash maps an operator-supplied short hash back to the full hash
// of a single torrent. The client may only ever name a torrent by its short
// hash; the full hash is resolved server-side from the latest snapshot and never
// accepted from the request. It returns ok==false when no row matches or when
// more than one row shares the same short hash (ambiguous).
func resolveShortHash(torrents []watchdog.Row, short string) (full string, ok bool) {
	short = strings.ToLower(strings.TrimSpace(short))
	for _, row := range torrents {
		if strings.EqualFold(row.ShortHash, short) {
			if full != "" {
				return "", false // ambiguous: two rows share the short hash
			}
			full = row.Hash
		}
	}
	if full == "" {
		return "", false
	}
	return full, true
}

// csrfAllowed enforces that a mutation came from the same origin via an HTMX
// request. Browser-only headers (HX-Request and Sec-Fetch-Site) cannot be set
// by a cross-site form or script, and the Origin must match the request's own
// host when present.
func csrfAllowed(r *http.Request) bool {
	if r.Header.Get("HX-Request") != "true" {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}
