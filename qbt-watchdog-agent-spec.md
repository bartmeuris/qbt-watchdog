# Build Specification: `qbt-watchdog`

## Instruction to the implementation agent

Implement the complete project described below. Treat this document as the source of truth. Produce working code, tests, documentation, and a production-ready multi-stage Docker image; do not stop at a design, pseudocode, or scaffolding.

Make pragmatic decisions where minor details are unspecified. Do not expand the scope into a general qBittorrent manager.

## Goal

Build a small, stateful Go daemon named `qbt-watchdog` that prevents magnet downloads stuck in qBittorrent's **Downloading metadata** state from occupying download queue slots indefinitely.

The daemon must:

1. Poll qBittorrent through its WebUI API.
2. Track how long each torrent has been continuously observed in the `metaDL` state.
3. Delete torrents that remain in `metaDL` longer than a configurable timeout.
4. Default to a safe dry-run mode.
5. Persist its tracking state across short restarts.
6. Expose a small web UI showing current torrent and watchdog status, plus authenticated settings editing and manual actions.
7. Expose health, readiness, JSON status, and Prometheus metrics endpoints.
8. Build and run as a small, non-root Docker container using a multi-stage build.

Deletion must use `deleteFiles=false` by default.

## Current technical baseline

- Use the latest stable Go release available when implementation begins.
- As of **2026-09-12**, the latest stable release is **Go 1.27.1**: <https://go.dev/dl/>.
- Re-check the official Go downloads page before implementation. Pin the exact current patch release in the builder image and `toolchain` directive rather than using an unpinned `latest` image.
- Set the `go` directive to the corresponding language version.
- Target qBittorrent WebUI API v2 as documented for qBittorrent 5.x: <https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-5.0)>.
- Keep compatibility with recent qBittorrent 4.x/5.x versions that expose the required v2 endpoints.
- Build with `CGO_ENABLED=0` and introduce no runtime dependencies.

## Required qBittorrent API usage

Use these endpoints:

| Purpose | Method and endpoint | Notes |
| --- | --- | --- |
| Authenticate | `POST /api/v2/auth/login` | Form fields `username` and `password`; retain the `SID` cookie. |
| Application version | `GET /api/v2/app/version` | Display in status UI. |
| Web API version | `GET /api/v2/app/webapiVersion` | Display in status UI. |
| List torrents | `GET /api/v2/torrents/info` | Fetch all torrents; do not rely on the broad `downloading` filter to identify `metaDL`. |
| Confirm one torrent | `GET /api/v2/torrents/info?hashes=<hash>` | Required immediately before deletion. |
| Delete one torrent | `POST /api/v2/torrents/delete` | Form fields `hashes=<hash>` and `deleteFiles=<bool>`. |

Use a cookie jar and a reusable `http.Client`. Set a clear `User-Agent`, a correct same-origin `Referer`, and appropriate request timeouts. Resolve endpoints through `net/url`; do not concatenate untrusted URL strings.

Authentication requirements:

- If both username and password are absent, operate without an explicit login so qBittorrent's configured local-auth bypass can work.
- If credentials are configured, perform login and verify both the response body and session cookie; an HTTP 200 alone does not necessarily mean authentication succeeded.
- If only one credential is supplied, fail configuration validation.
- On an authentication failure or expired session, invalidate the local session and retry authentication once. Do not repeatedly hammer the login endpoint because qBittorrent can temporarily ban clients after failed logins.
- Apply bounded exponential backoff with jitter to subsequent poll attempts after API failures.
- Never log or expose credentials, cookies, magnet URIs, save paths, or full request bodies.

Only the exact `metaDL` state is subject to this watchdog. Do not treat `stalledDL`, `queuedDL`, `pausedDL`, `downloading`, `unknown`, or any other state as equivalent.

At minimum, decode these fields from `/torrents/info`; unknown response fields must be tolerated:

