package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

func TestStatusPublishesPerPolicyModelAndReloadHealth(t *testing.T) {
	c, s, m := fixture(t)
	s.ConfigStatus = config.Status{Generation: 7, LastReloadAt: time.Now().UTC()}
	s.Policies = []watchdog.PolicyView{
		{Policy: config.Metadata, Action: config.DeleteFile, EffectiveAction: config.Warn, ThresholdSeconds: 1800},
		{Policy: config.StalledPartial, Action: config.Delete, EffectiveAction: config.Delete, ThresholdSeconds: 60},
	}
	s.Torrents = []watchdog.Row{{
		Name: "example", ShortHash: "aaaaaaaaaaaa", State: "stalledDL",
		Policy: config.StalledPartial, EffectiveAction: config.Delete,
		ThresholdSeconds: 60, Elapsed: 90, Remaining: -30, SeedObserved: true, Decision: "eligible",
	}}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	var decoded map[string]any
	body := request(h, "/api/v1/status", "viewer", "SECRET_PASSWORD").Body.Bytes()
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	status, ok := decoded["config"].(map[string]any)
	if !ok || status["generation"].(float64) != 7 || status["last_reload_error"] != nil {
		t.Fatal("reload health missing from status", decoded["config"])
	}
	policies := decoded["policies"].([]any)
	first := policies[0].(map[string]any)
	// dry_run must be visible as an override, not as a rewritten policy.
	if first["action"] != "delete_file" || first["effective_action"] != "warn" {
		t.Fatal("effective action does not show the dry-run override", first)
	}
	row := decoded["torrents"].([]any)[0].(map[string]any)
	for key, want := range map[string]any{
		"policy": "stalled_partial", "effective_action": "delete",
		"threshold_seconds": 60., "elapsed_seconds": 90., "remaining_seconds": -30.,
	} {
		if row[key] != want {
			t.Fatal("torrent row missing policy detail", key, row[key])
		}
	}
}

func TestFailedReloadDegradesReadinessButNotLiveness(t *testing.T) {
	c, s, m := fixture(t)
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	now := time.Now().UTC()
	s.LastSuccess = &now
	if request(h, "/readyz", "", "").Code != 200 {
		t.Fatal("healthy process not ready")
	}
	s.ConfigStatus = config.Status{Generation: 3, LastReloadError: "poll_interval must be a positive duration"}
	if code := request(h, "/readyz", "", "").Code; code != 503 {
		t.Fatal("failed reload did not degrade readiness", code)
	}
	if request(h, "/healthz", "", "").Code != 200 {
		t.Fatal("failed reload wrongly degraded liveness")
	}
	// The reason is a fixed string, never the operator's file content.
	reason := request(h, "/readyz", "", "").Body.String()
	if strings.Contains(reason, "poll_interval") {
		t.Fatal("readiness echoed reload error detail", reason)
	}
}

func TestPolicyMetricsUseBoundedEnumLabelsOnly(t *testing.T) {
	c, s, m := fixture(t)
	for _, id := range config.PolicyIDs() {
		m.PolicyTracked.WithLabelValues(string(id)).Set(2)
		m.PolicyOverdue.WithLabelValues(string(id)).Set(1)
		m.PolicyThreshold.WithLabelValues(string(id)).Set(30)
		for _, action := range config.Actions() {
			m.PolicyEffective.WithLabelValues(string(id), string(action)).Set(0)
			m.PolicyActions.WithLabelValues(string(id), string(action), "success").Inc()
		}
	}
	m.ReloadHealthy.Set(0)
	m.ConfigGeneration.Set(4)
	body := request(Handler(c, func() watchdog.Snapshot { return *s }, m), "/metrics", "", "").Body.String()
	for _, want := range []string{
		`qbt_watchdog_policy_tracked{policy="stalled_no_seeders"} 2`,
		`qbt_watchdog_policy_threshold_seconds{policy="stalled_partial"} 30`,
		`qbt_watchdog_policy_effective_action{effective_action="warn",policy="metadata"} 0`,
		`qbt_watchdog_policy_actions_total{effective_action="delete_file",outcome="success",policy="stalled_seeders_seen"} 1`,
		"qbt_watchdog_config_reload_healthy 0",
		"qbt_watchdog_config_generation 4",
	} {
		if !strings.Contains(body, want) {
			t.Fatal("missing metric", want)
		}
	}
	// Label cardinality stays bounded: only the policy and action enums appear.
	for _, label := range allLabelValues(body) {
		if !bounded(label) {
			t.Fatal("unbounded label value in metrics", label)
		}
	}
}

// allLabelValues extracts every label value from an exposition body.
func allLabelValues(body string) []string {
	values := []string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "qbt_watchdog_") {
			continue
		}
		open, close := strings.Index(line, "{"), strings.Index(line, "}")
		if open < 0 || close < open {
			continue
		}
		for _, pair := range strings.Split(line[open+1:close], ",") {
			name, value, found := strings.Cut(pair, "=")
			// Histogram bucket boundaries are structural, not a dimension.
			if found && name != "le" {
				values = append(values, strings.Trim(value, `"`))
			}
		}
	}
	return values
}

func bounded(value string) bool {
	for _, id := range config.PolicyIDs() {
		if value == string(id) {
			return true
		}
	}
	for _, action := range config.Actions() {
		if value == string(action) {
			return true
		}
	}
	switch value {
	// Build info and the pre-existing action counter are fixed-cardinality.
	case "test", "success", "accepted", "skipped", "failed", "true", "false",
		"warn", "action_requested", "action_confirmed", "action_skipped", "action_failed":
		return true
	}
	return strings.HasPrefix(value, "go1.")
}
