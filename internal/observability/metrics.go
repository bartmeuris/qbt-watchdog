package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	IntegrationHealthy, RecoveryPending                            *prometheus.GaugeVec
	RecoveryOutcomes                                               *prometheus.CounterVec
	ReloadHealthy, ConfigGeneration                                prometheus.Gauge
	PolicyActions                                                  *prometheus.CounterVec
	PolicyTracked, PolicyOverdue, PolicyThreshold, PolicyEffective *prometheus.GaugeVec
	Registry                                                       *prometheus.Registry
	Up, LastSuccess, Total, Tracked, Overdue                       prometheus.Gauge
	Duration                                                       prometheus.Histogram
	PollErrors, StateErrors                                        prometheus.Counter
	Actions                                                        *prometheus.CounterVec
}

func New(build Build) *Metrics {
	r := prometheus.NewRegistry()
	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "qbt_watchdog_" + name, Help: help})
		r.MustRegister(g)
		return g
	}
	counter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: "qbt_watchdog_" + name, Help: help})
		r.MustRegister(c)
		return c
	}
	m := &Metrics{Registry: r, Up: gauge("qbt_up", "Whether the latest poll succeeded."), LastSuccess: gauge("last_successful_poll_timestamp_seconds", "Last successful poll UTC Unix time."), Total: gauge("torrents_total", "Current torrents."), Tracked: gauge("episodes_tracked", "Tracked policy episodes and pending requests."), Overdue: gauge("episodes_overdue", "Overdue policy episodes."), PollErrors: counter("poll_errors_total", "Failed poll cycles."), StateErrors: counter("state_write_errors_total", "Failed state writes.")}
	m.Duration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "qbt_watchdog_poll_duration_seconds", Help: "Poll cycle duration.", Buckets: prometheus.DefBuckets})
	m.IntegrationHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_integration_healthy", Help: "Fresh successful queue snapshot; independent of core readiness."}, []string{"kind"})
	m.RecoveryPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_recovery_pending", Help: "Durable recovery jobs by closed stage."}, []string{"kind", "stage"})
	m.RecoveryOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "qbt_watchdog_recovery_outcomes_total", Help: "Terminal recovery outcomes since startup; search completion is not a grab."}, []string{"kind", "stage", "outcome"})
	r.MustRegister(m.IntegrationHealthy, m.RecoveryPending, m.RecoveryOutcomes)
	m.Actions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "qbt_watchdog_actions_total", Help: "Watchdog actions since process startup."}, []string{"action", "outcome", "dry_run"})
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_build_info", Help: "Build information."}, []string{"version", "revision", "go_version"})
	m.ReloadHealthy = gauge("config_reload_healthy", "Whether the latest configuration reload was accepted.")
	m.ReloadHealthy.Set(1)
	m.ConfigGeneration = gauge("config_generation", "Configurations successfully applied, starting at one for startup.")
	m.ConfigGeneration.Set(1)
	m.PolicyActions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "qbt_watchdog_policy_actions_total", Help: "Policy actions by bounded policy and effective action."}, []string{"policy", "effective_action", "outcome"})
	r.MustRegister(m.PolicyActions)
	m.PolicyTracked = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_policy_tracked", Help: "Current matching policy episodes."}, []string{"policy"})
	m.PolicyOverdue = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_policy_overdue", Help: "Overdue matching policy episodes."}, []string{"policy"})
	m.PolicyThreshold = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_policy_threshold_seconds", Help: "Active policy thresholds."}, []string{"policy"})
	m.PolicyEffective = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "qbt_watchdog_policy_effective_action", Help: "One for each effective action, including dry-run override."}, []string{"policy", "effective_action"})
	r.MustRegister(m.PolicyTracked, m.PolicyOverdue, m.PolicyThreshold, m.PolicyEffective)
	info.WithLabelValues(build.Version, build.Revision, build.GoVersion).Set(1)
	r.MustRegister(m.Duration, m.Actions, info)
	return m
}