```go
type Torrent struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Progress      float64 `json:"progress"`
	Downloaded    int64   `json:"downloaded"`
	DownloadSpeed int64   `json:"dlspeed"`
	NumSeeds      int     `json:"num_seeds"`
	NumLeechers   int     `json:"num_leechs"`
	AddedOn       int64   `json:"added_on"`
	Category      string  `json:"category"`
	Tags          string  `json:"tags"`
}
```

## Watchdog semantics

### Polling

- Run the first poll immediately after startup; do not wait one full interval.
- Never overlap polls. A slow poll delays the next one rather than creating concurrent polling goroutines.
- Use contexts throughout and support graceful shutdown on `SIGINT` and `SIGTERM`.
- A failed poll must not result in deletion or reset all persisted state.
- Publish the poll error and degraded status to logs, metrics, the JSON endpoint, and the UI.

### Continuous metadata tracking

For every successful poll:

1. If a torrent is in `metaDL` and is not tracked, create a record with `first_seen_meta` and `last_seen_meta` equal to the current UTC time.
2. If it is already tracked and the prior observation is recent enough to establish continuity, preserve `first_seen_meta` and update `last_seen_meta`.
3. If the observation gap exceeds `max-observation-gap`, reset `first_seen_meta` to now. Default this gap to three times the configured poll interval unless explicitly set.
4. If the torrent changes to any state other than `metaDL`, immediately remove its tracking record.
5. If a previously tracked torrent no longer exists, remove its tracking record.

This clock represents **continuous observed time in `metaDL`**. Do not use `time_active`, `added_on`, or daemon uptime as a substitute.

### Eligibility for deletion

A torrent becomes eligible only when all these conditions hold:

- Its exact state is `metaDL`.
- It has continuously been observed there for at least `metadata-timeout`.
- It is not excluded by category or tag configuration.
- It satisfies the configurable safety guards for zero progress and zero downloaded bytes.
- It has not already had a successful action in the current `metaDL` episode.

Before deleting, make a fresh targeted API request for that hash. Delete only if the torrent still exists and still satisfies every rule. This closes the race between a poll and metadata arriving.

Process eligible torrents deterministically, oldest `first_seen_meta` first, and enforce `max-deletions-per-poll`. Use one delete request per torrent so failures and audit events can be attributed correctly. qBittorrent documents HTTP 200 for all delete scenarios, so treat HTTP 200 only as an accepted request: record `delete requested`, then confirm on a later successful list poll that the torrent disappeared. If it remains beyond `delete-confirmation-timeout`, clear the pending marker and allow a bounded retry.

Never pass `hashes=all`.

### Dry-run behavior

Dry-run must default to `true`.

When an eligible torrent reaches its timeout in dry-run mode:

- Do not call the delete endpoint.
- Mark it as `would delete` in the status snapshot and UI.
- Emit one structured `would_delete` audit/log event for that metadata episode, not one every poll.
- Keep tracking it so elapsed and overdue time continue to update.
- Reset the episode marker if it leaves `metaDL` and later re-enters.

### Deletion behavior

When dry-run is disabled:

- Call the delete endpoint with the configured `delete-files` value, default `false`.
- On HTTP 200, emit a `delete_requested` audit event and increment the requested-action metric.
- Keep a bounded action history for the UI. Do not assume the torrent vanishes instantly; mark it `delete requested` until a later successful poll confirms it is gone, then emit `delete_confirmed`.
- If it changes state before the confirmation request, skip deletion and reset its metadata tracking.
- Never delete a torrent solely because its name, age, peer count, availability, or general active time looks suspicious.

### Safety behavior during outages and restarts

- Persist `first_seen_meta`, `last_seen_meta`, per-episode dry-run notification state, aggregate counters, and bounded action history.
- Use the observation-gap rule after a long API outage or daemon downtime. A long unobserved period must not count as proven continuous `metaDL` time.
- If the state file is absent, start with empty state.
- If it is corrupt, preserve it with a timestamped `.corrupt` suffix, log the problem, start with empty tracking state, and remain otherwise operational.
- Write state atomically using a temporary file in the same directory, flush it, set restrictive permissions, and rename it over the target.
- A persistence write failure must make readiness degraded and be clearly visible, but must not crash the poll loop.

