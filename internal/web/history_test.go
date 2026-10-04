package web

import (
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

func TestActivityRowsGetDistinctIdentitiesForSharedCommand(t *testing.T) {
	at := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	s := watchdog.Snapshot{History: []store.Event{
		{Time: at, Action: "action_requested", Outcome: "accepted", ShortHash: "aaaaaaaaaaaa", Name: "same", CommandID: 44},
		{Time: at, Action: "action_confirmed", Outcome: "success", ShortHash: "aaaaaaaaaaaa", Name: "same", CommandID: 44},
	}}

	v := buildView(s)
	if len(v.History) != 2 {
		t.Fatalf("expected two history rows, got %d", len(v.History))
	}
	if v.History[0].Identity == v.History[1].Identity {
		t.Fatalf("requested and confirmed share identity %q", v.History[0].Identity)
	}

	c, _, m := fixture(t)
	h := Handler(c, func() watchdog.Snapshot { return s }, m)
	page := body(t, h, "/activity")
	if got := strings.Count(page, "data-history"); got != 2 {
		t.Fatalf("expected two keyed history rows, got %d", got)
	}
	for _, id := range []string{v.History[0].Identity, v.History[1].Identity} {
		if !strings.Contains(page, `id="history-`+id+`"`) {
			t.Fatalf("rendered page missing row id %q", id)
		}
	}
}

func TestLegacyEventIdentityIsDeterministicAndDistinct(t *testing.T) {
	e := store.Event{Time: time.Unix(1700000000, 123456789), Action: "warn", Outcome: "success", ShortHash: "aaaaaaaaaaaa"}
	first := buildView(watchdog.Snapshot{History: []store.Event{e}})
	second := buildView(watchdog.Snapshot{History: []store.Event{e}})
	if first.History[0].Identity != second.History[0].Identity {
		t.Fatalf("legacy identity moved between renders: %q vs %q", first.History[0].Identity, second.History[0].Identity)
	}
	if !strings.HasPrefix(first.History[0].Identity, "legacy-") {
		t.Fatalf("legacy event did not use a derived identity: %q", first.History[0].Identity)
	}

	// Byte-identical retained events still stay distinct.
	dup := buildView(watchdog.Snapshot{History: []store.Event{e, e}})
	if dup.History[0].Identity == dup.History[1].Identity {
		t.Fatalf("duplicate legacy events share identity %q", dup.History[0].Identity)
	}
}

func TestActivityRowWithoutNameRendersHashFallback(t *testing.T) {
	c, s, m := fixture(t)
	s.History = []store.Event{{
		Time: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		// No Name: an older record written before the field existed.
		Action: "action_confirmed", Outcome: "success", ShortHash: "aaaaaaaaaaaa",
	}}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	page := body(t, h, "/activity")
	if !strings.Contains(page, "Name unavailable · aaaaaaaaaaaa") {
		t.Fatal("blank name was not replaced with the hash fallback")
	}
	if strings.Contains(page, `<span class="t-name">—</span>`) {
		t.Fatal("history row still renders a blank placeholder name")
	}
}

func TestWordingIsStandardizedAcrossSurfaces(t *testing.T) {
	if got := ActionLabels(); got[string(config.Warn)] != "Report only" ||
		got[string(config.Delete)] != "Remove torrent" ||
		got[string(config.DeleteFile)] != "Remove torrent and files" {
		t.Fatalf("action wording drifted: %v", got)
	}
	if dryRunLabel(true) != "Dry run enabled" || dryRunLabel(false) != "Dry run disabled" {
		t.Fatalf("dry-run wording drifted: %q / %q", dryRunLabel(true), dryRunLabel(false))
	}

	c, s, m := fixture(t)
	s.DryRun = false
	s.Policies = []watchdog.PolicyView{
		{Policy: config.Metadata, Action: config.DeleteFile, EffectiveAction: config.DeleteFile, ThresholdSeconds: 60},
		{Policy: config.StalledPartial, Action: config.Delete, EffectiveAction: config.Warn, ThresholdSeconds: 60},
	}
	h := Handler(c, func() watchdog.Snapshot { return *s }, m)

	policies := body(t, h, "/policies")
	if !strings.Contains(policies, "Remove torrent and files") || !strings.Contains(policies, "Report only") {
		t.Fatal("policies page does not use the standard action wording")
	}
	settings := body(t, h, "/settings")
	if !strings.Contains(settings, "Dry run disabled") {
		t.Fatal("settings page does not use the standard dry-run wording")
	}
}
