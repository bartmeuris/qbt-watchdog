'use strict'
const byId = id => document.getElementById(id)
const text = (id, value) => {
  byId(id).textContent = value
}
const localTime = value => (value ? new Date(value).toLocaleString() : '—')
const duration = value => {
  const n = Math.round(Math.abs(value))
  return n < 60
    ? `${n}s`
    : n < 3600
    ? `${Math.floor(n / 60)}m ${n % 60}s`
    : `${Math.floor(n / 3600)}h ${Math.floor((n % 3600) / 60)}m`
}
const bytes = value => {
  if (value < 1024) return `${value} B`
  const unit = Math.min(Math.floor(Math.log(value) / Math.log(1024)), 4)
  return `${(value / 1024 ** unit).toFixed(1)} ${
    ['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]
  }`
}
// Friendly wording lives only here. Machine values keep their exact spelling
// in /api/v1/status, in the metrics and in the persisted state, so nothing
// downstream depends on how this page reads.
const POLICY_LABELS = {
  metadata: 'Metadata stall',
  stalled_no_seeders: 'Stalled · no seeders ever seen',
  stalled_seeders_seen: 'Stalled · seeders seen before',
  stalled_partial: 'Stalled · partly downloaded',
  completed_no_data: 'Completed · no payload data',
  stopped_arr_managed: 'Stopped · Arr managed'
}
const ACTION_LABELS = {
  warn: 'Warn only',
  delete: 'Delete torrent, keep files',
  delete_file: 'Delete torrent and files'
}
// Each decision the engine can publish, as a headline plus the reason an
// action is or is not possible right now. "blocked" greys the row's status.
const DECISIONS = {
  eligible: [
    'Eligible now',
    'The action cap, exclusions and a final re-read still apply'
  ],
  tracking: [
    'Counting down',
    'Waiting for continuous observation to reach the threshold'
  ],
  'not applicable': [
    'Not applicable',
    'Outside every policy, so no clock and no action',
    'blocked'
  ],
  protected: [
    'No action · protected',
    'Excluded by category or tag',
    'blocked'
  ],
  'nonzero progress': [
    'No action · has progress',
    'The metadata policy requires zero progress',
    'blocked'
  ],
  'nonzero downloaded': [
    'No action · has payload',
    'This policy requires zero downloaded bytes',
    'blocked'
  ],
  'payload present': [
    'No action · payload present',
    'The completed-no-data policy requires zero size and zero downloaded bytes',
    'blocked'
  ],
  'delete requested': [
    'No action · delete pending',
    'A delete was requested and is awaiting confirmation',
    'blocked'
  ],
  'actions disabled': [
    'No action · actions disabled',
    'The per-poll action cap is zero',
    'blocked'
  ],
  warned: [
    'No action · already warned',
    'This episode has already produced its one warning',
    'blocked'
  ],
  'retry limit reached': [
    'No action · retry limit reached',
    'The attempt budget for this episode is exhausted',
    'blocked'
  ]
}
// Lookups are total: an unknown value falls back to something truthful rather
// than reaching an inherited object property.
const look = (table, value, fallback) =>
  Object.hasOwn(table, value) ? table[value] : fallback
const policyLabel = id => look(POLICY_LABELS, id, 'Not applicable')
const actionLabel = action => look(ACTION_LABELS, action, '—')
const decisionOf = decision =>
  look(DECISIONS, decision, [decision || 'Unknown', '', 'blocked'])
