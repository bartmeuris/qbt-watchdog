package web

import (
	"html/template"
	"sort"
	"time"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
	"qbt-watchdog/internal/watchdog"
)

// view is everything the templates render. It wraps the raw snapshot with the
// per-entity timelines derived from the correlation fields, so a template never
// recomputes a story the engine already told.
type view struct {
	Snapshot watchdog.Snapshot
	Torrents []torrentView
	Recovery []recoveryView
	History  []historyView
	// Page names the section a full page shell should render ("overview",
	// "policies", "activity", "settings"). It is empty for section partials,
	// which render a single fragment and never consult the page name.
	Page string
}

type torrentView struct {
	Row      watchdog.Row
	Timeline Timeline
	// Open reports whether this row is currently expanded on the client.
	Open bool
}

type recoveryView struct {
	Job      watchdog.RecoveryStatus
	Timeline Timeline
}

type historyView struct {
	Event    store.Event
	Timeline Timeline
}

func buildView(s watchdog.Snapshot) view {
	v := view{Snapshot: s}

	v.Torrents = make([]torrentView, 0, len(s.Torrents))
	for _, row := range s.Torrents {
		v.Torrents = append(v.Torrents, torrentView{
			Row:      row,
			Timeline: TorrentTimeline(row, s.RecoveryJobs, s.History),
		})
	}
	sortTorrentViews(v.Torrents)

	v.Recovery = make([]recoveryView, 0, len(s.RecoveryJobs))
	for _, job := range s.RecoveryJobs {
		v.Recovery = append(v.Recovery, recoveryView{
			Job:      job,
			Timeline: RecoveryTimeline(job),
		})
	}

	// History renders newest first, same as the old app.js re-render.
	v.History = make([]historyView, 0, len(s.History))
	for i := len(s.History) - 1; i >= 0; i-- {
		e := s.History[i]
		v.History = append(v.History, historyView{
			Event:    e,
			Timeline: TorrentTimeline(eventRow(e), s.RecoveryJobs, s.History),
		})
	}

	return v
}

// sortTorrentViews imposes the presentation order for the torrent list:
// overdue rows first (they are the ones an operator watches), then a stable
// key (short hash, then name) so a routine refresh never moves rows around as
// their countdown changes. This is purely a display concern; it does not touch
// the engine's candidate processing order in service.go, which sorts by
// FirstSeen before capping deletions.
func sortTorrentViews(rows []torrentView) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].Row, rows[j].Row
		ao := overdue(a)
		bo := overdue(b)
		if ao != bo {
			return ao
		}
		if a.ShortHash != b.ShortHash {
			return a.ShortHash < b.ShortHash
		}
		return a.Name < b.Name
	})
}

// overdue reports whether a row's threshold has already been met. It mirrors
// the engine's own overdue predicate (FirstSeen proven and Remaining <= 0)
// without importing that logic, so the display order can never contradict the
// summary counters.
func overdue(r watchdog.Row) bool {
	return r.FirstSeen != nil && r.Remaining <= 0
}

// buildPageView wraps buildView with the active page name so a full page shell
// renders exactly one section. It shares buildView, so a page and its matching
// partial always render identical rows from the same template.
func buildPageView(s watchdog.Snapshot, page string) view {
	v := buildView(s)
	v.Page = page
	return v
}

// markTorrentsOpen flags the view's torrent rows whose short hash appears in
// open. It is how a poll's partial re-renders already-expanded rows as open,
// so the client never collapses and re-expands them.
func (v view) markTorrentsOpen(open map[string]bool) view {
	for i := range v.Torrents {
		v.Torrents[i].Open = open[v.Torrents[i].Row.ShortHash]
	}
	return v
}

// eventRow lifts the correlation fields of an audit event into a minimal Row so
// the same TorrentTimeline path can describe a history row without inventing a
// second timeline builder.
func eventRow(e store.Event) watchdog.Row {
	return watchdog.Row{ShortHash: e.ShortHash, Policy: e.Policy}
}

// timeHTML renders a time as a localizable <time datetime> element, falling back
// to an em dash when the instant is zero. The datetime attribute is generated
// from a trusted time value, never operator input, so marking it safe is sound.
// The title attribute carries the full instant so a compact display still offers
// the exact timestamp on demand.
func timeHTML(t time.Time) template.HTML {
	return timeHTMLWithStyle(t, false)
}

// timeHTMLShort renders the same element flagged for a compact (short) display
// style, used by the Activity page where a full date+time would crowd the row.
func timeHTMLShort(t time.Time) template.HTML {
	return timeHTMLWithStyle(t, true)
}

func timeHTMLWithStyle(t time.Time, short bool) template.HTML {
	if t.IsZero() {
		return "—"
	}
	dt := t.UTC().Format(time.RFC3339)
	style := ""
	if short {
		style = ` data-short`
	}
	return template.HTML(`<time datetime="` + dt + `" title="` + dt + `"` + style + `>` + dt + `</time>`)
}

func timeOrDash(t *time.Time) template.HTML {
	if t == nil {
		return "—"
	}
	return timeHTML(*t)
}

// refreshEvery returns the poll interval in whole seconds, used by the client
// refresh coordinator via the document's data-refresh-seconds attribute.
func refreshEvery(seconds float64) int {
	if seconds < 1 {
		return 1
	}
	return int(seconds)
}

func newViewFuncs() template.FuncMap {
	return template.FuncMap{
		"policyLabel":       policyLabel,
		"policyOrUnclass":   policyLabelOrUnclassified,
		"policyDescription": policyDescription,
		"actionLabel":       actionLabel,
		"decisionLabel":     decisionLabel,
		"gateLabel":         gateLabel,
		"stageLabel":        stageLabel,
		"modeLabel":         modeLabel,
		"kindLabel":         kindLabel,
		"actionEventLabel":  actionEventLabel,
		"outcomeLabel":      outcomeLabel,
		"statusLabel":       statusLabel,
		"timeOrDash":        timeOrDash,
		"timeHTML":          timeHTML,
		"timeHTMLShort":     timeHTMLShort,
		"refreshEvery":      refreshEvery,
		"dash":              dash,
		"duration":          duration,
		"bytes":             bytes,
		"pct":               pct,
		"join":              join,
		"hasDestructive":    hasDestructive,
		"nextActionLabel":   nextActionLabel,
		"actionVerb":        actionVerb,
		"decisionSentence":  decisionSentence,
		"gateStrip":         gateStrip,
		"serviceIndicators": serviceIndicators,
		"torrentStates":     torrentStates,
		"progressSymbol":    progressSymbol,
		"policyRows":        policyRows,
		"warningGroups":     warningGroups,
		"policyNames":       policyNames,
		"historyService":    historyService,
		"historyRowClass":   historyRowClass,
		"outcomeClass":      outcomeClass,
		"historyPolicies":   historyPolicies,
		"historyOutcomes":   historyOutcomes,
		"historyServices":   historyServices,
	}
}

func hasDestructive(s watchdog.Snapshot) bool {
	for _, p := range s.Policies {
		if p.EffectiveAction != "" && p.EffectiveAction != config.Warn {
			return true
		}
	}
	return false
}
