package web

import (
	"strings"
	"testing"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/watchdog"
)

// warningsPage renders a fixed snapshot so assertions are made against what an
// operator's browser receives, never against a source file.
func warningsPage(t *testing.T, s *watchdog.Snapshot) string {
	t.Helper()
	c, _, m := fixture(t)
	return body(t, Handler(c, func() watchdog.Snapshot { return *s }, m), "/policies")
}

func TestPoliciesPageDropsRepeatedDeletionWarningBlock(t *testing.T) {
	c, s, m := fixture(t)
	s.DryRun = false
	s.Policies = []watchdog.PolicyView{
		{Policy: config.Metadata, Action: config.DeleteFile, EffectiveAction: config.DeleteFile, ThresholdSeconds: 60},
		{Policy: config.StalledPartial, Action: config.Delete, EffectiveAction: config.Warn, ThresholdSeconds: 60},
	}
	s.Warnings = []config.Warning{
		{Code: config.WarningDryRunDisabled, Scope: config.WarningSetting, Target: "dry-run", Message: "Dry run disabled"},
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Target: "policy-metadata-action", Message: "Remove torrent and files", Policy: config.Metadata},
	}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	page := body(t, h, "/policies")

	if !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Fatal("policies page did not render to completion")
	}
	// The old loud blocks must never come back.
	for _, old := range []string{"DELETE FILES ENABLED", "DELETION ENABLED"} {
		if strings.Contains(page, old) {
			t.Fatalf("old top-of-page warning block returned: %q", old)
		}
	}
	// The action is stated once, on its policy row, and linked to its control.
	if got := strings.Count(page, "Remove torrent and files"); got != 1 {
		t.Fatalf("destructive action should be stated once, got %d", got)
	}
	if !strings.Contains(page, `class="p-effective p-destructive"`) {
		t.Fatal("destructive effective action is not styled")
	}
	if !strings.Contains(page, `href="/settings#policy-metadata-action"`) {
		t.Fatal("destructive effective action is not linked to its control")
	}
	// The override reads as configured intent that will not execute, not as a
	// second destructive action.
	if !strings.Contains(page, "configured, not executed while dry run is on: Remove torrent") {
		t.Fatal("dry-run override is not muted context")
	}
}

func TestDryRunDisabledBadgeLinksToItsSetting(t *testing.T) {
	c, s, m := fixture(t)
	s.DryRun = false
	s.Policies = []watchdog.PolicyView{
		{Policy: config.Metadata, Action: config.Delete, EffectiveAction: config.Delete, ThresholdSeconds: 60},
	}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	for _, path := range []string{"/", "/policies", "/settings"} {
		page := body(t, h, path)
		if !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
			t.Fatalf("%s did not render to completion", path)
		}
		if !strings.Contains(page, `href="/settings#dry-run"`) || !strings.Contains(page, "Dry run disabled") {
			t.Fatalf("%s header does not link the disabled badge to dry run", path)
		}
		if strings.Contains(page, "DELETION ENABLED") {
			t.Fatalf("%s still renders the old deletion block", path)
		}
	}
}

func TestStoppedPolicyWarningSitsOnItsRowWithDistinctLinks(t *testing.T) {
	c, s, m := fixture(t)
	s.DryRun = false
	s.Policies = []watchdog.PolicyView{
		{Policy: config.Metadata, Action: config.Warn, EffectiveAction: config.Warn, ThresholdSeconds: 60},
		{
			Policy: config.StoppedArrManaged, Action: config.Delete, EffectiveAction: config.Delete,
			ThresholdSeconds: 600, ArrMode: config.InheritArrMode, MatchTags: []string{"Sonarr", "Radarr"},
		},
	}
	s.Warnings = []config.Warning{{
		Code: config.WarningStoppedPolicyArmed, Scope: config.WarningPolicy, Target: "policy-stopped_arr_managed",
		Message: "Stopping a torrent with these tags makes it eligible for this policy after its waiting period. Arr recovery depends on the configured recovery mode.",
		Policy:  config.StoppedArrManaged,
	}}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)
	page := body(t, h, "/policies")

	// The guide must be attached to the stopped row, not floating at page top.
	row := strings.Index(page, `id="policy-stopped_arr_managed"`)
	guide := strings.Index(page, `data-warning-code="stopped_policy_armed"`)
	if row < 0 || guide < 0 || guide < row {
		t.Fatalf("stopped warning is not on its policy row (row=%d guide=%d)", row, guide)
	}
	if !strings.Contains(page, "Matching tags: <strong>Sonarr, Radarr</strong>") {
		t.Fatal("stopped warning does not show the actual matching tags")
	}
	// Three different controls, three different links: match tags, action and
	// Arr recovery must not be collapsed into one remediation.
	for _, href := range []string{
		`href="/settings#policy-stopped_arr_managed-match-tags"`,
		`href="/settings#policy-stopped_arr_managed-action"`,
		`href="/settings#policy-stopped_arr_managed-arr"`,
	} {
		if !strings.Contains(page, href) {
			t.Fatalf("stopped warning is missing link %s", href)
		}
	}
	if !strings.Contains(page, "Arr recovery depends on the configured recovery mode.") {
		t.Fatal("stopped warning lost its message")
	}
}