## Configuration

Support equivalent CLI flags and environment variables. CLI flags take precedence over environment variables, which take precedence over defaults. Parse durations using Go duration syntax such as `30s`, `15m`, and `2h`.

Use the environment prefix `QBTW_`.

| CLI flag | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `--qbt-url` | `QBTW_QBT_URL` | required | qBittorrent WebUI base URL, for example `http://qbittorrent:8080`. |
| `--qbt-username` | `QBTW_QBT_USERNAME` | empty | WebUI username. |
| `--qbt-password` | `QBTW_QBT_PASSWORD` | empty | WebUI password; supported, but a password file is preferred. |
| `--qbt-password-file` | `QBTW_QBT_PASSWORD_FILE` | empty | Read password from a mounted secret file. Mutually exclusive with a directly supplied password. |
| `--poll-interval` | `QBTW_POLL_INTERVAL` | `30s` | Normal interval between completed polls. |
| `--metadata-timeout` | `QBTW_METADATA_TIMEOUT` | `30m` | Continuous observed `metaDL` time before action. |
| `--max-observation-gap` | `QBTW_MAX_OBSERVATION_GAP` | `3 × poll interval` | Maximum gap that still counts as continuous observation. |
| `--http-timeout` | `QBTW_HTTP_TIMEOUT` | `10s` | Overall timeout for each qBittorrent API request. |
| `--dry-run` | `QBTW_DRY_RUN` | `true` | Report eligible torrents without deleting them. |
| `--delete-files` | `QBTW_DELETE_FILES` | `false` | Pass `deleteFiles=true` to qBittorrent. This is intentionally dangerous and must be documented prominently. |
| `--max-deletions-per-poll` | `QBTW_MAX_DELETIONS_PER_POLL` | `10` | Safety cap; `0` must mean no deletion, not unlimited. |
| `--delete-confirmation-timeout` | `QBTW_DELETE_CONFIRMATION_TIMEOUT` | `2m` | Wait this long for a requested deletion to disappear before allowing a bounded retry. |
| `--require-zero-progress` | `QBTW_REQUIRE_ZERO_PROGRESS` | `true` | Require API progress to be exactly zero before action. |
| `--require-zero-downloaded` | `QBTW_REQUIRE_ZERO_DOWNLOADED` | `true` | Require downloaded payload bytes to be zero before action. |
| `--include-categories` | `QBTW_INCLUDE_CATEGORIES` | empty/all | Comma-separated exact category allowlist. |
| `--exclude-categories` | `QBTW_EXCLUDE_CATEGORIES` | empty | Comma-separated exact category denylist; exclusion wins. |
| `--exclude-tags` | `QBTW_EXCLUDE_TAGS` | `keep,qbt-watchdog-ignore` | Comma-separated exact tags that protect a torrent. |
| `--state-file` | `QBTW_STATE_FILE` | `/data/state.json` | Persistent state location. |
| `--history-limit` | `QBTW_HISTORY_LIMIT` | `100` | Maximum retained audit events. |
| `--listen` | `QBTW_LISTEN` | `:8080` | HTTP listen address. |
| `--ui-refresh-interval` | `QBTW_UI_REFRESH_INTERVAL` | `5s` | Browser status refresh interval. |
| `--readiness-max-age` | `QBTW_READINESS_MAX_AGE` | `2m` | Maximum age of last successful poll before readiness fails. |
| `--web-username` | `QBTW_WEB_USERNAME` | empty | Optional Basic Auth username for `/` and `/api/v1/status`. |
| `--web-password` | `QBTW_WEB_PASSWORD` | empty | Optional Basic Auth password; password file is preferred. |
| `--web-password-file` | `QBTW_WEB_PASSWORD_FILE` | empty | Optional mounted Basic Auth password file. |
| `--metrics-public` | `QBTW_METRICS_PUBLIC` | `true` | Exempt `/metrics` from optional Basic Auth. |
| `--tls-ca-file` | `QBTW_TLS_CA_FILE` | empty | Optional custom CA bundle for qBittorrent HTTPS. |
| `--tls-insecure-skip-verify` | `QBTW_TLS_INSECURE_SKIP_VERIFY` | `false` | Development escape hatch; warn loudly when enabled. |
| `--log-level` | `QBTW_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `--log-format` | `QBTW_LOG_FORMAT` | `json` | `json`, `text`, or `console`, using `log/slog`. `console` renders one human-readable line per record for terminal output. |
| `--log-color` | `QBTW_LOG_COLOR` | `auto` | `auto`, `always`, or `never`. `auto` colors only when stderr is a real terminal and `NO_COLOR` is unset. Affects only the `console` format. |
| `--once` | `QBTW_ONCE` | `false` | Execute one poll/action cycle, persist state, print a summary, and exit; do not start the web server. |

Also support:

```text
qbt-watchdog --help
qbt-watchdog version
qbt-watchdog healthcheck --url http://127.0.0.1:8080/healthz
```

Validation requirements:

- Reject non-positive poll intervals, metadata timeouts, HTTP timeouts, readiness ages, and history limits.
- Reject malformed URLs and unsupported URL schemes.
- Reject conflicting direct-password and password-file settings for both qBittorrent and web credentials.
- Reject incomplete Basic Auth credential pairs.
- Reject a negative deletion cap.
- Warn if `delete-files=true`, dry-run is disabled, TLS verification is disabled, or the web UI is exposed without auth on a non-loopback address.
- Trim comma-separated category/tag values, discard empty values, and compare exact strings case-sensitively to match qBittorrent semantics.

Do not display secret configuration values in `--help`, startup logs, UI, JSON status, or metrics.

## Web UI and HTTP API

Use Go's standard `html/template`, embedded CSS, and minimal embedded vanilla JavaScript. Do not introduce Node, npm, a frontend framework, external fonts, analytics, CDNs, or remotely loaded assets. The binary and container must work without internet access.

### `GET /`

Provide a clean, responsive page with:

- Service version and build information.
- qBittorrent reachability, application version, and Web API version.
- Dry-run/active mode shown prominently.
- Last successful poll, most recent poll error, next expected poll, and persistence health.
- Summary cards: total torrents, currently in `metaDL`, protected/excluded, overdue, would-delete, delete-requested, and deletions/would-deletions since startup.
- A torrent table, sorted with overdue/actionable rows first, containing:
  - name;
  - short hash;
  - qBittorrent state;
  - progress;
  - download speed;
  - connected seeds/leechers;
  - category and tags;
  - added time;
  - time continuously observed in `metaDL`;
  - time remaining or overdue duration;
  - watchdog decision such as `not applicable`, `tracking`, `protected`, `would delete`, or `delete requested`.
- A bounded recent-action history showing time, action, torrent name/short hash, dry-run status, and outcome/error.

The UI should auto-refresh data from the JSON status endpoint without reloading the page, preserve table readability on a phone, use semantic HTML, and support the browser's light/dark preference. Display times in the browser's local timezone while preserving RFC 3339 UTC timestamps in JSON.

Do not expose magnet URIs, full hashes, filesystem paths, qBittorrent credentials, session cookies, or stack traces.

### `GET /api/v1/status`

Return the complete UI status as versioned JSON. Include a top-level schema version. Use RFC 3339 UTC timestamps and numeric durations in seconds. Return the latest immutable in-memory snapshot without initiating a qBittorrent request.

Set `Cache-Control: no-store` on the UI and status responses.

### `GET /healthz`

Liveness endpoint. Return HTTP 200 with a small JSON body whenever the process and HTTP server are running. It must not depend on qBittorrent being reachable.

### `GET /readyz`

Return HTTP 200 only when:

- startup/configuration completed;
- at least one qBittorrent poll succeeded;
- the last successful poll is not older than `readiness-max-age`; and
- persistence has no unresolved write error.

Otherwise return HTTP 503 with a concise JSON reason.

### `GET /metrics`

Expose Prometheus metrics. Use `prometheus/client_golang`. Avoid torrent name, hash, category, tag, or error text labels.

At minimum provide:

```text
qbt_watchdog_build_info{version,revision,go_version} 1
qbt_watchdog_qbt_up
qbt_watchdog_last_successful_poll_timestamp_seconds
qbt_watchdog_poll_duration_seconds
qbt_watchdog_poll_errors_total
qbt_watchdog_torrents_total
qbt_watchdog_metadata_tracked
qbt_watchdog_metadata_overdue
qbt_watchdog_actions_total{action,outcome,dry_run}
qbt_watchdog_state_write_errors_total
```

Keep label values bounded enums. Register metrics explicitly rather than through global package state so tests can create isolated registries.

### Settings editor and manual actions

The web surface is not read-only: two authenticated, CSRF-protected mutation endpoints sit behind the same Basic Auth as the dashboard.

#### `GET /api/v1/config` and `POST /api/v1/config`

A settings editor for the configuration document. `GET` returns the raw file text plus an opaque `stamp` (for optimistic concurrency control). `POST` accepts the edited document (JSON or `application/x-www-form-urlencoded`) and, on save, preserves the operator's YAML comments and formatting. Concurrent edits are detected via the stamp: a `409` is returned on conflict rather than silently overwriting a newer edit. A save that yields an invalid configuration is rejected (`422`) and the previously running configuration stays in force; the resulting reload health is reported in the response. Both routes require authentication, and `POST` additionally requires the same-origin CSRF check.

#### `POST /api/v1/actions`

Queues a manual action for one torrent: `run_now` (accelerate a still-tracking torrent through its threshold) or `explicit` removal. The client names the torrent only by `short_hash`; the full hash is resolved server-side from the latest snapshot and never accepted from the request. The endpoint does not delete anything itself — it records the intent and returns `202` with the current decision; the next `Poll` executes the action through the existing safety pipeline (exclusions, dry-run, cap, attempts, continuity). Requires authentication plus the same-origin CSRF check.

#### HTMX server-rendered UI

The dashboard is rendered server-side with Go templates and a vendored copy of htmx (no CDN, no frontend build step); partials refresh individual sections without reloading the page, and light/dark themes follow the browser preference.

### HTTP hardening

- Set `Content-Type` explicitly.
- Add `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, a restrictive `Content-Security-Policy`, and `X-Frame-Options: DENY`.
- Configure server read-header, read, write, and idle timeouts.
- Bound request header sizes.
- Use constant-time comparison for optional Basic Auth credentials.
- Protect `/`, `/api/v1/status`, `/api/v1/config`, and `/api/v1/actions` when web credentials are configured. Health endpoints remain unauthenticated. `/metrics` follows `metrics-public`.
- Manual actions (`POST /api/v1/actions`) and settings edits (`POST /api/v1/config`) are the only mutation surfaces, and both require authentication plus a same-origin CSRF check; they never accept a full torrent hash.
- Escape all torrent-controlled text and never construct HTML with `innerHTML` from API values.

