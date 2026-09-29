package config

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// Status is the reload-health surface. Stage 2 publishes it into the status
// payload, the UI and the readiness endpoint. It deliberately carries no
// configuration values, so it can never leak a secret.
type Status struct {
	// Generation counts successfully applied configurations. It starts at 1
	// for the configuration the process started with.
	Generation uint64 `json:"generation"`
	// LastReloadError is empty while the file on disk is the configuration
	// actually in effect. A non-empty value means the running configuration
	// is the last known good one and the file has a problem.
	LastReloadError string `json:"last_reload_error,omitempty"`
	// LastReloadAt is when the last reload attempt finished, successful or
	// not; zero until the first attempt.
	LastReloadAt time.Time `json:"last_reload_at"`
}

func (s Status) Healthy() bool { return s.LastReloadError == "" }

// Manager owns the single mutable configuration cell in the process.
//
// Readers call Current, which is a lock-free load of an immutable snapshot, so
// poll and request paths never block on a reload and never touch Viper.
// Writers go through Reload, which is serialized and transactional: a
// candidate that is invalid, that touches a restart-only setting, or whose
// commit fails leaves the previous snapshot untouched and records why.
type Manager struct {
	path    string
	apply   sync.Mutex
	current atomic.Pointer[Config]
	status  atomic.Pointer[Status]
}

// NewManager publishes the startup configuration as generation 1.
func NewManager(path string, initial Config) *Manager {
	m := &Manager{path: path}
	snapshot := initial.Clone()
	m.current.Store(&snapshot)
	m.status.Store(&Status{Generation: 1})
	return m
}

// Current returns the configuration in effect, safe to mutate by the caller.
func (m *Manager) Current() Config { return m.current.Load().Clone() }

// Status returns the reload health of the process.
func (m *Manager) Status() Status { return *m.status.Load() }

// Path is the configuration file being watched.
func (m *Manager) Path() string { return m.path }

// Reload validates a candidate against the running configuration and, if
// commit accepts it, publishes it as the new snapshot. commit receives its own
// clone and must either take effect completely or fail without side effects.
func (m *Manager) Reload(next Config, commit func(Config) error) error {
	m.apply.Lock()
	defer m.apply.Unlock()
	current := m.current.Load()
	if err := RestartRequiredError(*current, next); err != nil {
		m.Record(err)
		return err
	}
	if reflect.DeepEqual(next, *current) {
		m.Record(nil)
		return nil
	}
	if commit != nil {
		if err := commit(next.Clone()); err != nil {
			m.Record(err)
			return err
		}
	}
	applied := next.Clone()
	m.current.Store(&applied)
	m.status.Store(&Status{Generation: m.status.Load().Generation + 1, LastReloadAt: time.Now().UTC()})
	return nil
}

// Record publishes the outcome of a reload attempt without changing the
// running configuration. A nil error clears a previously reported failure.
func (m *Manager) Record(err error) {
	status := *m.status.Load()
	status.LastReloadAt = time.Now().UTC()
	status.LastReloadError = ""
	if err != nil {
		status.LastReloadError = err.Error()
	}
	m.status.Store(&status)
}

// Run watches the configuration file until ctx is cancelled, reloading on
// every change. observe, if set, is notified of each attempt's outcome so the
// caller can log it; the manager has no logger of its own because a rejected
// reload must be reported exactly once, by the owner of the log.
func (m *Manager) Run(ctx context.Context, commit func(Config) error, observe func(error)) error {
	return Watch(ctx, m.path, func(next Config) error {
		next.Once = m.current.Load().Once
		return m.Reload(next, commit)
	}, func(err error) {
		m.Record(err)
		if observe != nil {
			observe(err)
		}
	})
}
