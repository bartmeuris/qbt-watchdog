# qbt-watchdog

A small, stateful daemon that watches qBittorrent for torrents stuck in **Downloading metadata** (`metaDL`), **stalled downloads** (`stalledDL`), completed-looking torrents with no payload, or explicitly tagged stopped Arr-managed torrents across independent policies. It measures continuously observed time, reports overdue torrents in a read-only dashboard, and can remove them after a configurable timeout.

**Start in dry-run mode.** Dry-run is enabled by default and never calls the delete endpoint or writes qBittorrent watchdog tags. Enabling deletion requires the deliberate setting `dry_run: false` in the configuration file. Removal preserves payload files by default; `action: delete_file` can permanently delete downloaded data through qBittorrent and is irreversible.

This is not a general torrent manager. It does not diagnose metadata failures, manage normal completed downloads with payload, implement BitTorrent, integrate with Prowlarr, or manage multiple qBittorrent instances in one process. Optional Sonarr/Radarr recovery can blocklist/search after watchdog-controlled qBittorrent cleanup; it never manages import libraries and never deletes imported episode/movie files.

## Prerequisites and compatibility

- Enable qBittorrent's WebUI and make it reachable from the watchdog. Use the WebUI base URL, not a URL ending in `/api/v2`. HTTP and HTTPS are supported, including a reverse-proxy base path.
- Target: qBittorrent WebUI API v2 as documented for [qBittorrent 5.x](<https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-5.0)>). Recent 4.x/5.x installations are expected to work if they expose the required endpoints and fields. API-key authentication requires qBittorrent >= 5.2.0 (WebUI API >= 2.14.1). **Integration against a real qBittorrent instance has not been verified.** Validate your installation in dry-run before enabling deletion.
- Required endpoints under `/api/v2/`: `auth/login` (SID mode only), `app/version`, `app/webapiVersion`, `torrents/info` (all torrents and a single `hashes` value), `torrents/delete`, and `torrents/addTags` / `torrents/removeTags` when `tag_sync.enabled` is true or while a previously claimed prefix is being swept with sync disabled and `dry_run: false`.
- Three authentication modes: API key (`qbt_api_key` or `qbt_api_key_file`), username/password (SID cookie), or bypass (all credentials empty). API-key and username/password modes are mutually exclusive.
- Container deployment requires Docker with BuildKit; the supplied example also requires Docker Compose and an existing network shared with qBittorrent.
- Source builds use Go **1.27.1**, confirmed against the [official downloads page](https://go.dev/dl/) for this implementation. The builder image pins `golang:1.27.1-alpine`; that builder has successfully built. Runtime containers need neither Go nor a shell.

## Dry-run-first quick start

### 1. Connect to your existing qBittorrent network

The supplied [`compose.yaml`](compose.yaml) deploys only the watchdog, not qBittorrent. It joins an **external, already existing** Docker network named `qbt` by default. Set the `networks` entry in `compose.yaml` to your existing network's actual name if different.

The qBittorrent container must be on that network and resolve there as **`qbittorrent`**, either through its Compose service name or a network alias. The example connects to `http://qbittorrent:8080`: change `qbt_url` in `config/config.yaml` if your internal WebUI port, protocol, or alias differs. Use the container's WebUI port, not its host-published port. `localhost` inside the watchdog is the watchdog itself.

### 2. Create the configuration file

Create `config/config.yaml`. The supplied `config.example.yaml` is the complete reference with documented defaults and inert placeholders; `qbt_url` is required and has no built-in default. At minimum:

```yaml
qbt_url: 'http://qbittorrent:8080'

# Choose one authentication mode:

# Option A — API key (qBittorrent >= 5.2.0):
qbt_api_key_file: '/run/secrets/qbt_api_key'
# Option B — Username/password:
# qbt_username: "admin"
# qbt_password_file: "/run/secrets/qbt_password"

# Option C — Bypass (leave all credentials empty):
```

API-key and username/password modes are mutually exclusive. Password and password-file are mutually exclusive for each credential pair.

### 3. Create a secret

For API-key mode, create a file containing your qBittorrent API key:

```bash
mkdir -p secrets
chmod 750 secrets
umask 077
read -r -s -p 'qBittorrent API key: ' QBT_SECRET
printf '\n'
printf '%s' "$QBT_SECRET" > secrets/qbt_api_key
unset QBT_SECRET
```

For username/password mode, create a file containing your WebUI password instead.

The mounted file must be readable by container UID/GID **65532:65532**. On a native Linux Docker host, you can grant that group access without making the secret world-readable:

```sh
sudo chgrp 65532 secrets secrets/qbt_api_key
chmod 750 secrets
chmod 640 secrets/qbt_api_key
```

Local Compose secrets are file mounts, not an encrypted secret store. File ownership behavior varies with Docker Desktop, rootless Docker, and user-namespace mappings; arrange equivalent readable permissions for the mapped container identity. UID/GID 65532 must be able to traverse every parent directory and read the mounted secret files. Do not solve permission errors by making secrets public. Secret files are read at startup and re-read when their files change, limited to 64 KiB, and have trailing CR/LF removed.

Mount configuration as a directory rather than a single file. The watchdog watches the directory containing the resolved config file so it can notice atomic replacements of `config.yaml` and `.env` as well as secret/ConfigMap symlink swaps. Real `.env` files and `config/`/`secrets/` directories are ignored by Git and excluded from Docker build context by this repository.

### 4. Start and inspect

```sh
docker compose up -d --build
docker compose logs -f qbt-watchdog
```

Open **http://127.0.0.1:8090**. Confirm the dashboard says dry-run, qBittorrent versions are detected, polling succeeds, and persistence is healthy. By default, a torrent must be continuously observed matching a policy for 30 minutes before it is marked `would delete`.

The example binds the host UI port to loopback, runs with a read-only root filesystem and all capabilities dropped, and persists `/data` in a named volume. The image prepares `/data` with owner `65532:65532` and mode `0700`; a new normally initialized named volume inherits this. Existing volumes and bind mounts may need ownership correction. Do not use `docker compose down -v` if you intend to retain tracking and action history.

### 5. Enable deletion only after reviewing dry-run

1. Review overdue torrents, exclusions, and safety guards in the dashboard.
2. Keep a persistent state volume and confirm readiness is healthy.
3. Edit `config/config.yaml`: set `dry_run: false` and explicitly set each policy's `action` (e.g. `action: "delete"` or `action: "delete_file"`). Destructive actions require both `dry_run: false` and a selected action; the default `warn` action never removes anything. Remember that `delete_file` removes downloaded data irreversibly.
4. The file watcher detects the change and applies it automatically; confirm the dashboard now reports active mode.

**Do not enable `action: delete_file` unless you intentionally want qBittorrent to remove payload files, including any shared data affected by its deletion behavior.** The watchdog needs no access to the download filesystem itself.

Switching from dry-run to active mode does not inherit previously accumulated elapsed time; the full threshold must be observed again under the new configuration. Safety-affecting changes -- switching modes, adjusting thresholds, modifying any policy setting, or changing TLS CA/insecure verification settings -- reset policy timers as applicable, requiring a fresh continuous observation period before any action is taken. Pending deletion requests and the finite retry budget are preserved across most reloads, so a request already accepted is not cancelled by an ordinary configuration change. Exception: changing the qBittorrent endpoint fences old endpoint-specific state by clearing per-torrent clocks, pending request markers, retry budgets, and seeder observations. To stop all actions immediately, set every policy's action to `warn` or set `max_actions_per_poll: 0`.

## Docker without Compose

After preparing the shared network and secret file as above:

```sh
docker build -t qbt-watchdog:local .
docker volume create qbt-watchdog-data
docker run -d --name qbt-watchdog \
  --restart unless-stopped \
  --network "${QBT_NETWORK:-qbt}" \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -p 127.0.0.1:8090:8080 \
  --mount type=volume,src=qbt-watchdog-data,dst=/data \
  --mount "type=bind,src=$(pwd)/config,dst=/config,readonly" \
  --mount "type=bind,src=$(pwd)/secrets,dst=/run/secrets,readonly" \
  qbt-watchdog:local --config /config/config.yaml
```

For a Linux bind-mounted state directory, prepare it before starting the container:

```sh
mkdir -p data
sudo chown 65532:65532 data
sudo chmod 700 data
```

Then replace the volume mount with `--mount "type=bind,src=$(pwd)/data,dst=/data"`. If restoring state, its files must also be accessible to UID 65532; stop the service before changing ownership or restoring files. Use mapped ownership where user namespaces apply. Keep `/data` writable even when the root filesystem is read-only.

## Bare binary

Run as an unprivileged account with a private, writable state directory. Unlike the container, a local process uses your account's UID; its secret file should be readable only by that account. Prepare a secret file with the interactive prompt above and appropriate local ownership, then:

```sh
CGO_ENABLED=0 go build -trimpath -o qbt-watchdog ./cmd/qbt-watchdog
mkdir -p data
chmod 700 data
```

Set the state file and listen address in `config.yaml` (or a copy) to match the defaults the healthcheck expects:

```yaml
state_file: './data/state.json'
listen: '127.0.0.1:8080'
```

Prepare the state directory before starting the daemon:

```sh
mkdir -p data
chmod 700 data
```

Then start the daemon:

```sh
./qbt-watchdog --config config.yaml
```

The only CLI flags are `--config FILE` (default: `config.yaml`, or set `QBTW_CONFIG`) and `--once`. All settings live in the configuration file; there are no per-setting flags or environment variables.

`SIGINT` and `SIGTERM` stop polling and shut down the HTTP server gracefully. Polls never overlap; the first poll is immediate and subsequent intervals start after the previous poll completes.

```sh
./qbt-watchdog --help
./qbt-watchdog version
./qbt-watchdog healthcheck --url http://127.0.0.1:8080/healthz
```

Add `--once` for one poll/action cycle, state persistence, and a JSON summary on stdout, without a web server. A fresh empty state normally only starts tracking; `--once` does not wait for the metadata timeout. Repeated invocations must be close enough to preserve observation continuity. `--once` can persist recovery work created by the poll, but it does **not** run the background Sonarr/Radarr recovery workers; continuous daemon mode is required to process those jobs. It exits `1` on poll or persistence failure and `2` for configuration errors.

## Configuration

All settings live in a single YAML or TOML file. The CLI is intentionally minimal:

```
Usage: qbt-watchdog [--config FILE] [--once]
       qbt-watchdog version
       qbt-watchdog healthcheck [--url URL]
```

| Flag / Variable | Description                                                                   |
| --------------- | ----------------------------------------------------------------------------- |
| `--config FILE` | Configuration file path (YAML or TOML). Default: `config.yaml`.               |
| `QBTW_CONFIG`   | Environment variable supplying the config path when `--config` is not given.  |
| `--once`        | Run one poll/action cycle, persist, print JSON summary, exit. No HTTP server. |

The `healthcheck` subcommand has a separate `--url` flag, defaulting to `http://127.0.0.1:8080/healthz`, with no environment equivalent. It accepts only HTTP 200 as healthy. `version` prints build information without requiring qBittorrent configuration.

See [`config.example.yaml`](config.example.yaml) for the complete field reference with documented defaults and inert placeholders. TOML is accepted too (`--config /etc/qbt-watchdog/config.toml`); the key names and values are identical. Unknown keys and values of the wrong type are rejected: the process refuses to start rather than silently ignore a typo.

### Environment substitution and `.env`

Configuration is not a shell script. After YAML/TOML is parsed, only string values are scanned for explicit `${NAME}` references; `$$` becomes a literal `$`. Bare `$HOME`, `$NAME`, backticks and `$(...)` are literal. Substituted values are not recursively expanded, so a value containing `${OTHER}` or `$$` stays exactly that text after insertion. Keys, numbers, booleans and list structure are never rewritten.

An optional `.env` file is loaded from the directory containing the resolved config file (for example `/config/.env` when running `--config /config/config.yaml`), not necessarily from the current working directory. Process environment wins over `.env`, including empty process values, so inherited variables override `.env` edits until restart. On reload, missing variables, malformed references, invalid `.env` syntax, and invalid candidates retain the last working configuration. During initial startup, the same errors are fatal because there is no last working configuration yet.

Supported `.env` syntax is deliberately small: one `NAME=value` assignment per line; optional `export`; spaces around `=`; blank lines, comments, CRLF line endings, and single/double quoted values. Duplicate names use the last value. Single quotes are fully literal. Double quotes decode only `\n`, `\r`, `\t`, `\\`, `\"` and `\$`; all other escapes, multiline quotes and trailing tokens are errors. In unquoted values, `#` begins a comment only at the start of the value or after horizontal whitespace. `.env` does not expand `$NAME`, `${NAME}`, `$$`, backticks or command substitutions.

The watcher reloads both `config.yaml` and adjacent `.env` changes automatically, including atomic replacement. Referenced secret files and TLS CA bundles are watched through their parent directories so direct writes and atomic replacements trigger a reload without idle polling. Mount the containing config directory into containers instead of bind-mounting only the file, and ensure UID/GID 65532 can traverse the directory and read any referenced secret files. Real `.env` files are Git-ignored and Docker-ignored. `.env.example` is committed as a placeholder template but excluded from Docker build context by `.dockerignore`; it still must never contain real secrets.

### Reloading

The configuration file is re-read automatically whenever it or a referenced dependency changes; there is no need to restart or send a signal. Atomic replacements (write-to-temp then rename) and Kubernetes ConfigMap/Secret symlink swaps are detected, and rapid bursts of writes are debounced into a single reload.

If a reload is invalid, the previously running configuration is kept in full, the problem is logged, and the error is surfaced in the status payload and on the readiness endpoint until a good file is written.

**Restart required** -- changing either of these is rejected with a warning and the entire candidate is discarded:

- `listen` (the socket is already bound)
- `state_file` (the episode store is already open)

**Applied live** -- everything else, including the qBittorrent endpoint, credentials (including API key) and TLS settings (the HTTP client is rebuilt), all policies, every interval, the category/tag exclusions, `tag_sync`, `log_level`, `log_format`, the readiness and UI settings, and Sonarr/Radarr integrations. Password files, API key files, integration key files, `.env`, and the CA bundle are watched and re-read when they change, so rotating a mounted secret needs no restart.

**Safety-affecting reloads reset timers.** Changing anything that could change the qBittorrent cleanup outcome -- a policy, `dry_run`, an interval, the exclusions, the action cap, qBittorrent credentials (including API key), or TLS CA/insecure verification settings -- resets policy timers as applicable so no torrent inherits unsafe elapsed time across a configuration change. Sonarr/Radarr-only changes do not reset qBittorrent policy clocks. The finite attempt budget and a delete request still awaiting confirmation are deliberately preserved across ordinary safety reloads; a reload is not a licence to retry something already in flight. Exception: changing the qBittorrent endpoint clears per-torrent clocks, pending request markers, retry budgets, and seeder observations, because old endpoint-scoped state cannot be trusted for a different WebUI.

### Authentication

Three modes, enforced as mutually exclusive at startup:

**API key** (qBittorrent >= 5.2.0): Set `qbt_api_key` or `qbt_api_key_file`. The key is sent as a `Bearer` token in the `Authorization` header on every request. No cookie jar is created and no `/api/v2/auth/login` endpoint is called. qBittorrent must have API-key authentication enabled in its Web UI settings. On HTTP 401/403, the error is reported immediately without retry.

**Username + password**: Set both `qbt_username` and `qbt_password` (or `qbt_password_file`). The client logs in via `/api/v2/auth/login`, stores the SID cookie, and reauthenticates once on session expiry. Mutations (POST/DELETE) are never retried after reauthentication to avoid double actions.

**Bypass**: Leave all credentials empty. The client omits explicit login and relies on qBittorrent's local-network authentication bypass. Do not broaden that policy just to deploy this service.

Credentials are never logged and never appear in the status payload, the metrics, the UI, or any error message.

### Sonarr/Radarr recovery integrations

Sonarr and Radarr are optional and disabled by default. The schema is:

```yaml
integrations:
  sonarr:
    enabled: false
    url: "http://sonarr:8989"
    api_key: ""
    api_key_file: ""
    categories: []
    mode: blocklist_and_search
    timeout: "10s"
  radarr:
    enabled: false
    url: "http://radarr:7878"
    api_key: ""
    api_key_file: ""
    categories: []
    mode: blocklist_and_search
    timeout: "10s"
```

An enabled integration requires a valid HTTP(S) `url` and exactly one credential source: `api_key` or `api_key_file`. Disabled stubs may leave credentials empty, but malformed URLs, bad modes, invalid timeouts and conflicting credential sources are still rejected. `api_key_file` is re-read when its file changes and has trailing CR/LF stripped. Empty `categories` means the integration handles all qBittorrent categories; non-empty lists are exact, case-sensitive matches.

Modes:

- `blocklist_and_search` removes the matching Arr queue item with `blocklist=true`, `removeFromClient=false`, `skipRedownload=true` and `changeCategory=false`, then sends a targeted `EpisodeSearch` (Sonarr) or `MoviesSearch` (Radarr).
- `search_only` sends only the targeted search, without blocklisting or removing the Arr queue item.

Explicit opt-in example using `.env` placeholders:

```yaml
integrations:
  sonarr:
    enabled: true
    url: "http://sonarr:8989"
    api_key: "${SONARR_API_KEY}"
    api_key_file: ""
    categories: ["tv"]
    mode: blocklist_and_search
    timeout: "10s"
```

Recovery requires all of these: an enabled integration matching the torrent category, `dry_run: false`, a destructive effective policy (`delete` or `delete_file`), and `max_actions_per_poll` greater than zero. Warn-only and dry-run configurations never mutate Sonarr/Radarr. qBittorrent cleanup is not blocked by Arr outages: the watchdog captures recovery identity before deletion, but it makes no Arr network calls between the final qBittorrent safety read and the qBittorrent delete request.

Each policy may override recovery with `arr_mode: inherit` (default), `none`, `blocklist_and_search`, or `search_only`. Service-level `integrations.<service>.mode` accepts only `blocklist_and_search` or `search_only`.

Recovery identity is the exact qBittorrent torrent hash / Arr `downloadId`, never a torrent or release name. Sonarr season packs are aggregated into one recovery job containing all matching queue rows and episode IDs. The recovery workflow starts only after qBittorrent accepts deletion **and** a later successful full qBittorrent poll confirms that the hash disappeared. The Arr call never deletes imported media files; even blocklisting uses `removeFromClient=false`, because qBittorrent cleanup already happened under the watchdog's policy.

Safety details and limits:

- Queue snapshots are refreshed every 30 seconds and considered fresh for 2 minutes. Up to 500 recovery jobs are persisted, each with at most 200 queue/history/media IDs, a 24-hour lifetime, and five attempts per stage.
- A vanished queue item in `blocklist_and_search` mode produces an incomplete/failed recovery outcome (`queue_vanished`); it is not silently downgraded to search-only and it does not write Sonarr/Radarr history/failed state.
- Ambiguous mutation outcomes are not blindly replayed. Jobs enter an uncertain state after outcomes such as mutation timeouts or restart during an intent stage.
- Search command completion only means the Arr command finished; it does **not** prove a replacement was grabbed. Replacement/import checks conservatively ignore the captured pack itself.
- The watchdog makes no exactly-once promise against Sonarr/Radarr's own recovery/import behavior. Arr may import, remove queue rows, or start searches independently.
- Endpoint changes fence old recovery jobs. Rotating credentials for the same endpoint is adopted live and keeps endpoint-scoped jobs valid.

### Protecting torrents

Add the qBittorrent tag **`keep`** or **`qbt-watchdog-ignore`** to protect a torrent with the defaults. Lists are comma-separated, trimmed, and compared case-sensitively as exact strings; there are no wildcards. Empty elements are ignored. Configuring `exclude_tags` replaces the defaults, so include the default tags if you want to retain them. Tags beginning with the reserved watchdog prefix (default `qbtw-`) are ignored for protection and policy matching and cannot be configured as `exclude_tags` or `match_tags`; case-insensitive prefix near-misses are accepted with a warning.

For example, restrict actions to category `temporary`, but always protect category `archive` and tag `manual`:

```yaml
include_categories: ['temporary']
exclude_categories: ['archive']
exclude_tags: ['keep', 'qbt-watchdog-ignore', 'manual']
```

Category exclusion and excluded tags win over category inclusion. Protected torrents still accumulate observed time, so removing protection can make an already overdue torrent immediately actionable.

## Policies

Policies are mutually exclusive. Each has an `action`, a `threshold`, and an optional `arr_mode`. `stopped_arr_managed` also has mandatory `match_tags`. A torrent must match the same policy continuously for the whole threshold before the action is taken; any change of state, progress, seeder observation, tag match, or payload evidence starts the clock again.

| Policy                 | Condition                                                      | Description                                                                                                                         |
| ---------------------- | -------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `metadata`             | `state == "metaDL"`                                            | Torrent stuck fetching metadata.                                                                                                    |
| `stalled_no_seeders`   | `state == "stalledDL"`, zero progress, seeders never observed  | Stalled downloads most likely genuinely dead.                                                                                       |
| `stalled_seeders_seen` | `state == "stalledDL"`, zero progress, seeders observed before | Stalled downloads where the swarm may simply be idle.                                                                               |
| `stalled_partial`      | `state == "stalledDL"`, `0 < progress < 1`                     | Stalled downloads that made some progress. This is the only policy that acts despite downloaded bytes; treat with the most caution. |
| `completed_no_data`    | upload/seeding/stopped-UP state, `size == 0`, `total_size > 0`, `downloaded == 0`, `amount_left == 0` | Completed-looking torrents with metadata but no payload. Default threshold: `2m`. Takes precedence over `stopped_arr_managed`. |
| `stopped_arr_managed`  | `stoppedUP`, `pausedUP`, `stoppedDL`, or `pausedDL` with any configured `match_tags` | Operator-stopped Arr-managed torrents. Default action/threshold: `warn` after `10m`. Can delete/blocklist/search if you configure destructive action, Arr recovery, and `dry_run: false`. |

Actions:

- `warn` -- report only, never touch the torrent (default).
- `delete` -- remove the torrent, keep the downloaded files.
- `delete_file` -- remove the torrent and its downloaded files; irreversible.

**`dry_run` overrides every action to `warn`.** A destructive action requires both the configured action and `dry_run: false`. Enabling `delete_file` requires additionally setting `action: delete_file` on the relevant policy.

### qBittorrent tag sync

`tag_sync` can write status tags back to qBittorrent under a reserved prefix:

```yaml
tag_sync:
  enabled: false
  prefix: "qbtw-"
  max_writes_per_poll: 20
```

When enabled and not in dry-run, the watchdog writes `<prefix><policy>` for tracked torrents and `<prefix>due` once their threshold is met. It removes stale tags in that namespace from all listed torrents, bounded by `max_writes_per_poll`; this cap is independent of `max_actions_per_poll`. Tag API calls are made only for actual add/remove deltas. Dry-run suppresses all tag writes, including cleanup sweeps.

The prefix is claimed in the state file before the first external tag write and is intentionally **not** part of the qBittorrent endpoint key. If you change the prefix while sync is enabled and the state file already claims another prefix, startup/polling and hot reload refuse the new enabled prefix. Remediation: set `tag_sync.enabled: false` and the new prefix with `dry_run: false` (sweeps are suppressed in dry-run), let the watchdog sweep tags using the old persisted prefix, then re-enable. Partial sweeps keep the old prefix in state so re-enabling cannot orphan tags. Renaming, deleting, or editing the state file is an escape hatch, but it loses tracking history, counters, retry budgets, recovery jobs, and the ability to clean old tags safely.

### Seed history

The watchdog tracks whether connected seeders have ever been observed for each torrent (`seed_observed` in the state file). This distinction drives the split between `stalled_no_seeders` and `stalled_seeders_seen`:

- `NumSeeds > 0` in any observation sets the flag immediately.
- Once set, the flag persists until the torrent disappears from qBittorrent or the endpoint changes.
- When a torrent transitions from `stalled_no_seeders` to `stalled_seeders_seen` (a seeder appears), the episode is reset -- the clock starts over under the new policy.

## Tracking, deletion, and persistence

- Successful polls start or extend each torrent's timer under its current policy. Observing a different state, disappearance, or a policy transition ends the episode. Time added to qBittorrent, daemon uptime, and `time_active` are not used.
- A gap longer than `max_observation_gap` resets proven continuous time, but **preserves pending-request state and the retry budget**. Long outages or restarts are not evidence that the torrent remained in its previous state. The dry-run notification marker is tied to the episode and resets with the gap.
- Dry-run emits one `warn` event per episode and continues tracking. Because a long observation gap starts a new episode, the same torrent can notify again if it later satisfies the full threshold.
- Candidates are processed oldest-first, with hash ordering to break ties. The cap limits candidates, so skips can result in fewer actions than the cap. Candidate reads complete before actions begin; active mode makes an additional targeted read immediately before each deletion and rechecks state, exclusions, progress, downloaded bytes, and continuity.
- **A targeted GET and a delete POST are not atomic.** qBittorrent provides no conditional delete API. Metadata can arrive after the final check and before deletion; the recheck narrows this unavoidable race but cannot eliminate it. Dry-run is the only mode that avoids this deletion risk entirely.
- Each request targets one hash, never `all`. HTTP 200 means `delete requested`, not proven deletion. Only a later successful list poll observing disappearance produces `action_confirmed`; disappearance does not prove who removed the torrent.
- A still-present request can be retried after `delete_confirmation_timeout`, subject to all safety checks. There are **three total active attempts per episode, including failures**, not three retries after an initial attempt. This bound is not configurable. Failed requests consume budget too; they are subject to poll backoff rather than the pending-success confirmation timeout.
- The retry budget is saved **before** submitting deletion. If that save fails, the external action is blocked, the in-memory attempt budget is rolled back/preserved, readiness is degraded, and the action is skipped until persistence recovers. After the budget is saved durably, a crash can still leave a consumed attempt on disk even if the process stops before saving the accepted-request marker; that crash-consistency tradeoff favors avoiding repeated external side effects over guaranteed delivery.
- Failed initial API reads do not trigger deletion or discard tracking. A failure later in an active batch stops further actions but cannot undo requests already accepted. Poll errors use bounded exponential backoff with jitter and remain visible in status and logs.

## State and persistence

State is a schema-versioned JSON file (current: schema version 4). It stores full hashes as tracking keys, first/last observations, pending timestamps, attempt counts, per-episode dry-run markers, seed-observation flags, the claimed watchdog tag prefix, lifetime counters, bounded audit history, and bounded recovery jobs. Schema 4 keeps durable recovery jobs separate from audit history. **Audit history stores torrent names and short hashes.** Treat the state volume and backups as private. Credentials, cookies, magnet URIs, and qBittorrent save paths are not intentionally stored. Torrent names themselves are user-controlled and may contain sensitive text.

State writes use a same-directory temporary file with mode `0600`, flush, atomic rename, and directory sync. Keep only **one watchdog process per state file**; there is no cross-process locking. Back up state while the daemon is stopped. Removing state loses clocks, retry limits, counters, and deduplication markers; do not remove it simply to force another deletion attempt.

### Schema migration

Older state layouts (schema 1 and 2) are migrated pessimistically: history is dropped, episode clocks are reset, and partition assignments are cleared. Exactly three things survive those migrations because losing them would be unsafe: lifetime counters, the finite per-episode attempt budget, and a delete request still awaiting confirmation. Schema 3 migration to schema 4 preserves clocks, seeder observations, history, counters, pending deletions and retry budgets, and initializes an empty recovery-job set. Unknown or future schema versions are treated as corrupt.

### Bounded history

History stays inside the state file rather than in a separate rotating log because it is already bounded. `history_limit` (1--10000, default 100) is enforced on append, on reload, and on load, so the newest N events are all that ever exist. Each event is UTF-8 JSON with name and error fields capped at 256 bytes each and a 12-character short hash; worst-case JSON escaping (control characters in both text fields) produces roughly 3400 bytes per event. At the default limit of 100 events this is about 340 KB; at the maximum of 10000 events the theoretical 34 MB exceeds the 32 MiB state-file ceiling that is enforced on both read and write.

### Missing or corrupt state

A missing file starts empty. Invalid JSON, unsupported schema, invalid records, or oversized state are preserved as `state.json.<UTC timestamp>.corrupt`, then tracking starts empty with a visible load warning. If the file cannot be read or the corrupt original cannot be preserved, the daemon remains operational but blocks state writes and active deletion until operator intervention and restart. Ordinary write errors degrade readiness and block deletion when the pre-action save fails; successful later saves clear the write error. Review recovered corrupt state in dry-run before returning to active operation.

## Dashboard, endpoints, and security

The web surface is read-only and uses embedded assets with no CDN, analytics, or frontend build dependencies. The dashboard shows all torrents, current policy only, configured versus effective action, observed time/threshold, “Eligible in,” versions, poll/persistence health, read-only recovery health/status, and recent actions. “Eligible in” is not a guaranteed deletion countdown: the action cap, exclusions, final qBittorrent re-read, persistence, retry budget and dry-run override still apply. Normally downloading torrents outside the closed policy set have no hypothetical classification or timer. Status uses UTC RFC 3339 timestamps and numeric seconds; the browser displays local times. Elapsed time reflects observations, not an independently advancing browser clock.

| Endpoint                                      | Purpose                                                                                         | Authentication                                                      |
| --------------------------------------------- | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| `GET /`                                       | Auto-refreshing dashboard.                                                                      | Basic Auth when configured.                                         |
| `GET /api/v1/status`                          | Schema-versioned in-memory snapshot; no qBittorrent request.                                    | Basic Auth when configured.                                         |
| `GET /healthz`                                | HTTP 200 when the HTTP server is alive, independent of qBittorrent.                             | Always public.                                                      |
| `GET /readyz`                                 | HTTP 200 after a successful, recent poll with healthy persistence; otherwise 503 with a reason. | Always public.                                                      |
| `GET /metrics`                                | Prometheus/OpenMetrics exposition.                                                              | Public by default; follows Basic Auth when `metrics_public: false`. |
| `GET /assets/app.js`, `GET /assets/style.css` | Embedded static assets, without torrent data.                                                   | Public.                                                             |

Readiness follows the age of the last successful poll, not simply the latest `qbt_up` value: it can remain ready briefly after a poll failure. A rejected configuration reload degrades readiness deliberately. Recovery health/status is read-only and separate from core readiness; a stale Sonarr/Radarr queue does not by itself make `/readyz` fail. Docker's built-in healthcheck tests **liveness**, not readiness. If you change the internal listen port, update or override the image healthcheck too.

Configure both `web_username` and `web_password` (or `web_password_file`) to protect the dashboard and status API. To protect metrics as well, set `metrics_public: false`; that setting alone does not enable authentication if no web credentials exist. The supplied Compose file does not configure web authentication. Its loopback host binding does not prevent access from other containers on the shared network.

The HTTP server has no built-in TLS listener. Keep it on trusted networks or behind a TLS-terminating reverse proxy; Basic Auth over plain HTTP does not encrypt credentials. Do not expose it directly to the internet. Response hardening includes a restrictive CSP, framing denial, no-referrer and nosniff headers, server timeouts, and bounded headers. UI/status responses disable caching. Torrent-controlled text is escaped and rendered without HTML injection. Logs and public status use short hashes, but the dashboard/status intentionally expose names, categories, and tags.

For qBittorrent HTTPS, the image includes system CA certificates; mount a PEM bundle read-only and set `tls_ca_file` for a private CA. That option configures the outbound client, not HTTPS for the dashboard.

### Prometheus

For a Prometheus container on the same Docker network, the Compose service is reachable on its internal port:

```yaml
scrape_configs:
  - job_name: qbt-watchdog
    scrape_interval: 30s
    static_configs:
      - targets: ['qbt-watchdog:8080']
```

A Prometheus process running directly on the Docker host can instead target `127.0.0.1:8090`. If metrics are protected, add `basic_auth` with the configured web username and a `password_file` readable by Prometheus; use HTTPS through your proxy across untrusted networks.

Metrics include:

| Metric                                                | Type      | Labels                                  | Description                                                |
| ----------------------------------------------------- | --------- | --------------------------------------- | ---------------------------------------------------------- |
| `qbt_watchdog_build_info`                             | gauge     | `version`, `revision`, `go_version`     | Build information.                                         |
| `qbt_watchdog_qbt_up`                                 | gauge     |                                         | Whether the latest poll succeeded.                         |
| `qbt_watchdog_last_successful_poll_timestamp_seconds` | gauge     |                                         | Last successful poll UTC Unix time.                        |
| `qbt_watchdog_poll_duration_seconds`                  | histogram |                                         | Poll cycle duration.                                       |
| `qbt_watchdog_poll_errors_total`                      | counter   |                                         | Failed poll cycles.                                        |
| `qbt_watchdog_torrents_total`                         | gauge     |                                         | Current torrents.                                          |
| `qbt_watchdog_episodes_tracked`                       | gauge     |                                         | Tracked policy episodes and pending requests.              |
| `qbt_watchdog_episodes_overdue`                       | gauge     |                                         | Overdue policy episodes.                                   |
| `qbt_watchdog_actions_total`                          | counter   | `action`, `outcome`, `dry_run`          | Watchdog actions since process startup.                    |
| `qbt_watchdog_state_write_errors_total`               | counter   |                                         | Failed state writes.                                       |
| `qbt_watchdog_config_reload_healthy`                  | gauge     |                                         | Whether the latest configuration reload was accepted.      |
| `qbt_watchdog_config_generation`                      | gauge     |                                         | Configurations successfully applied, starting at one.      |
| `qbt_watchdog_policy_tracked`                         | gauge     | `policy`                                | Current matching policy episodes.                          |
| `qbt_watchdog_policy_overdue`                         | gauge     | `policy`                                | Overdue matching policy episodes.                          |
| `qbt_watchdog_policy_threshold_seconds`               | gauge     | `policy`                                | Active policy thresholds.                                  |
| `qbt_watchdog_policy_effective_action`                | gauge     | `policy`, `effective_action`            | One for each effective action, including dry-run override. |
| `qbt_watchdog_policy_actions_total`                   | counter   | `policy`, `effective_action`, `outcome` | Policy actions by bounded policy and effective action.     |
| `qbt_watchdog_integration_healthy`                    | gauge     | `kind`                                  | Fresh successful queue snapshot; separate from readiness.  |
| `qbt_watchdog_recovery_pending`                       | gauge     | `kind`, `stage`                         | Durable recovery jobs by closed stage.                     |
| `qbt_watchdog_recovery_outcomes_total`                | counter   | `kind`, `stage`, `outcome`              | Terminal recovery outcomes; search completion is not grab. |

Labels do not contain torrent names, hashes, categories, tags, or error text. Prometheus counters are process-local; persisted lifetime action counters are available in status separately.

## CI and container images

The repository includes a GitHub Actions workflow (`.github/workflows/ci.yml`) that:

1. Runs Go checks (formatting, vet, test, race) on every pull request and push to `main`.
2. Builds a multi-platform Docker image (`linux/amd64`, `linux/arm64`) for validation.
3. Publishes to the GitHub Container Registry (`ghcr.io`) on pushes to `main` or valid v-prefixed SemVer version tags, and on manual `workflow_dispatch` from the default branch only.

**PR validation only** -- pull requests run Go checks and multi-platform image build validation; no images are published. The metadata step computes `pr-<number>` tags, but the publish job never runs for pull requests. **Manual publishing** on `workflow_dispatch` uses main branch semantics: it only publishes from the default branch, the same as a push to `main`.

Published tags include `sha-<full-sha>`, SemVer patterns (`1.2.3`, `1.2`, `1`), and `latest` on the default branch only. The workflow uses pinned action versions, read-only `contents` permissions at the top level, and `packages: write` only on the publish job.

To pull the image:

```sh
docker pull ghcr.io/<owner>/<repo>:latest
```

## Troubleshooting

| Symptom                                                     | Check                                                                                                                                                                                                                                                                                                                                            |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| External network not found / `qbittorrent` does not resolve | Select an existing network with Compose `networks`; attach both services and provide the `qbittorrent` alias. Compose does not create the external network.                                                                                                                                                                                      |
| Authentication rejected or HTTP 401/403                     | Check credentials, secret readability, WebUI host/reverse-proxy settings, and any qBittorrent temporary IP ban. HTTP 200 alone is not enough for SID login: `Ok.` and a usable SID cookie are required. For API-key mode, check key permissions, expiry, and server support for API-key auth. Avoid repeated manual login attempts during a ban. |
| Credential configuration conflict                           | API-key and username/password modes are mutually exclusive. Password and password-file are mutually exclusive for each credential pair.                                                                                                                                                                                                          |
| Cannot create or replace state                              | Keep `/data` writable and owned by UID/GID 65532 in containers. Check free space and directory permissions, not just the existing file's mode; atomic writes require creating and renaming files in that directory.                                                                                                                              |
| State could not be safely loaded                            | Preserve the original, fix access or corrupt-file backup permissions, then restart in dry-run. Unrecoverable startup load errors intentionally remain blocked for that process lifetime.                                                                                                                                                         |
| Network or TLS failure                                      | Use the final HTTP(S) URL and correct internal port. Check DNS, connectivity, certificate hostname, validity, system clock, and CA mount. Prefer `tls_ca_file`; do not disable verification in production.                                                                                                                                       |
| qBittorrent is down but container remains healthy           | Expected: `/healthz` is liveness. Inspect `/readyz`, `qbt_up`, poll errors, and last-success age. Backoff delays retries; a long gap restarts the metadata clock.                                                                                                                                                                                |
| Overdue torrent is not deleted                              | Dry-run is the default. Check policy assignment, exclusions, `max_actions_per_poll`, fresh checks, persistence, pending timeout, and retry budget. `retry limit reached` requires investigation, not automatic state removal.                                                                                                                    |
| `action_requested` persists                                 | HTTP 200 is only acceptance. The watchdog waits for disappearance and allows at most three active attempts per episode. Check qBittorrent directly.                                                                                                                                                                                              |
| Time does not survive repeated `--once` runs                | Run often enough that observations remain within `max_observation_gap`, and use the same writable state file.                                                                                                                                                                                                                                    |
| Dashboard stays ready after a recent API error              | Readiness allows a recent successful poll until `readiness_max_age`; `qbt_up` and `poll_error` show the current failure immediately.                                                                                                                                                                                                             |
| Configuration reload rejected                               | The running configuration is retained (last-known-good). Check the config file for syntax errors, unknown fields, or restart-only changes (`listen`, `state_file`). Readiness is degraded until a valid file is written.                                                                                                                         |

Use `log_level: debug` for tracking and safety decisions. Do not share raw state backups or status snapshots without reviewing torrent names and other private metadata.

## Build and test

Normal Go commands are sufficient; the Makefile provides `fmt`, `vet`, `test`, `race`, `build`, and `docker-build` wrappers.

```sh
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
make build
make docker-build
docker run --rm qbt-watchdog:test version
```

Race tests require a supported platform and C compiler; do not disable CGO for that command. Tests use fake clients/clocks, `httptest`, and temporary directories rather than a real qBittorrent instance. Once toolchains and modules are available locally, tests do not require internet access. Tests cover configuration, authentication/session handling (SID and API key), state and retry semantics, persistence, web escaping/auth/readiness, metrics, and cancellation. These are not a substitute for a dry-run integration check against your own server.

The multi-stage image builds a static binary with `CGO_ENABLED=0`, includes CA certificates, and runs from `scratch` as `65532:65532`. It contains no shell, curl, package manager, or Go runtime installation. `make build` and `make docker-build` embed version metadata. Local defaults use the current Git checkout and UTC build time; override them when needed:

```sh
VERSION=1.2.3 \
REVISION="$(git rev-parse HEAD)" \
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
make build
```

Releases are manual for now: create and push a valid `vX.Y.Z` Git tag. CI uses that tag to resolve the application version, then embeds it together with the commit revision and build timestamp in the binary and container image.

Build each supported architecture into the local image store:

```sh
docker buildx build --platform linux/amd64 --load -t qbt-watchdog:amd64 .
docker buildx build --platform linux/arm64 --load -t qbt-watchdog:arm64 .
```

For a multi-platform registry image, replace the destination below with a registry/repository you control and authenticate to it first:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=dev \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t registry.example.com/your-project/qbt-watchdog:dev --push .
```

Building a cross-platform image does not establish runtime compatibility on that architecture; running a foreign-architecture image requires emulation or a matching host. No published image location is assumed here.