## Persistence format

Use a versioned JSON document, not a database. Keep it human-inspectable and small. An illustrative shape is:

```json
{
  "schema_version": 1,
  "tracked": {
    "0123456789abcdef": {
      "first_seen_meta": "2026-09-12T18:30:00Z",
      "last_seen_meta": "2026-09-12T18:34:30Z",
      "dry_run_notified": false,
      "delete_requested_at": null
    }
  },
  "counters": {
    "deletions": 0,
    "would_deletions": 1
  },
  "history": []
}
```

Do not store magnet URIs, credentials, cookies, save paths, or other unnecessary qBittorrent data. It is acceptable to store a torrent name in bounded audit history, but document that choice.

## Logging

Use `log/slog` with JSON output by default. Emit structured events for:

- startup and effective non-secret configuration;
- qBittorrent authentication and detected versions;
- poll success/failure and backoff;
- tracking start/reset/stop;
- exclusions and safety-guard decisions at debug level;
- `would_delete`, `delete_requested`, `delete_succeeded`, `delete_skipped`, and `delete_failed`;
- persistence load/write errors;
- graceful shutdown.

Use stable event names and fields. Include only a short hash in logs. Do not log magnet URIs, cookies, passwords, direct password-file contents, or full API bodies.

## Internal design

Keep boundaries testable. A suggested layout is:

