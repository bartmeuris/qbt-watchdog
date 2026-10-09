# API

The HTTP surface of qbt-watchdog: pages, partials, JSON endpoints, health, metrics and assets. See also [the README](../README.md) and [monitoring](monitoring.md).

The server has **no built-in authentication**. Every endpoint is served to anyone who can reach `listen`; deploy behind a reverse proxy that terminates TLS and enforces access control. See [deployment](deployment.md).

## Endpoints

| Endpoint | Purpose | Protection |
| -------- | ------- | ---------- |
| `GET /` | Overview page: counters, torrent search/filter, torrent list, recent actions. | None (proxy). |
| `GET /policies` | Active policies page: effective-policy table and diagnostics. | None (proxy). |
| `GET /activity` | Activity page: retained history and media recovery. | None (proxy). |
| `GET /settings` | Structured Settings page; fetched once per navigation, never polled. | None (proxy). |
| `GET /partials/...` | Server-rendered HTML fragments. Full sections: `overview`, `policies`, `torrents`, `recovery`, `history`, `recent`, `settings`. Header: `services`. Raw editor: `settings-editor`. Live inner fragments: `overview-counters`, `torrent-rows`, `history-rows`, `recovery-rows`, `recent-rows`, `policy-items`. | None (proxy). |
| `GET /api/v1/status` | Schema-versioned in-memory snapshot; makes no qBittorrent request. | None (proxy). |
| `GET /api/v1/config` | Raw configuration document plus an opaque stamp for the raw editor. | None (proxy). |
| `POST /api/v1/config` | Save a raw-edited configuration; preserves comments; conflicts return 409, invalid config 422. | Same-origin CSRF. |
| `GET /api/v1/settings` | Structured settings read model (typed values, sources, secret metadata; never secret values). | None (proxy). |
| `GET /api/v1/settings/environment` | Environment variable names with availability/empty markers for the secret picker. | None (proxy). |
| `POST /api/v1/settings` | Apply a structured settings patch; field errors 422, conflicts 409. | Same-origin CSRF. |
| `POST /api/v1/actions` | Queue a manual action (`run_now` / `explicit`) by short hash; returns 202 and runs on the next poll. | Same-origin CSRF. |
| `GET /healthz` | HTTP 200 when the HTTP server is alive, independent of qBittorrent. | Always public. |
| `GET /readyz` | HTTP 200 after a successful, recent poll with healthy persistence; otherwise 503 with a reason. | Always public. |
| `GET /metrics` | Prometheus/OpenMetrics exposition. | Always public; protect at the proxy if needed. |
| `GET /assets/app.js`, `GET /assets/style.css`, `GET /assets/htmx.min.js` | Embedded static assets, without torrent data. | Public. |

Live inner fragments answer `204 No Content` when the snapshot has not advanced since the client's last fetch, and the client reconciles fragments by stable `data-key` identity rather than replacing the DOM wholesale.

## CSRF

The mutation endpoints (`POST /api/v1/config`, `POST /api/v1/settings`, `POST /api/v1/actions`) require a same-origin check, not a built-in username:

- an `HX-Request` header is present;
- `Sec-Fetch-Site`, when sent, is `same-origin` or `none`;
- `Origin`, when sent, has the same host as the request.

A failed check returns `403` with `{"error":"cross-site request rejected"}`. These headers cannot be set by a cross-site form or script, so they stand in for a CSRF token.

## Reverse proxy guidance

- Terminate TLS and require authentication at the proxy for every path you want to protect, including `/metrics` and the JSON API.
- Do not expose `listen` directly to an untrusted network; the loopback bind in the example Compose file does not stop other containers on the shared network.
- Keep the same origin end to end (host and scheme), because mutation CSRF validation compares the `Origin` host with the request host.
