package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/watchdog"
)

//go:embed assets/*
var assets embed.FS

func New(c config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics) *http.Server {
	return &http.Server{Addr: c.Listen, Handler: Handler(c, snapshot, metrics), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
}

func Handler(c config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics) http.Handler {
	return DynamicHandler(func() config.Config { return c }, snapshot, metrics)
}

func DynamicHandler(configuration func() config.Config, snapshot func() watchdog.Snapshot, metrics *observability.Metrics) http.Handler {
	page := template.Must(template.ParseFS(assets, "assets/index.html"))
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
	mux.Handle("GET /{$}", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = page.Execute(w, snapshot())
	})))
	mux.Handle("GET /api/v1/status", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, 200, snapshot())
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
	for _, name := range []string{"app.js", "style.css"} {
		mux.Handle("GET /assets/"+name, http.StripPrefix("/assets/", http.FileServer(http.FS(static))))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		mux.ServeHTTP(w, r)
	})
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