// What the engine would actually do, and whether that differs from what the
// operator configured. A difference is always a dry-run downgrade.
const effectiveNote = t => {
  if (!t.policy) return 'No policy, so no action'
  const effective = `Effective now: ${actionLabel(t.effective_action)}`
  if (t.configured_action === t.effective_action) return effective
  return `${effective} (dry-run override)`
}
// Time left before the policy threshold is met. Only a torrent with a proven,
// uninterrupted episode has one at all; the rest keep a dash.
const eligibility = t => {
  if (!t.first_seen_policy) return ['—', 'No episode clock']
  if (t.remaining_seconds > 0)
    return [duration(t.remaining_seconds), 'Until the threshold is met']
  return [`${duration(t.remaining_seconds)} overdue`, 'Threshold already met']
}
function cell (row, value, detail, className) {
  const td = document.createElement('td')
  td.textContent = value
  if (className) td.className = className
  if (detail !== undefined) {
    const small = document.createElement('small')
    small.textContent = detail
    td.append(small)
  }
  row.append(td)
}
function empty (body, columns, message) {
  if (body.childElementCount) return
  const tr = document.createElement('tr'),
    td = document.createElement('td')
  td.colSpan = columns
  td.textContent = message
  tr.append(td)
  body.append(tr)
}
function render (s) {
  text(
    'build',
    `${s.build.version} · ${s.build.revision} · ${s.build.go_version} · ${s.build.build_date}`
  )
  const destructive = s.policies.some(p => p.effective_action !== 'warn')
  text(
    'mode',
    s.dry_run
      ? 'DRY RUN — every policy downgraded to warn'
      : destructive
      ? 'ACTIVE — destructive policies enabled'
      : 'WARN ONLY — no deletions'
  )
  byId('mode').classList.toggle('active', destructive)
  text(
    'reload',
    s.config.last_reload_error ||
      `Healthy · generation ${s.config.generation}${
        s.config.last_reload_at && !s.config.last_reload_at.startsWith('0001-')
          ? ` · checked ${localTime(s.config.last_reload_at)}`
          : ''
      }`
  )
  text(
    'policies',
    s.policies
      .map(
        p =>
          `${policyLabel(p.policy)} — eligible after ${duration(
            p.threshold_seconds
          )}, configured ${actionLabel(p.action)}, effective ${actionLabel(
            p.effective_action
          )}${(p.match_tags || []).length ? `, tags ${(p.match_tags || []).join(', ')}` : ''}`
      )
      .join(' · ')
  )
  text(
    'connection',
    `${s.qbt_up ? 'Reachable' : 'Degraded / unreachable'} · qBittorrent ${
      s.qbt_version || 'unknown'
    } · Web API ${s.webapi_version || 'unknown'}`
  )
  text('last', localTime(s.last_successful_poll))
  text('next', localTime(s.next_poll))
  text('persistence', s.persistence_error || 'Healthy')
  text('persistence', `${s.persistence_error || 'Healthy'} · tag sync ${s.tag_sync?.enabled ? 'enabled' : 'disabled'} (${s.tag_sync?.prefix || '—'}${s.tag_sync?.dry_run_suppressed ? ', dry-run suppressed' : ''})`)
  text('error', s.poll_error || 'None')
  text('load-warning', s.state_load_warning || 'None')
  text('integrations', (s.integrations || []).map(a => `${a.kind}: ${!a.enabled ? 'disabled' : `${a.queue_fresh ? 'fresh queue' : 'queue unavailable / stale'} · ${a.code || 'waiting'}`}`).join(' · '))
  text('recovery-count', `${(s.recovery_jobs || []).length} pending${s.dry_run ? ' · mutations paused' : ''}`)
  const recovery = document.createElement('tbody')
  for (const job of s.recovery_jobs || []) {
    const row = document.createElement('tr')
    cell(row, job.kind, job.mode)
    cell(row, job.stage, job.code || '—')
    cell(row, localTime(job.captured_at))
    cell(row, localTime(job.expires_at))
    cell(row, job.attempts, job.next_at && !job.next_at.startsWith('0001-') ? localTime(job.next_at) : 'Next worker cycle')
    recovery.append(row)
  }
  empty(recovery, 5, 'No pending recovery jobs.')
  byId('recovery-jobs').replaceChildren(...recovery.childNodes)
  const cards = document.createDocumentFragment()
  for (const [label, count] of [
    ['Total torrents', s.summary.total],
    ['In metadata', s.summary.metadata],
    ['Protected', s.summary.protected],
    ['Overdue', s.summary.overdue],
    ['Warned', s.summary.would_delete],
    ['Delete requested', s.summary.delete_requested],
    ['Confirmed / startup', s.since_startup.deletions],
    ['Warnings / startup', s.since_startup.would_deletions],
    ['Confirmed / lifetime', s.lifetime.deletions],
    ['Warnings / lifetime', s.lifetime.would_deletions]
  ]) {
    const card = document.createElement('div'),
      strong = document.createElement('strong'),
      span = document.createElement('span')
    card.className = 'card'
    strong.textContent = count
    span.textContent = label
    card.append(strong, span)
    cards.append(card)
  }
  byId('cards').replaceChildren(cards)
  const rows = document.createElement('tbody')
  for (const t of s.torrents) {
    const row = document.createElement('tr')
    // A clock exists only for a torrent currently inside a policy partition
    // and proven continuously observed there. Everything else shows a dash
    // rather than a guess, and never a hypothetical classification.
    const counting = Boolean(t.first_seen_policy)
    const overdue = counting && t.remaining_seconds <= 0
    if (overdue) row.className = 'overdue'
    const [headline, reason, blocked] = decisionOf(t.decision)
    const [left, leftDetail] = eligibility(t)
    cell(row, t.name, t.short_hash)
    cell(
      row,
      policyLabel(t.policy),
      t.policy ? `${t.policy} · ${t.state}` : `${t.state} · no policy matches`
    )
    cell(row, headline, reason, blocked ? 'flag-blocked' : 'flag-eligible')
    cell(
      row,
      t.policy ? actionLabel(t.configured_action) : '—',
      effectiveNote(t)
    )
    cell(row, `${(t.progress * 100).toFixed(2)}%`, `downloaded ${bytes(t.downloaded)} · size ${bytes(t.size || 0)} / total ${bytes(t.total_size || 0)} · left ${bytes(t.amount_left || 0)} · completed ${bytes(t.completed || 0)}`)
    cell(row, `${bytes(Math.max(0, t.download_speed))}/s`)
    cell(
      row,
      `${t.num_seeds} / ${t.num_leechers}`,
      t.seed_observed
        ? 'Seed connection observed'
        : 'No seed connection observed'
    )
    cell(row, t.category || '—', `${t.tags || 'No tags'}${(t.watchdog_tags || []).length ? ` · watchdog: ${(t.watchdog_tags || []).join(', ')}` : ''}`)
    cell(row, localTime(t.added_at))
    cell(
      row,
      counting ? duration(t.elapsed_seconds) : '—',
      t.policy
        ? `of ${duration(t.threshold_seconds)} required`
        : 'No episode clock'
    )
    cell(row, left, leftDetail, overdue ? 'flag-overdue' : undefined)
    rows.append(row)
  }
  empty(rows, 11, 'No torrents in the latest snapshot.')
  byId('torrents').replaceChildren(...rows.childNodes)
  const history = document.createElement('tbody')
  for (const e of [...s.history].reverse()) {
    const row = document.createElement('tr')
    cell(row, localTime(e.time))
    cell(row, e.action, e.integration || (e.policy ? policyLabel(e.policy) : 'Legacy event'))
    cell(row, e.name || '—', e.short_hash)
    cell(
      row,
      e.effective_action ? actionLabel(e.effective_action) : 'Unrecorded',
      e.dry_run ? 'Dry-run override' : 'Policy action'
    )
    cell(row, e.outcome, e.error || '')
    history.append(row)
  }
  empty(history, 5, 'No actions yet.')
  byId('history').replaceChildren(...history.childNodes)
}
let interval = 5000
async function refresh () {
  try {
    const response = await fetch('/api/v1/status', {
      cache: 'no-store',
      credentials: 'same-origin',
      signal: AbortSignal.timeout(10000)
    })
    if (!response.ok) throw new Error('Status unavailable')
    const snapshot = await response.json()
    render(snapshot)
    interval = Math.max(250, snapshot.refresh_seconds * 1000)
    text(
      'refresh-message',
      `Updated ${localTime(snapshot.updated_at)} · times shown locally`
    )
  } catch (_) {
    text(
      'refresh-message',
      'Status refresh failed. Displayed data may be stale; check connectivity or authentication.'
    )
  } finally {
    window.setTimeout(refresh, interval)
  }
}
refresh()