```text
cmd/qbt-watchdog/main.go
internal/config/
internal/qbt/
internal/watchdog/
internal/store/
internal/web/
internal/observability/
web/templates/
web/static/
```

This layout is guidance, not a reason to add boilerplate.

Recommended interfaces:

- `qbt.Client`: authenticate, get versions, list torrents, get torrent by hash, delete torrent.
- `store.Store`: load and atomically save versioned state.
- `watchdog.Clock`: injectable time source for deterministic tests.
- `watchdog.Service`: own the non-overlapping poll loop, state transitions, confirmation, and actions.
- `web.Server`: serve immutable status snapshots supplied by the service.

Use a mutex or `atomic.Value` to publish immutable snapshots to HTTP handlers. Do not expose mutable maps or hold locks while performing network or filesystem I/O.

Prefer the standard library except where a dependency clearly earns its place. The expected external runtime-code dependency is `prometheus/client_golang`; avoid a web framework, ORM, configuration framework, and JavaScript build chain.

## Container requirements

Provide a production-quality `Dockerfile` with at least two stages.

Builder stage:

- Pin the exact latest stable Go patch release.
- Download modules in a cache-friendly layer before copying all source.
- Use BuildKit cache mounts where appropriate.
- Build with `CGO_ENABLED=0`, `-trimpath`, and reproducible linker flags.
- Inject version, revision, and build date through build arguments/linker variables.
- Support at least `linux/amd64` and `linux/arm64` through `TARGETOS`/`TARGETARCH`.