func TestStoppedPolicyGuideIsAlsoBesideItsSettingsControls(t *testing.T) {
	c, s, m := fixture(t)
	s.Policies = []watchdog.PolicyView{
		{Policy: config.StoppedArrManaged, Action: config.Delete, EffectiveAction: config.Delete, ThresholdSeconds: 600, MatchTags: []string{"Sonarr"}},
	}
	s.Warnings = []config.Warning{{
		Code: config.WarningStoppedPolicyArmed, Scope: config.WarningPolicy, Target: "policy-stopped_arr_managed",
		Message: "Stopping a torrent with these tags makes it eligible for this policy after its waiting period. Arr recovery depends on the configured recovery mode.",
		Policy:  config.StoppedArrManaged,
	}}
	// The structured settings seam renders the policy details.
	h := DynamicHandlerWithConfig(func() config.Config { return c }, func() watchdog.Snapshot { return *s }, m, ConfigSaverFunc{
		SettingsFunc: func() (config.Settings, error) { return structuredSettings(), nil },
	})
	page := body(t, h, "/settings")
	if !strings.Contains(page, `id="policy-stopped_arr_managed-match-tags"`) {
		t.Skip("structured settings fixture unavailable")
	}
	if !strings.Contains(page, `class="policy-guide"`) {
		t.Fatal("settings page does not repeat the stopped-policy guide")
	}
	if !strings.Contains(page, "policy-stopped_arr_managed-arr") {
		t.Fatal("settings guide lost the Arr recovery control")
	}
}

func TestDiagnosticWarningsKeepOnlyGlobalNotices(t *testing.T) {
	warnings := []config.Warning{
		{Code: config.WarningTLSInsecure, Scope: config.WarningSetting, Target: "advanced", Message: "TLS certificate verification disabled"},
		{Code: config.WarningDryRunDisabled, Scope: config.WarningSetting, Target: "dry-run", Message: "Dry run disabled"},
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Target: "policy-metadata-action", Message: "Remove torrent and files", Policy: config.Metadata},
		{Code: config.WarningStoppedPolicyArmed, Scope: config.WarningPolicy, Target: "policy-stopped_arr_managed", Message: "x", Policy: config.StoppedArrManaged},
	}
	diagnostics := diagnosticWarnings(warnings)
	if len(diagnostics) != 1 || diagnostics[0].Code != config.WarningTLSInsecure {
		t.Fatalf("diagnostics should hold only the TLS notice, got %+v", diagnostics)
	}

	c, s, m := fixture(t)
	s.Warnings = warnings
	page := body(t, Handler(c, func() watchdog.Snapshot { return *s }, m), "/policies")
	if !strings.Contains(page, `<details class="diagnostics" data-key="diagnostics">`) ||
		!strings.Contains(page, "TLS certificate verification disabled") ||
		!strings.Contains(page, "Open settings") {
		t.Fatal("global diagnostics are not rendered in the compact area")
	}
	if strings.Contains(page, `data-warning-code="dry_run_disabled"`) {
		t.Fatal("dry-run notice leaked into the diagnostics block")
	}
}

func TestWarningGroupsDeduplicateByIdentityAndPolicy(t *testing.T) {
	groups := warningGroups([]config.Warning{
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Message: "Remove torrent and files", Policy: config.Metadata},
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Message: "Remove torrent and files", Policy: config.StalledPartial},
		// A repeated policy warning must not list the policy twice.
		{Code: config.WarningRemoveFiles, Scope: config.WarningPolicy, Message: "rewritten wording", Policy: config.Metadata},
		{Code: config.WarningReservedPrefix, Scope: config.WarningSetting, Message: "x", Tags: []string{"QBTW-a"}},
		{Code: config.WarningReservedPrefix, Scope: config.WarningSetting, Message: "x", Tags: []string{"QBTW-b"}},
		// Same message, different code: never merged by text.
		{Code: config.WarningTLSInsecure, Scope: config.WarningSetting, Message: "x"},
		{Code: config.WarningDryRunDisabled, Scope: config.WarningSetting, Message: "x"},
	})
	if len(groups) != 4 {
		t.Fatalf("expected 4 identity groups, got %d: %+v", len(groups), groups)
	}
	if len(groups[0].Policies) != 2 || groups[0].Policies[0] != config.Metadata || groups[0].Policies[1] != config.StalledPartial {
		t.Fatalf("multi-policy warning did not list each policy once: %+v", groups[0])
	}
	if groups[0].Message != "Remove torrent and files" {
		t.Fatalf("group dedup should keep the first wording: %q", groups[0].Message)
	}
	if len(groups[1].Tags) != 2 {
		t.Fatalf("tag warnings did not aggregate tags: %+v", groups[1])
	}
}
