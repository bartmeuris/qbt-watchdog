package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"golang.org/x/term"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/observability"
	"qbt-watchdog/internal/qbt"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
	"qbt-watchdog/internal/web"
)

var version = "dev"
var revision = "unknown"
var buildDate = "unknown"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func run(args []string, out, stderr io.Writer) int {
	build := observability.NewBuild(version, revision, buildDate)
	if len(args) > 0 && args[0] == "version" {
		_ = json.NewEncoder(out).Encode(build)
		return 0
	}
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(args[1:], stderr)
	}
	c, err := config.Parse(args, os.LookupEnv, out)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	logging := newLogSwitch(stderr)
	logging.Apply(c.LogLevel, c.LogFormat, c.LogColor)
	log := slog.New(logging)
	log.Info("qbt-watchdog starting", "event", "startup", "version", version, "dry_run", c.DryRun, "poll_interval", c.PollInterval, "max_observation_gap", c.MaxObservationGap, "max_actions_per_poll", c.MaxDeletions, "max_attempts_per_episode", store.MaxAttempts)
	announce(log, c)
	client, err := qbt.New(c, log)
	if err != nil {
		log.Error("client configuration failed", "error", err)
		return 2
	}
	metrics := observability.New(build)
	disk := store.File{Path: c.StateFile, HistoryLimit: c.HistoryLimit}
	service := watchdog.New(c, client, disk, watchdog.RealClock{}, log, metrics, build)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.Once {
		err := service.Poll(ctx)
		snapshot := service.Snapshot()
		_ = json.NewEncoder(out).Encode(struct {
			Summary          watchdog.Summary `json:"summary"`
			DryRun           bool             `json:"dry_run"`
			Error            string           `json:"error"`
			PersistenceError string           `json:"persistence_error"`
		}{snapshot.Summary, c.DryRun, snapshot.PollError, snapshot.PersistenceError})
		if err != nil || snapshot.PersistenceError != "" {
			return 1
		}
		return 0
	}
	server := web.New(c, service.Snapshot, metrics)
	// The service's manager owns the running configuration and the reload
	// health that the status, UI and readiness surfaces report, so the watcher
	// drives that same manager rather than a second one. Committing means the
	// service has adopted the candidate; only then do process-wide settings
	// follow. Apply is the commit half: the manager already holds its lock.
	manager := service.ConfigManager()
	commit := func(next config.Config) error {
		if err := service.Apply(next, func(candidate config.Config) (watchdog.Client, error) { return qbt.New(candidate, log) }); err != nil {
			return err
		}
		logging.Apply(next.LogLevel, next.LogFormat, next.LogColor)
		announce(log, next)
		return nil
	}
	editor := config.NewEditor(manager.Path())
	saver := web.ConfigSaverFunc{
		ReadFunc: editor.Read,
		SaveFunc: func(raw []byte, stamp string) (string, config.Status, error) {
			newStamp, err := editor.SaveRaw(raw, stamp)
			if err != nil {
				return "", config.Status{}, err
			}
			next, err := config.Load(manager.Path())
			if err != nil {
				return newStamp, manager.Status(), err
			}
			if err := manager.Reload(next, commit); err != nil {
				return newStamp, manager.Status(), err
			}
			return newStamp, manager.Status(), nil
		},
	}
	server.Handler = web.DynamicHandlerWithActions(service.Config, service.Snapshot, metrics, saver, service)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if err := manager.Run(ctx, commit, service.ReportReload); err != nil {
			service.ReportReload(err)
		}
	}()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	exit := 0
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server failed", "event", "http_error")
			exit = 1
		}
		stop()
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		log.Warn("HTTP shutdown deadline reached", "event", "shutdown_timeout")
	}
	<-done
	<-watchDone
	log.Info("shutdown complete", "event", "shutdown")
	return exit
}

// announce records the active policies and repeats every loud safety warning.
// It runs after each accepted reload so a dangerous change is never quiet.
func announce(log *slog.Logger, c config.Config) {
	for _, id := range config.PolicyIDs() {
		log.Info("policy configured", "policy", id, "action", c.Policies[id].Action, "effective_action", c.EffectiveAction(id), "threshold", c.Policies[id].Threshold)
	}
	for _, warning := range c.Warnings() {
		attributes := []any{"event", "unsafe_configuration"}
		if warning.Policy != "" {
			attributes = append(attributes, "policy", warning.Policy)
		}
		log.Warn(warning.Message, attributes...)
	}
}

// logSwitch makes log_level and log_format reloadable by swapping the handler
// behind the root logger. Loggers derived with WithAttrs or WithGroup keep the
// handler they were built from; the process only logs through the root, so
// that limitation is never observable.
type logSwitch struct {
	out     io.Writer
	current atomic.Pointer[slog.Handler]
}

func newLogSwitch(out io.Writer) *logSwitch { return &logSwitch{out: out} }

func (l *logSwitch) Apply(level, format, color string) {
	parsed := slog.LevelInfo
	_ = parsed.UnmarshalText([]byte(level))
	options := &slog.HandlerOptions{Level: parsed}
	var handler slog.Handler = slog.NewJSONHandler(l.out, options)
	switch format {
	case "text":
		handler = slog.NewTextHandler(l.out, options)
	case "console":
		handler = tint.NewTextHandler(l.out, &tint.Options{
			Level:      parsed,
			TimeFormat: time.Kitchen,
			NoColor:    !resolveColor(l.out, color),
		})
	}
	l.current.Store(&handler)
}

// resolveColor decides whether the console handler may emit ANSI color. The
// decision honors the operator's explicit `always`/`never`, otherwise falling
// back to terminal detection and the NO_COLOR convention.
func resolveColor(out io.Writer, mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func (l *logSwitch) handler() slog.Handler { return *l.current.Load() }
func (l *logSwitch) Enabled(ctx context.Context, level slog.Level) bool {
	return l.handler().Enabled(ctx, level)
}
func (l *logSwitch) Handle(ctx context.Context, record slog.Record) error {
	return l.handler().Handle(ctx, record)
}
func (l *logSwitch) WithAttrs(attrs []slog.Attr) slog.Handler { return l.handler().WithAttrs(attrs) }
func (l *logSwitch) WithGroup(name string) slog.Handler       { return l.handler().WithGroup(name) }

func healthcheck(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	target := fs.String("url", "http://127.0.0.1:8080/healthz", "liveness endpoint")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "invalid healthcheck flags")
		return 2
	}
	u, err := url.Parse(*target)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		fmt.Fprintln(stderr, "invalid healthcheck URL")
		return 2
	}
	client := http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(u.String())
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck connection failed")
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(stderr, "healthcheck unhealthy")
		return 1
	}
	return 0
}
