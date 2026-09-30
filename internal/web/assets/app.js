'use strict'
// Client behaviour is deliberately tiny. All markup is server-rendered; HTMX
// swaps fragments, and this script only (1) turns UTC instants into local
// times, (2) toggles the light/dark theme, (3) drives ONE shared refresh
// coordinator, and (4) reflects manual-action / settings feedback. It never
// builds markup, so hostile values stay inert: every visible string is
// assigned through textContent.
;(function () {
  // Harden HTMX: no expression evaluation of any kind.
  if (window.htmx) {
    window.htmx.config.allowEval = false
  }

  const root = document.documentElement
  const body = document.body

  // --- Theme toggle ---------------------------------------------------------
  const STORAGE_KEY = 'qbt-watchdog-theme'
  const order = ['auto', 'light', 'dark']
  function applyTheme (theme) {
    root.setAttribute('data-theme', theme)
    root.style.colorScheme = theme === 'auto' ? 'light dark' : theme
    const control = document.getElementById('theme-toggle')
    if (control) {
      const label = control.querySelector('.theme-follow')
      if (label) label.textContent = 'Theme: ' + theme
    }
  }
  function nextTheme (current) {
    const i = order.indexOf(current)
    return order[(i + 1) % order.length]
  }
  let saved = null
  try {
    saved = window.localStorage.getItem(STORAGE_KEY)
  } catch (_) { /* storage unavailable */ }
  let theme = order.indexOf(saved) >= 0 ? saved : 'auto'
  applyTheme(theme)
  const toggleButton = document.getElementById('theme-toggle')
  if (toggleButton) {
    toggleButton.addEventListener('click', function () {
      theme = nextTheme(theme)
      try {
        window.localStorage.setItem(STORAGE_KEY, theme)
      } catch (_) { /* storage unavailable */ }
      applyTheme(theme)
    })
  }

  // --- Time localization ----------------------------------------------------
  const formatter = new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'medium'
  })
  const shortFormatter = new Intl.DateTimeFormat(undefined, {
    dateStyle: 'short',
    timeStyle: 'short'
  })
  // Only ever localizes a bounded subtree (<time> elements inside scope), so a
  // refresh never repaints the whole page.
  function localize (scope) {
    const scopeEl = scope || document
    const times = scopeEl.querySelectorAll
      ? scopeEl.querySelectorAll('time[datetime]')
      : []
    for (let i = 0; i < times.length; i++) {
      const el = times[i]
      const dt = el.getAttribute('datetime')
      if (!dt) continue
      const parsed = new Date(dt)
      if (Number.isNaN(parsed.getTime())) continue
      const fmt = el.hasAttribute('data-short') ? shortFormatter : formatter
      el.textContent = fmt.format(parsed)
    }
  }
  localize(document)

  // --- Torrent filter ------------------------------------------------------
  // Client-side name/state filtering. It only toggles the hidden attribute on
  // server-rendered rows; it never builds markup, so hostile names stay inert.
  function applyTorrentFilter () {
    const search = document.getElementById('torrent-search')
    const stateSel = document.getElementById('torrent-state')
    const query = search ? search.value.trim().toLowerCase() : ''
    const state = stateSel ? stateSel.value : ''
    const rows = document.querySelectorAll('#torrent-list details.torrent')
    for (let i = 0; i < rows.length; i++) {
      const row = rows[i]
      const name = (row.getAttribute('data-name') || '').toLowerCase()
      const rowState = row.getAttribute('data-state') || ''
      const matchesName = !query || name.indexOf(query) !== -1
      const matchesState = !state || rowState === state
      row.hidden = !(matchesName && matchesState)
    }
  }
  const torrentSearch = document.getElementById('torrent-search')
  const torrentState = document.getElementById('torrent-state')
  if (torrentSearch) torrentSearch.addEventListener('input', applyTorrentFilter)
  if (torrentState) torrentState.addEventListener('change', applyTorrentFilter)

  // --- Activity filter -----------------------------------------------------
  // Client-side name/policy/outcome/service filtering for the activity list.
  // It only toggles the hidden attribute on server-rendered rows; it never
  // builds markup, so hostile names stay inert.
  function applyHistoryFilter () {
    const search = document.getElementById('history-search')
    const policySel = document.getElementById('history-policy')
    const outcomeSel = document.getElementById('history-outcome')
    const serviceSel = document.getElementById('history-service')
    const query = search ? search.value.trim().toLowerCase() : ''
    const policy = policySel ? policySel.value : ''
    const outcome = outcomeSel ? outcomeSel.value : ''
    const service = serviceSel ? serviceSel.value : ''
    const rows = document.querySelectorAll('#history-list details.history')
    for (let i = 0; i < rows.length; i++) {
      const row = rows[i]
      const name = (row.getAttribute('data-name') || '').toLowerCase()
      const rowPolicy = row.getAttribute('data-policy') || ''
      const rowOutcome = row.getAttribute('data-outcome') || ''
      const rowService = row.getAttribute('data-service') || ''
      const matchesName = !query || name.indexOf(query) !== -1
      const matchesPolicy = !policy || rowPolicy === policy
      const matchesOutcome = !outcome || rowOutcome === outcome
      const matchesService = !service || rowService === service
      row.hidden = !(matchesName && matchesPolicy && matchesOutcome && matchesService)
    }
  }
  const historySearch = document.getElementById('history-search')
  const historyPolicy = document.getElementById('history-policy')
  const historyOutcome = document.getElementById('history-outcome')
  const historyService = document.getElementById('history-service')
  if (historySearch) historySearch.addEventListener('input', applyHistoryFilter)
  if (historyPolicy) historyPolicy.addEventListener('change', applyHistoryFilter)
  if (historyOutcome) historyOutcome.addEventListener('change', applyHistoryFilter)
  if (historyService) historyService.addEventListener('change', applyHistoryFilter)

  // Natural-navigation full page loads reset everything; the coordinator below
  // re-initialises per page, so there is nothing to unbind here.

  // --- Refresh coordinator ---------------------------------------------------
  // ONE timer drives every live fragment. It polls only what is actually on
  // this page ([data-refresh] containers), pauses while the tab is hidden,
  // never overlaps requests, and answers a 204 (no swap) from the server by
  // leaving the DOM — and its open rows, focus and selection — untouched.
  const interval = parseInt(root.getAttribute('data-refresh-seconds'), 10)
  const pollMs = (interval && interval > 0 ? interval : 30) * 1000

  const stamps = {}      // partial URL -> last snapshot stamp (for change suppression)
  let pending = 0        // in-flight fragment requests
  let generation = 0     // bumped each tick; late responses are ignored
  let timer = null
  let capturedFocus = null

  function fragments () {
    return document.querySelectorAll('[data-refresh]')
  }

  function schedule () {
    if (timer) window.clearTimeout(timer)
    timer = window.setTimeout(tick, pollMs)
  }

  function captureFocus () {
    const ae = document.activeElement
    if (ae && ae.closest) {
      const row = ae.closest('details[data-torrent], details[data-recovery], details[data-history]')
      capturedFocus = row ? row.id : null
    }
  }

  function restoreFocus () {
    if (!capturedFocus) return
    const row = document.getElementById(capturedFocus)
    capturedFocus = null
    if (!row) return
    const summary = row.querySelector('summary')
    if (summary) summary.focus()
  }

  function openParam (el) {
    const open = []
    const rows = el.querySelectorAll('details[open][data-torrent]')
    for (let i = 0; i < rows.length; i++) {
      const id = rows[i].getAttribute('data-torrent')
      if (id) open.push(id)
    }
    return open.join(',')
  }

  function request (el, g) {
    const url = el.getAttribute('data-refresh')
    let qs = 'digest=' + encodeURIComponent(stamps[url] || '')
    if (el.getAttribute('id') === 'torrent-list') {
      qs += '&open=' + encodeURIComponent(openParam(el))
    }
    const sep = url.indexOf('?') >= 0 ? '&' : '?'
    let settled = false
    const done = function (stale) {
      if (settled) return
      settled = true
      if (stale && g === generation) markStale(el)
      pending -= 1
      if (pending <= 0) {
        pending = 0
        restoreFocus()
        schedule()
      }
    }
    let promise = null
    try {
      promise = window.htmx.ajax('GET', url + sep + qs, { target: el })
    } catch (_) {
      promise = null
    }
    if (promise && typeof promise.then === 'function') {
      promise.then(function () { done(false) }, function () { done(true) })
    } else {
      done(false)
    }
  }

  function markStale (el) {
    // Retain the last good data and flag it stale; a backend outage must never
    // blank an already-rendered fragment.
    el.classList.add('stale')
  }

  function tick () {
    if (document.hidden) {
      schedule()
      return
    }
    if (pending > 0) {
      schedule()
      return
    }
    const els = fragments()
    if (els.length === 0) {
      schedule()
      return
    }
    generation += 1
    const g = generation
    pending = els.length
    captureFocus()
    for (let i = 0; i < els.length; i++) {
      request(els[i], g)
    }
  }

  if (window.htmx) {
    // Record the new snapshot stamp from each response so the next poll can ask
    // the server to suppress an unchanged fragment with a 204.
    body.addEventListener('htmx:afterRequest', function (evt) {
      const el = evt.detail && (evt.detail.target || evt.detail.elt)
      const url = el && el.getAttribute ? el.getAttribute('data-refresh') : null
      if (!url) return
      const xhr = evt.detail.xhr
      if (xhr && xhr.getResponseHeader && xhr.status === 200) {
        const stamp = xhr.getResponseHeader('X-Snapshot-Stamp')
        if (stamp) stamps[url] = stamp
      }
    })
    // Localize only the subtree that actually swapped, and re-apply the filter
    // so newly added rows respect the active search/state selection.
    body.addEventListener('htmx:afterSwap', function (evt) {
      const target = evt.detail && evt.detail.target
      if (target) localize(target)
      applyTorrentFilter()
      applyHistoryFilter()
    })
  }

  // Start only on pages that actually have live fragments.
  if (window.htmx && document.querySelector('[data-refresh]')) {
    schedule()
  }

  // Cheap fallback: re-localize once a minute in case a fragment ever arrived
  // without a swap hook. It touches only <time> elements, not the whole page.
  window.setInterval(function () { localize(document) }, 30000)

  // --- Settings editor status ---------------------------------------------
  if (window.htmx) {
    body.addEventListener('htmx:responseError', function (evt) {
      const status = document.getElementById('settings-status')
      if (!status || evt.detail.target.id !== 'settings-status') return
      const statusCode = evt.detail.xhr.status
      let message = 'Save failed.'
      try {
        const parsed = JSON.parse(evt.detail.xhr.responseText)
        if (parsed && parsed.error) message = parsed.error
      } catch (_) { /* non-JSON error body */ }
      if (statusCode === 409) message = 'Configuration changed on disk — reload and retry.'
      status.textContent = message
    })
    body.addEventListener('htmx:afterRequest', function (evt) {
      const status = document.getElementById('settings-status')
      if (!status || evt.detail.target.id !== 'settings-status') return
      const parsed = JSON.parse(evt.detail.xhr.responseText)
      if (parsed && parsed.status === 'applied') {
        status.textContent = 'Configuration applied (generation ' + parsed.generation + ').'
        return
      }
      if (parsed && parsed.status === 'rejected' && parsed.error) {
        status.textContent = parsed.error
      }
    })
  }

  // --- Manual actions status ----------------------------------------------
  if (window.htmx) {
    body.addEventListener('htmx:responseError', function (evt) {
      const status = evt.detail.target
      if (!status || !status.hasAttribute('data-action-status')) return
      let message = 'Action rejected.'
      try {
        const parsed = JSON.parse(evt.detail.xhr.responseText)
        if (parsed && parsed.error) message = parsed.error
      } catch (_) { /* non-JSON error body */ }
      status.textContent = message
      status.classList.remove('action-status-ok')
      status.classList.add('action-status-error')
    })
    body.addEventListener('htmx:afterRequest', function (evt) {
      const status = evt.detail.target
      if (!status || !status.hasAttribute('data-action-status')) return
      let message = 'Action queued.'
      try {
        const parsed = JSON.parse(evt.detail.xhr.responseText)
        if (parsed && parsed.accepted && parsed.decision) {
          message = 'Queued — decision: ' + parsed.decision
        }
      } catch (_) { /* non-JSON body */ }
      status.textContent = message
      status.classList.remove('action-status-error')
      status.classList.add('action-status-ok')
    })
  }

  // Pause while hidden; resume when the tab becomes visible again.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden && pending === 0) {
      schedule()
    }
  })
})()