Runtime stage:

- Use `scratch` or a pinned minimal distroless/static image.
- Include CA certificates if HTTPS qBittorrent URLs are supported.
- Run as an unprivileged numeric UID/GID such as `65532:65532`.
- Contain only the binary and required certificate/timezone data.
- Expose port `8080`.
- Declare `/data` as the expected writable persistence location, but do not bake mutable state into the image.
- Use the binary as `ENTRYPOINT`.
- Add a Docker `HEALTHCHECK` using the binary's `healthcheck` subcommand; do not require a shell, curl, or wget in the final image.
- Work with a read-only root filesystem and all Linux capabilities dropped.

Also provide:

- `.dockerignore`;
- a `compose.yaml` example;
- OCI image labels;
- documented `docker buildx build` examples for amd64 and arm64.

The Compose example must:

- connect to qBittorrent by Docker service name;
- persist `/data`;
- mount the qBittorrent password as a read-only secret file rather than placing it in the file directly;
- publish the UI on loopback by default, for example `127.0.0.1:8090:8080`;
- set `read_only: true`, `cap_drop: [ALL]`, and `no-new-privileges`;
- use `restart: unless-stopped`;
- start in dry-run mode;
- explain ownership/permissions required for the `/data` volume by the container UID.

## Documentation and developer experience

Provide a concise but complete `README.md` containing:

- what the service does and deliberately does not do;
- a prominent dry-run-first quick start;
- qBittorrent WebUI/API prerequisites;
- Docker and Compose examples;
- bare-binary usage;
- the full configuration table;
- state persistence behavior;
- UI, health, readiness, JSON, and metrics endpoints;
- how to protect torrents with category/tag exclusions;
- how to safely enable deletion;
- the danger of `delete-files=true`;
- troubleshooting for authentication, permissions, TLS, and qBittorrent downtime;
- an example Prometheus scrape configuration;
- build and test commands;
- compatibility assumptions.

Provide a small `Makefile` with self-explanatory targets such as `fmt`, `test`, `race`, `vet`, `build`, and `docker-build`. It must merely wrap normal Go/Docker commands and must not be required to build the application.

Do not invent a license or copyright owner. If no license is supplied by the repository owner, mention that explicitly in the handoff rather than adding one.

