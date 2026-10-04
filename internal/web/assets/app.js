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
      const formatted = fmt.format(parsed)
      // Writing an identical string would replace the text node and disturb a
      // selection, so only assign when the localized value actually changed.
      if (el.textContent !== formatted) el.textContent = formatted
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

  // Counts beside each state choice come from the full snapshot, never from the
  // name search: every server-rendered row stays in the DOM (hidden, not
  // removed), so counting all rows gives the pre-filter total. The select is
  // never rebuilt, so the operator's selection survives a count change.
  function updateStateCounts () {
    const stateSel = document.getElementById('torrent-state')
    if (!stateSel) return
    const rows = document.querySelectorAll('#torrent-list details.torrent')
    const counts = {}
    for (let i = 0; i < rows.length; i++) {
      const state = rows[i].getAttribute('data-state') || ''
      counts[state] = (counts[state] || 0) + 1
    }
    const options = stateSel.options
    for (let i = 0; i < options.length; i++) {
      const option = options[i]
      const label = option.getAttribute('data-state-label') || option.textContent
      const count = option.value === '' ? rows.length : (counts[option.value] || 0)
      const text = label + ' (' + count + ')'
      if (option.textContent !== text) option.textContent = text
    }
  }

  // --- Service popovers ----------------------------------------------------
  // The header indicators are native <details> disclosures, so they are
  // keyboard-operable and work without script. This layer makes them
  // hover-first: pointing at an indicator opens it, moving into the popover
  // keeps it open, and leaving both closes it. Opening one closes any other,
  // keyboard focus opens it, Escape dismisses it, and a tap still toggles it on
  // touch. The keyed coordinator preserves the `open` attribute across a
  // refresh, so an active popover survives a live update.
  let servicePointerInteraction = false
  document.addEventListener('pointerdown', function () { servicePointerInteraction = true }, true)
  document.addEventListener('keydown', function (evt) {
    servicePointerInteraction = false
    // Escape dismisses a hover-opened popover even when focus never entered it.
    if (evt.key !== 'Escape') return
    const open = document.querySelectorAll('.services details.service[open]')
    for (let i = 0; i < open.length; i++) open[i].open = false
  })

  // A WeakSet, not a DOM attribute: the keyed coordinator strips attributes the
  // server did not render, so a marker attribute would be removed on every
  // refresh and the listeners would be bound again and again.
  const boundServicePopovers = new WeakSet()

  function initServicePopovers () {
    const services = document.querySelectorAll('.services details.service')
    for (let i = 0; i < services.length; i++) {
      const svc = services[i]
      if (boundServicePopovers.has(svc)) continue
      boundServicePopovers.add(svc)
      bindServicePopover(svc)
    }
  }

  function bindServicePopover (svc) {
    const summary = svc.querySelector('summary')
    let lastPointerType = ''

    function closeOthers () {
      const all = document.querySelectorAll('.services details.service')
      for (let i = 0; i < all.length; i++) {
        if (all[i] !== svc) all[i].open = false
      }
    }

    // Opening one closes any other, however it was opened.
    svc.addEventListener('toggle', function () {
      if (svc.open) closeOthers()
    })

    // Hover opens; leaving the indicator and its popover closes. A pointer that
    // is still focused keeps it open.
    svc.addEventListener('pointerenter', function (evt) {
      if (evt.pointerType === 'mouse') svc.open = true
    })
    svc.addEventListener('pointerleave', function (evt) {
      if (evt.pointerType !== 'mouse') return
      if (svc.contains(document.activeElement)) return
      svc.open = false
    })

    // Keyboard focus opens; focus leaving the indicator closes it. Focus caused
    // by a pointer is left to the native toggle, so a tap does not open and
    // immediately close.
    svc.addEventListener('focusin', function () {
      if (servicePointerInteraction) return
      svc.open = true
    })
    svc.addEventListener('focusout', function (evt) {
      if (!svc.contains(evt.relatedTarget)) svc.open = false
    })

    // Escape dismisses and returns focus to the indicator.
    svc.addEventListener('keydown', function (evt) {
      if (evt.key !== 'Escape' || !svc.open) return
      svc.open = false
      if (summary) summary.focus()
    })

    // On a mouse, hover already governs the popover, so a click must not toggle
    // it shut. On touch and pen the native toggle is left intact.
    if (summary) {
      summary.addEventListener('pointerdown', function (evt) {
        lastPointerType = evt.pointerType || ''
      })
      summary.addEventListener('click', function (evt) {
        if (evt.detail === 0) return // keyboard activation: native toggle
        if (lastPointerType !== 'mouse') return
        evt.preventDefault()
        svc.open = true
      })
    }
  }

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

  // --- Keyed reconciliation --------------------------------------------------
  // `data-key` gives every reconcilable node a stable identity (a torrent short
  // hash, a history identity, a recovery id, a policy id, a service id, a
  // counter group). The coordinator patches matched nodes in place instead of
  // replacing list content, so an element that did not change is never rebuilt:
  // nested disclosures, focus, text selection, service popovers and
  // manual-action feedback all survive a refresh.

  function keyedChildren (node) {
    const out = []
    const kids = node.children || []
    for (let i = 0; i < kids.length; i++) {
      if (kids[i].getAttribute('data-key')) out.push(kids[i])
    }
    return out
  }

  function hasKeyed (node) {
    return keyedChildren(node).length > 0
  }

  // patchAttributes copies changed attributes only. `open` and `hidden` are
  // skipped because the live client state is authoritative for both: an open
  // disclosure and a filtered-out row must survive a refresh (the filter is
  // re-applied explicitly after reconciliation).
  function patchAttributes (live, fresh) {
    const freshAttrs = fresh.attributes
    for (let i = 0; i < freshAttrs.length; i++) {
      const attr = freshAttrs[i]
      if (attr.name === 'open' || attr.name === 'hidden') continue
      if (live.getAttribute(attr.name) !== attr.value) live.setAttribute(attr.name, attr.value)
    }
    const liveAttrs = Array.from(live.attributes)
    for (let i = 0; i < liveAttrs.length; i++) {
      const name = liveAttrs[i].name
      if (name === 'open' || name === 'hidden') continue
      if (!fresh.hasAttribute(name)) live.removeAttribute(name)
    }
  }

  // patchNode updates a matched node to match the server's fresh node without
  // replacing it, so node identity (and therefore focus, selection and open
  // state) is preserved.
  function patchNode (live, fresh, report) {
    if (live.nodeType !== fresh.nodeType || live.nodeName !== fresh.nodeName) {
      live.replaceWith(fresh.cloneNode(true))
      return
    }
    if (live.nodeType === 3) {
      if (live.nodeValue !== fresh.nodeValue) live.nodeValue = fresh.nodeValue
      return
    }
    if (live.nodeType !== 1) return

    // Manual-action feedback is operator-visible text the server never
    // re-renders; keep it until the page is reloaded so it can be read. The
    // ok/error classes are added by this script, not the server, so they are
    // merged back after the server's class attribute is synced.
    if (live.hasAttribute('data-action-status') && live.textContent.trim() !== '') {
      const statusClasses = []
      if (live.classList.contains('action-status-ok')) statusClasses.push('action-status-ok')
      if (live.classList.contains('action-status-error')) statusClasses.push('action-status-error')
      patchAttributes(live, fresh)
      for (let i = 0; i < statusClasses.length; i++) live.classList.add(statusClasses[i])
      return
    }

    // A <time> carries its machine instant in `datetime`; compare that, never
    // the localized text, so a tick never rewrites an unchanged timestamp.
    if (live.tagName === 'TIME' && fresh.hasAttribute('datetime')) {
      const dt = fresh.getAttribute('datetime')
      if (live.getAttribute('datetime') !== dt) {
        live.setAttribute('datetime', dt)
        live.setAttribute('title', fresh.getAttribute('title') || dt)
      }
      return
    }

    const keepOpen = live.tagName === 'DETAILS' ? live.open : null
    patchAttributes(live, fresh)
    patchChildren(live, fresh, false, report)
    if (keepOpen !== null) live.open = keepOpen
  }

  function patchChildren (live, fresh, keepOrder, report) {
    if (hasKeyed(live) || hasKeyed(fresh)) {
      patchKeyedChildren(live, fresh, keepOrder, report)
      return
    }
    patchPositionalChildren(live, fresh)
  }

  // Positional patching is for structural, unkeyed subtrees (a summary, a
  // definition list): children are matched by position and patched in place.
  function patchPositionalChildren (live, fresh) {
    const liveKids = Array.from(live.childNodes)
    const freshKids = Array.from(fresh.childNodes)
    const count = Math.max(liveKids.length, freshKids.length)
    for (let i = 0; i < count; i++) {
      const l = liveKids[i]
      const f = freshKids[i]
      if (!f) {
        if (l) l.remove()
        continue
      }
      if (!l) {
        live.appendChild(f.cloneNode(true))
        continue
      }
      patchNode(l, f, null)
    }
  }

  // patchKeyedChildren matches direct children by `data-key`: matched nodes are
  // patched in place, new nodes are inserted, and vanished nodes are removed.
  // When keepOrder is set (the torrent list) an existing row is never moved, so
  // a countdown crossing a threshold cannot reshuffle the list; new rows are
  // placed relative to the server's ordering around the rows that remain.
  function patchKeyedChildren (live, fresh, keepOrder, report) {
    const freshEls = keyedChildren(fresh)
    const liveKeyed = keyedChildren(live)
    const liveByKey = {}
    for (let i = 0; i < liveKeyed.length; i++) {
      liveByKey[liveKeyed[i].getAttribute('data-key')] = liveKeyed[i]
    }
    const ordered = []
    const inserted = []
    for (let i = 0; i < freshEls.length; i++) {
      const fEl = freshEls[i]
      const key = fEl.getAttribute('data-key')
      const existing = liveByKey[key]
      if (existing) {
        patchNode(existing, fEl, report)
        ordered.push(existing)
        continue
      }
      const created = fEl.cloneNode(true)
      inserted.push(created)
      ordered.push(created)
      if (report) report.added.push(key)
    }
    for (let i = 0; i < liveKeyed.length; i++) {
      if (ordered.indexOf(liveKeyed[i]) === -1) liveKeyed[i].remove()
    }
    if (!keepOrder) {
      // Only reorder when the server's order actually differs from the DOM, so
      // an unchanged sequence costs zero node moves (and no scroll disturbance).
      const current = keyedChildren(live)
      let orderedSame = current.length === ordered.length
      if (orderedSame) {
        for (let i = 0; i < ordered.length; i++) {
          if (current[i] !== ordered[i]) { orderedSame = false; break }
        }
      }
      if (orderedSame) return
      let ref = live.firstChild
      for (let i = 0; i < ordered.length; i++) {
        if (ordered[i] !== ref) live.insertBefore(ordered[i], ref)
        ref = ordered[i].nextSibling
      }
      return
    }
    for (let i = 0; i < ordered.length; i++) {
      const node = ordered[i]
      if (inserted.indexOf(node) === -1) continue
      let after = null
      for (let j = i - 1; j >= 0; j--) {
        if (inserted.indexOf(ordered[j]) === -1) { after = ordered[j]; break }
      }
      if (after) {
        live.insertBefore(node, after.nextSibling)
        continue
      }
      let before = null
      for (let j = i + 1; j < ordered.length; j++) {
        if (inserted.indexOf(ordered[j]) === -1) { before = ordered[j]; break }
      }
      if (before) live.insertBefore(node, before)
      else live.appendChild(node)
    }
  }

  function reconcile (container, freshRoot, keepOrder) {
    const report = { added: [] }
    patchChildren(container, freshRoot, keepOrder, report)
    return report
  }

  // --- Refresh coordinator ---------------------------------------------------
  // ONE timer drives every live fragment. Each tick fetches the fragments on
  // this page, answers a 204 (snapshot unchanged) by doing nothing, and
  // reconciles any changed fragment by stable identity. It pauses while the tab
  // is hidden, keeps one page update in flight, and drops obsolete responses.
  const interval = parseInt(root.getAttribute('data-refresh-seconds'), 10)
  const pollMs = (interval && interval > 0 ? interval : 30) * 1000

  const stamps = {}      // partial URL -> last snapshot stamp (server-side suppression)
  const lastBody = {}    // partial URL -> last rendered HTML (client-side no-op skip)
  let pending = 0        // in-flight fragment requests
  let generation = 0     // bumped each tick; late responses are ignored
  let timer = null
  let anchor = null      // visible-row scroll anchor captured at tick start

  function fragments () {
    return document.querySelectorAll('[data-refresh]')
  }

  function schedule () {
    if (timer) window.clearTimeout(timer)
    timer = window.setTimeout(tick, pollMs)
  }

  function markStale (el) {
    // Retain the last good data and flag it stale; a backend outage must never
    // blank an already-rendered fragment.
    el.classList.add('stale')
  }

  function clearStale (el) {
    // Clear the marker once a poll succeeds again, including a 204 no-change.
    el.classList.remove('stale')
  }

  // captureAnchor records the first keyed row still visible from the top of the
  // viewport, so content inserted above it cannot jump the page under the
  // reader. It is a no-op when the page is already scrolled to the top, where
  // new entries should simply appear.
  function captureAnchor () {
    if (window.scrollY <= 0) return null
    const els = document.querySelectorAll('[data-key]')
    for (let i = 0; i < els.length; i++) {
      const rect = els[i].getBoundingClientRect()
      if (rect.bottom > 0) {
        return {
          key: els[i].getAttribute('data-key'),
          scope: els[i].closest('[data-refresh]'),
          top: rect.top,
          scrollY: window.scrollY
        }
      }
    }
    return null
  }

  function restoreAnchor () {
    if (!anchor) return
    const saved = anchor
    anchor = null
    // If the operator scrolled during the tick, respect their position.
    if (Math.abs(window.scrollY - saved.scrollY) > 1) return
    const scope = saved.scope || document
    const els = scope.querySelectorAll('[data-key]')
    let target = null
    for (let i = 0; i < els.length; i++) {
      if (els[i].getAttribute('data-key') === saved.key) { target = els[i]; break }
    }
    if (!target) return
    const delta = target.getBoundingClientRect().top - saved.top
    if (delta) window.scrollBy(0, delta)
  }

  function announceNewHistory (count) {
    const region = document.getElementById('history-new')
    if (!region) return
    region.textContent = count === 1
      ? '1 new activity entry is available above.'
      : count + ' new activity entries are available above.'
  }

  function clearHistoryAnnouncement () {
    const region = document.getElementById('history-new')
    if (region) region.textContent = ''
  }

  function applyFilters () {
    applyTorrentFilter()
    applyHistoryFilter()
    updateStateCounts()
  }

  // fetchFragment returns a promise that always settles (never rejects), so the
  // coordinator's in-flight accounting stays exact even through a network error.
  function fetchFragment (el, g) {
    const url = el.getAttribute('data-refresh')
    const qs = 'digest=' + encodeURIComponent(stamps[url] || '')
    const sep = url.indexOf('?') >= 0 ? '&' : '?'
    let request = null
    try {
      request = window.fetch(url + sep + qs, { credentials: 'same-origin' })
    } catch (_) {
      if (g === generation) markStale(el)
      return Promise.resolve()
    }
    return request
      .then(function (response) {
        if (g !== generation) return
        if (response.status === 204) {
          clearStale(el)
          return
        }
        if (!response.ok) {
          markStale(el)
          return
        }
        const stamp = response.headers.get('X-Snapshot-Stamp')
        if (stamp) stamps[url] = stamp
        return response.text().then(function (html) {
          if (g !== generation) return
          // Unchanged fragment: skip parsing and all DOM work entirely.
          if (lastBody[url] === html) {
            clearStale(el)
            return
          }
          lastBody[url] = html
          const parsed = new DOMParser().parseFromString(html, 'text/html')
          if (!parsed || !parsed.body) return
          const keepOrder = el.getAttribute('data-live-order') === 'keep'
          try {
            const report = reconcile(el, parsed.body, keepOrder)
            localize(el)
            applyFilters()
            initServicePopovers()
            if (el.id === 'history-list') {
              if (report.added.length > 0 && window.scrollY > 0) announceNewHistory(report.added.length)
              else if (window.scrollY <= 0) clearHistoryAnnouncement()
            }
          } catch (_) {
            markStale(el)
            return
          }
          clearStale(el)
        })
      })
      .catch(function () {
        if (g === generation) markStale(el)
      })
  }

  function finished () {
    pending -= 1
    if (pending > 0) return
    pending = 0
    restoreAnchor()
    schedule()
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
    anchor = captureAnchor()
    for (let i = 0; i < els.length; i++) {
      fetchFragment(els[i], g).then(finished, finished)
    }
  }

  if (window.htmx) {
    // Any HTMX swap (a manual action's status, the raw editor) localizes and
    // re-applies the filters to the swapped subtree; live fragments are handled
    // by the coordinator above and never go through HTMX.
    body.addEventListener('htmx:afterSwap', function (evt) {
      const target = evt.detail && evt.detail.target
      if (target) localize(target)
      applyFilters()
    })
  }

  // Start only on pages that actually have live fragments. The Settings page has
  // none, so its form can never be replaced by a refresh.
  if (document.querySelector('[data-refresh]')) {
    schedule()
  }

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
      let parsed = null
      try { parsed = JSON.parse(evt.detail.xhr.responseText) } catch (_) { return }
      if (!parsed) return
      // The raw editor posts to /api/v1/config, which answers with the same
      // SaveResult shape as the structured form: saved and applied are separate
      // facts, so a persisted-but-unapplied document must warn rather than read
      // as success.
      if (parsed.saved && parsed.applied) {
        setSettingsStatus('Configuration saved and applied.', 'ok')
        return
      }
      if (parsed.saved && !parsed.applied) {
        setSettingsStatus('Saved, but not applied: ' + (parsed.message || 'the previous configuration remains active.'), 'warn')
        return
      }
      if (parsed.error) setSettingsStatus(parsed.error, 'error')
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

  // --- Structured settings form -------------------------------------------
  // The form is server-rendered from the typed settings model. This script only
  // tracks the draft, toggles the secret source controls, filters the
  // environment list, and reflects the save result. It never builds markup from
  // data: every visible string is assigned through textContent.
  const settingsForm = document.getElementById('settings-form')
  if (settingsForm) {
    initSettingsForm(settingsForm)
  }

  function initSettingsForm (form) {
    form.addEventListener('input', markDirty)
    form.addEventListener('change', markDirty)

    const controls = form.querySelectorAll('.secret-control')
    for (let i = 0; i < controls.length; i++) initSecretControl(controls[i])

    const authMode = document.getElementById('qbt_auth_mode')
    if (authMode) {
      authMode.addEventListener('change', applyAuthMode)
      applyAuthMode()
    }

    const discard = document.getElementById('settings-discard')
    if (discard) {
      discard.addEventListener('click', function () {
        form.reset()
        clearFieldErrors()
        applyAllSecretTypes()
        setSettingsStatus('Draft discarded.', 'ok')
        markClean()
      })
    }

    if (window.htmx) {
      body.addEventListener('htmx:afterRequest', function (evt) {
        if (!evt.detail || evt.detail.elt !== form) return
        const xhr = evt.detail.xhr
        if (!xhr || xhr.status !== 200) return
        let parsed = null
        try { parsed = JSON.parse(xhr.responseText) } catch (_) { return }
        applySaveResult(parsed, form)
      })
      body.addEventListener('htmx:responseError', function (evt) {
        if (!evt.detail || evt.detail.elt !== form) return
        handleSaveError(evt.detail.xhr)
      })
    }

    revealHash()
    window.addEventListener('hashchange', revealHash)
  }

  function markDirty () {
    settingsForm.classList.add('settings-dirty-form')
    const dirty = document.getElementById('settings-dirty')
    if (dirty) dirty.hidden = false
    const save = document.getElementById('settings-save')
    const discard = document.getElementById('settings-discard')
    if (save) save.disabled = false
    if (discard) discard.disabled = false
  }

  function markClean () {
    settingsForm.classList.remove('settings-dirty-form')
    const dirty = document.getElementById('settings-dirty')
    if (dirty) dirty.hidden = true
    const save = document.getElementById('settings-save')
    const discard = document.getElementById('settings-discard')
    if (save) save.disabled = true
    if (discard) discard.disabled = true
  }

  function setSettingsStatus (text, kind) {
    const status = document.getElementById('settings-status')
    if (!status) return
    status.textContent = text
    status.className = 'settings-status settings-status-' + kind
  }

  function applySaveResult (result, form) {
    if (result.stamp) {
      const stamp = form.querySelector('input[name="stamp"]')
      if (stamp) stamp.value = result.stamp
    }
    if (result.saved && result.applied) {
      setSettingsStatus('Saved and applied. Policy, interval, and dry-run changes reset observation timers.', 'ok')
      resetSecretDrafts()
      captureBaseline(form)
      markClean()
      return
    }
    if (result.saved && !result.applied) {
      setSettingsStatus('Saved, but not applied: ' + (result.message || 'the previous configuration remains active.'), 'warn')
      resetSecretDrafts()
      captureBaseline(form)
      markClean()
      return
    }
    setSettingsStatus('Save did not complete.', 'error')
  }

  // captureBaseline makes the just-saved values the new reset target, so a
  // later Discard restores the saved state rather than the page-load state.
  function captureBaseline (form) {
    const controls = form.querySelectorAll('input, select, textarea')
    for (let i = 0; i < controls.length; i++) {
      const el = controls[i]
      if (el.type === 'checkbox' || el.type === 'radio') {
        el.defaultChecked = el.checked
      } else if (el.tagName === 'SELECT') {
        for (let j = 0; j < el.options.length; j++) el.options[j].defaultSelected = el.options[j].selected
      } else {
        el.defaultValue = el.value
      }
    }
  }

  // resetSecretDrafts clears the one-shot value/clear drafts after a save so a
  // second save does not resend them, and re-bases the source metadata.
  function resetSecretDrafts () {
    const controls = settingsForm.querySelectorAll('.secret-control')
    for (let i = 0; i < controls.length; i++) {
      const control = controls[i]
      const select = control.querySelector('.secret-type')
      if (select) control.setAttribute('data-original-source', select.value)
      const value = control.querySelector('.secret-field-value input[type="password"]')
      if (value) value.value = ''
      const clear = control.querySelector('.secret-clear input[type="checkbox"]')
      if (clear) {
        clear.checked = false
        setSecretCleared(control, false)
      }
    }
  }

  function handleSaveError (xhr) {
    if (!xhr) return
    if (xhr.status === 409) {
      showConflict()
      return
    }
    let parsed = null
    try { parsed = JSON.parse(xhr.responseText) } catch (_) { /* non-JSON body */ }
    if (parsed && parsed.errors && parsed.errors.length) {
      showFieldErrors(parsed.errors)
      setSettingsStatus('Some values were rejected. Your draft is preserved.', 'error')
      return
    }
    setSettingsStatus((parsed && parsed.error) || 'Save failed. Your draft is preserved.', 'error')
  }

  function showConflict () {
    const status = document.getElementById('settings-status')
    if (!status) return
    status.textContent = ''
    const message = document.createElement('span')
    message.textContent = 'Configuration changed on disk. Reload to see the current values — your draft has not been saved. '
    status.appendChild(message)
    const link = document.createElement('a')
    link.href = window.location.pathname + window.location.hash
    link.textContent = 'Reload settings'
    status.appendChild(link)
    status.className = 'settings-status settings-status-error'
  }

  function fieldElement (field) {
    return document.getElementById(field) ||
      document.getElementById(field.replace(/_/g, '-')) ||
      document.getElementById('secret-' + field)
  }

  function clearFieldErrors () {
    const notes = settingsForm.querySelectorAll('.setting-error')
    for (let i = 0; i < notes.length; i++) notes[i].remove()
  }

  function showFieldErrors (errors) {
    clearFieldErrors()
    for (let i = 0; i < errors.length; i++) {
      const target = fieldElement(errors[i].field)
      if (!target) continue
      const note = document.createElement('p')
      note.className = 'setting-error'
      note.setAttribute('role', 'alert')
      note.textContent = errors[i].message
      target.appendChild(note)
    }
  }

  function initSecretControl (control) {
    const select = control.querySelector('.secret-type')
    if (select) {
      select.addEventListener('change', function () {
        applySecretType(control)
        markDirty()
      })
    }
    const clear = control.querySelector('.secret-clear input[type="checkbox"]')
    if (clear) {
      clear.addEventListener('change', function () {
        setSecretCleared(control, clear.checked)
        markDirty()
      })
    }
    const combo = control.querySelector('[data-env-combo]')
    if (combo) initEnvCombo(combo)
    applySecretType(control)
  }

  function applySecretType (control) {
    const select = control.querySelector('.secret-type')
    if (!select) return
    control.setAttribute('data-active-type', select.value)
  }

  function applyAllSecretTypes () {
    const controls = settingsForm.querySelectorAll('.secret-control')
    for (let i = 0; i < controls.length; i++) applySecretType(controls[i])
  }

  function setSecretCleared (control, cleared) {
    control.classList.toggle('secret-cleared', cleared)
    const fields = control.querySelectorAll('.secret-field input, .secret-field select, .secret-type')
    for (let i = 0; i < fields.length; i++) fields[i].disabled = cleared
  }

  // applyAuthMode shows only the credential controls the selected qBittorrent
  // authentication mode can use, so an operator never edits a dead field.
  function applyAuthMode () {
    const select = document.getElementById('qbt_auth_mode')
    if (!select) return
    const mode = select.value
    const fields = settingsForm.querySelectorAll('[data-auth-mode]')
    for (let i = 0; i < fields.length; i++) {
      fields[i].hidden = fields[i].getAttribute('data-auth-mode') !== mode
    }
  }

  function initEnvCombo (combo) {
    const search = combo.querySelector('.env-search')
    const hidden = combo.querySelector('input[type="hidden"]')
    const options = combo.querySelectorAll('.env-option')
    function filter () {
      const query = search ? search.value.trim().toLowerCase() : ''
      for (let i = 0; i < options.length; i++) {
        const name = (options[i].getAttribute('data-name') || '').toLowerCase()
        options[i].hidden = query !== '' && name.indexOf(query) === -1
      }
    }
    if (search) {
      search.addEventListener('focus', function () { combo.classList.add('open') })
      search.addEventListener('input', function () { combo.classList.add('open'); filter() })
    }
    for (let i = 0; i < options.length; i++) {
      options[i].addEventListener('click', function () {
        const name = options[i].getAttribute('data-name') || ''
        if (hidden) hidden.value = name
        if (search) search.value = name
        for (let j = 0; j < options.length; j++) {
          options[j].setAttribute('aria-selected', options[j] === options[i] ? 'true' : 'false')
        }
        combo.classList.remove('open')
        markDirty()
      })
    }
    const refresh = combo.parentElement.querySelector('[data-env-refresh]')
    if (refresh) refresh.addEventListener('click', function () { refreshEnvironment(combo) })
    document.addEventListener('click', function (evt) {
      if (!combo.contains(evt.target)) combo.classList.remove('open')
    })
  }

  // refreshEnvironment re-reads availability metadata and updates the existing
  // options in place, so the operator's selection survives the refresh.
  function refreshEnvironment (combo) {
    const refresh = combo.parentElement.querySelector('[data-env-refresh]')
    if (refresh) refresh.disabled = true
    window.fetch('/api/v1/settings/environment', { headers: { Accept: 'application/json' } })
      .then(function (response) { return response.ok ? response.json() : null })
      .then(function (payload) {
        if (!payload || !payload.environment) return
        const byName = {}
        for (let i = 0; i < payload.environment.length; i++) {
          byName[payload.environment[i].name] = payload.environment[i]
        }
        const options = combo.querySelectorAll('.env-option')
        for (let i = 0; i < options.length; i++) {
          const option = options[i]
          const entry = byName[option.getAttribute('data-name')]
          const available = entry ? entry.available : false
          const empty = entry ? entry.empty : false
          option.setAttribute('data-available', available ? 'true' : 'false')
          option.setAttribute('data-empty', empty ? 'true' : 'false')
          option.classList.toggle('env-missing', !available)
          option.classList.toggle('env-empty', available && empty)
          const marker = option.querySelector('.env-marker')
          if (!available || empty) {
            const text = !available ? 'Missing' : 'Empty'
            if (marker) marker.textContent = text
            else {
              const span = document.createElement('span')
              span.className = 'env-marker'
              span.textContent = text
              option.appendChild(span)
            }
          } else if (marker) {
            marker.remove()
          }
        }
      })
      .catch(function () { /* keep the last known availability */ })
      .then(function () { if (refresh) refresh.disabled = false })
  }

  // revealHash opens any ancestor disclosure and focuses the deep-linked field,
  // so /settings#policy-<id> and /settings#dry-run land on a visible target.
  function revealHash () {
    const hash = window.location.hash
    if (!hash || hash.length < 2) return
    const target = document.getElementById(hash.slice(1))
    if (!target) return
    let node = target
    while (node) {
      if (node.tagName === 'DETAILS') node.open = true
      node = node.parentElement
    }
    target.scrollIntoView({ block: 'center' })
    target.classList.add('deep-link-target')
    const focusable = target.querySelector('input:not([type="hidden"]), select, textarea, button, a')
    if (focusable) focusable.focus({ preventScroll: true })
  }

  // Pause while hidden; refresh promptly when the tab becomes visible again.
  document.addEventListener('visibilitychange', function () {
    if (document.hidden || pending > 0) return
    if (timer) window.clearTimeout(timer)
    timer = window.setTimeout(tick, 150)
  })

  // Bind the header popovers on first paint; the coordinator re-binds any
  // indicator a live update inserts.
  initServicePopovers()
})()