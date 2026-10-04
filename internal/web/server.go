package web

import (
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
	renderPage := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_ = page.ExecuteTemplate(w, "base", buildPageView(snapshot(), name))
		})
	}
	mux.Handle("GET /{$}", renderPage("overview"))
	mux.Handle("GET /policies", renderPage("policies"))
	mux.Handle("GET /activity", renderPage("activity"))
	// The Settings page renders the typed, source-preserving form server-side
	// from the structured seam. It is fetched once per navigation and never
	// polled, so a live refresh can never replace an operator's draft.
	mux.Handle("GET /settings", settingsPage("base", page, snapshot, saver))
	// Section partials: full-fragment responses used by tests and any direct
	// navigation; the client's live refresh does not poll these (it polls the
	// inner fragments below instead).
	for _, name := range []string{"overview", "policies", "torrents", "recovery", "history", "recent"} {
		mux.Handle("GET /partials/"+name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_ = page.ExecuteTemplate(w, name, buildView(snapshot()))
		}))
	}
	mux.Handle("GET /partials/settings", settingsPage("settings", page, snapshot, saver))
	// Live inner fragments: these are the only surfaces the client's refresh
	// coordinator polls. Each renders just its list/counter container's inner
	// content; the client reconciles it by stable data-key identity, so section
	// chrome and existing rows keep node identity. When the snapshot has not
	// advanced since the client's last poll, the handler answers 204 (no body)
	// and the client does no DOM work at all.
	for _, name := range []string{"overview-counters", "torrent-rows", "history-rows", "recovery-rows", "recent-rows", "policy-items"} {
		mux.Handle("GET /partials/"+name, liveFragment(name, page, snapshot))
	}
	// The header's service indicators are reconciled inside the #services
	// wrapper, so its partial renders only the indicator list, not the wrapper.
	mux.Handle("GET /partials/services", liveFragment("services-inner", page, snapshot))
	mux.Handle("GET /api/v1/status", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, 200, snapshot())
	}))
	mux.Handle("GET /api/v1/config", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	mux.Handle("POST /api/v1/config", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if saver == nil {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
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
		result, err := saver.Save([]byte(raw), stamp)
		if errors.Is(err, config.ErrConflict) {
			jsonResponse(w, 409, map[string]any{"status": "conflict", "error": err.Error()})
			return
		}
		if err != nil {
			jsonResponse(w, 422, map[string]any{"status": "rejected", "error": err.Error(), "errors": result.Errors})
			return
		}
		jsonResponse(w, 200, result)
	}))
	mux.Handle("GET /api/v1/settings", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings, ok := saver.(SettingsSaver)
		if !ok {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		model, err := settings.Settings()
		if errors.Is(err, errSettingsUnavailable) {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if err != nil {
			jsonResponse(w, 500, map[string]string{"error": "cannot read configuration"})
			return
		}
		jsonResponse(w, 200, model)
	}))
	mux.Handle("GET /api/v1/settings/environment", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings, ok := saver.(SettingsSaver)
		if !ok {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		environment, err := settings.Environment()
		if errors.Is(err, errSettingsUnavailable) {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if err != nil {
			jsonResponse(w, 500, map[string]string{"error": "cannot read environment"})
			return
		}
		jsonResponse(w, 200, map[string]any{"environment": environment})
	}))
	mux.Handle("POST /api/v1/settings", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settings, ok := saver.(SettingsSaver)
		if !ok {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if !csrfAllowed(r) {
			jsonResponse(w, 403, map[string]string{"error": "cross-site request rejected"})
			return
		}
		stamp, patch, fieldErrors, err := readSettingsBody(r, settings)
		if err != nil {
			jsonResponse(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		if len(fieldErrors) > 0 {
			jsonResponse(w, 422, map[string]any{"status": "rejected", "error": "validation failed", "errors": fieldErrors})
			return
		}
		result, err := settings.Patch(stamp, patch)
		if errors.Is(err, errSettingsUnavailable) {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
			return
		}
		if errors.Is(err, config.ErrConflict) {
			jsonResponse(w, 409, map[string]any{"status": "conflict", "error": err.Error()})
			return
		}
		if err != nil {
			jsonResponse(w, 422, map[string]any{"status": "rejected", "error": err.Error(), "errors": result.Errors})
			return
		}
		jsonResponse(w, 200, result)
	}))
	mux.Handle("POST /api/v1/actions", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actions == nil {
			jsonResponse(w, 404, map[string]string{"error": "not found"})
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
	}))
	mux.Handle("GET /partials/settings-editor", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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
	// Built-in web authentication was removed, so /metrics is intentionally
	// unauthenticated: access control is the reverse proxy's responsibility.
	// Protect /metrics at the proxy as well if the exposition must stay private.
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
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
// advanced and the response is 204 with no body, so the client performs no DOM
// work. Otherwise it renders the fragment and stamps it with the current stamp
// for the next round trip. The client reconciles a rendered fragment into the
// existing DOM by stable data-key identity (see app.js), so only changed nodes
// are touched.
func liveFragment(name string, page *template.Template, snapshot func() watchdog.Snapshot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := snapshot()
		v := buildView(snap)
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

// settingsEditorView feeds the settings-editor template fragment.
type settingsEditorView struct {
	Raw   string
	Stamp string
}

// settingsPage renders the structured Settings page (or its section partial)
// from the typed read model. When the structured seam is unavailable it renders
// the same shell with an explanatory note, so the raw editor stays reachable.
func settingsPage(name string, page *template.Template, snapshot func() watchdog.Snapshot, saver ConfigSaver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = page.ExecuteTemplate(w, name, settingsPageView(snapshot, saver))
	})
}

// settingsPageView builds the view for the Settings page. A missing or failing
// structured seam is reported as a note rather than a 500, so the page still
// renders and the raw editor remains available.
func settingsPageView(snapshot func() watchdog.Snapshot, saver ConfigSaver) view {
	settings, ok := saver.(SettingsSaver)
	if !ok {
		return buildSettingsPageView(snapshot(), nil, "Structured settings are not available; use the raw editor below.")
	}
	model, err := settings.Settings()
	if errors.Is(err, errSettingsUnavailable) {
		return buildSettingsPageView(snapshot(), nil, "Structured settings are not available; use the raw editor below.")
	}
	if err != nil {
		return buildSettingsPageView(snapshot(), nil, "Cannot read the configuration; use the raw editor below.")
	}
	return buildSettingsPageView(snapshot(), &model, "")
}

// readSettingsBody extracts a stamp and a patch from either a JSON body or the
// structured form. A form body is diffed against the current read model so only
// changed leaves are sent; a JSON body is passed through unchanged.
func readSettingsBody(r *http.Request, settings SettingsSaver) (string, config.Patch, []config.FieldError, error) {
	if ctype := r.Header.Get("Content-Type"); strings.HasPrefix(ctype, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return "", config.Patch{}, nil, err
		}
		current, err := settings.Settings()
		if err != nil {
			return "", config.Patch{}, nil, err
		}
		patch, fieldErrors := parseSettingsForm(r, current)
		return r.FormValue("stamp"), patch, fieldErrors, nil
	}
	var body struct {
		Stamp string       `json:"stamp"`
		Patch config.Patch `json:"patch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", config.Patch{}, nil, err
	}
	return body.Stamp, body.Patch, nil, nil
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