## Tests

Write deterministic tests using `httptest`, temporary directories, and an injectable clock. Tests must not require a real qBittorrent instance or internet access.

At minimum cover:

1. Successful and failed qBittorrent authentication, cookie reuse, and one re-authentication attempt.
2. Torrent list decoding with unknown fields.
3. A new `metaDL` torrent beginning tracking.
4. Tracking continuity across normal polls.
5. Timer reset when the torrent leaves and later re-enters `metaDL`.
6. Timer reset after `max-observation-gap`.
7. No action before timeout.
8. Dry-run produces a single `would_delete` event per episode and no delete request.
9. Active mode performs the fresh confirmation request and then requests deletion with `deleteFiles=false`.
10. Confirmation prevents deletion when state changes at the boundary.
11. Zero-progress and zero-downloaded safety guards.
12. Category/tag include/exclude precedence.
13. Deterministic oldest-first ordering and deletion cap.
14. Poll/API errors never trigger deletion.
15. State survives a short restart.
16. Missing, corrupt, and atomically rewritten state files.
17. UI/status rendering escapes hostile torrent names.
18. Health and readiness transition behavior.
19. Basic Auth protection and constant-time credential validation at the handler boundary.
20. Metrics have expected values and contain no high-cardinality torrent labels.
21. Graceful cancellation without goroutine leaks or overlapping polls.
22. A delete request remains pending until disappearance is observed, and a still-present torrent is retried only after `delete-confirmation-timeout`.

Required verification commands:

```bash
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/qbt-watchdog
docker build -t qbt-watchdog:test .
docker run --rm qbt-watchdog:test version
```

Run all applicable commands and report their exact outcome in the final handoff. If Docker is unavailable, say so clearly and still complete all Go-level verification.

## Acceptance criteria

The implementation is complete only when all of the following are true:

- A healthy torrent that leaves `metaDL` before the timeout is never deleted.
- A torrent continuously observed in `metaDL` beyond the timeout is shown as eligible in dry-run mode.
- With dry-run disabled, an eligible torrent is re-fetched, revalidated, and submitted to the correct qBittorrent deletion endpoint; success is confirmed only after a later poll observes that it disappeared.
- `deleteFiles` is false by default and verified by tests.
- Long polling/API gaps do not count as proven continuous `metaDL` time.
- Watchdog state survives normal container restarts through `/data/state.json`.
- qBittorrent outages degrade status without crashing or deleting anything.
- The UI shows all torrents and clearly explains each watchdog decision.
- The UI shows all torrents, safely escapes torrent-controlled content, and exposes settings editing and manual actions only behind authentication and CSRF protection.
- The service exposes functional liveness, readiness, versioned status JSON, and bounded-cardinality Prometheus metrics.
- Credentials and sensitive qBittorrent fields never appear in logs, state, UI, status JSON, or metrics.
- The final image is multi-stage, static, non-root, has no shell/runtime package manager, and starts successfully.
- The repository passes formatting, vetting, unit tests, race tests, and a container smoke test where tooling is available.
- The README lets a new user deploy safely without reading the source.

## Explicitly out of scope

- Diagnosing why metadata retrieval fails.
- Replacing qBittorrent or implementing BitTorrent behavior.
- Managing normal stalled downloads or completed torrents.
- A general torrent-management UI.
- Sonarr/Radarr/Prowlarr blocklisting or failure notification in the first version.
- Multiple qBittorrent instances in one process.
- Kubernetes manifests, Helm charts, or cloud deployment.
- SQLite, bbolt, Redis, or another database for this small state set.

Keep extension points clean enough that an external notification or `*arr` integration could be added later, but do not implement it now.

## Final handoff expected from the implementation agent

Return:

1. A short architecture summary.
2. A tree of important files.
3. Any material design decisions or deviations from this specification.
4. Exact verification commands and results.
5. The safest first-run command in dry-run mode.
6. The deliberate step required to enable deletion.
7. Any unresolved risks or follow-up work.
