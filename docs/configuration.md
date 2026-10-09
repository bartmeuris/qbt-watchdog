# Configuration

Every qbt-watchdog setting lives in one YAML or TOML file; this page documents the file, its keys, the command line, and hot reload. See also [the README](../README.md).

## File formats and command line

The configuration is a single YAML (`.yaml`/`.yml`) or TOML (`.toml`) file. Key names and values are identical in both. Parsing is strict: unknown keys and values of the wrong type are rejected, and the process refuses to start rather than silently ignore a typo.

```
Usage: qbt-watchdog [--config FILE] [--once]
       qbt-watchdog version
       qbt-watchdog healthcheck [--url URL]
```

| Flag / variable | Description |
| --------------- | ----------- |
| `--config FILE` | Configuration file path (YAML or TOML). Default: `config.yaml`. |
| `QBTW_CONFIG`   | Environment variable supplying the config path when `--config` is not given. |
| `--once`        | Run one poll/action cycle, persist, print a JSON summary, and exit. No HTTP server. Exits `1` on poll or persistence failure, `2` on configuration errors. |
| `version`       | Print build information as JSON; needs no qBittorrent configuration. |
| `healthcheck [--url URL]` | HTTP GET a liveness endpoint (default `http://127.0.0.1:8080/healthz`); accepts only HTTP 200. Exits `0` healthy, `1` unhealthy, `2` bad flags/URL. |

There are no per-setting flags or environment variables. Everything else lives in the file; `QBTW_CONFIG` only selects the file, and other environment values are used only through explicit `${NAME}` references inside it (see below).

## Keys

### qBittorrent endpoint and authentication

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `qbt_url` | string | *(required)* | WebUI base URL (`http://`/`https://`, no credentials, query or fragment). Use the base URL, not one ending in `/api/v2`. |
| `qbt_api_key` | string | `""` | API key sent as a Bearer token (qBittorrent >= 5.2.0). |
| `qbt_api_key_file` | string | `""` | File containing the API key; trailing CR/LF stripped, re-read on change. |
| `qbt_username` | string | `""` | WebUI username for SID cookie auth. |
| `qbt_password` | string | `""` | Inline password for SID cookie auth. |
| `qbt_password_file` | string | `""` | File containing the password; trailing CR/LF stripped, re-read on change. |

API-key and username/password modes are mutually exclusive. In username/password mode, supply both a username and exactly one password source; inline and file passwords are mutually exclusive. A bypass mode exists: leave every credential empty and rely on qBittorrent's local-network auth bypass (do not broaden that policy just to deploy this service). Credentials are never logged or exposed in status, metrics, UI or errors.

### Safety

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `dry_run` | bool | `true` | While true, every action is downgraded to `warn` and all tag writes/sweeps are suppressed. |
| `max_actions_per_poll` | int | `10` | Upper bound on actions (including warnings) per poll; `0` means no action at all. Bounds: `0`–`10000`. |
| `tag_sync.enabled` | bool | `false` | Write status tags under the reserved prefix back to qBittorrent. |
| `tag_sync.prefix` | string | `qbtw-` | Reserved tag namespace; at least 4 characters, no commas or surrounding whitespace. |
| `tag_sync.max_writes_per_poll` | int | `20` | Cap on tag add/remove deltas per poll; `1`–`10000`. Independent of `max_actions_per_poll`. |

### Policies

The six policies are mutually exclusive and share one shape. `policies.stopped_arr_managed.match_tags` is mandatory; all other keys are optional with defaults.

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `policies.<id>.action` | string | `warn` | `warn`, `delete`, or `delete_file`. |
| `policies.<id>.threshold` | duration | see below | Continuous match time before the action. |
| `policies.<id>.arr_mode` | string | `inherit` | `inherit`, `none`, `blocklist_and_search`, `blocklist_only`, `search_only`. |
| `policies.stopped_arr_managed.match_tags` | list | `["Sonarr","Radarr"]` | Required non-empty list of operator tags that select stopped torrents. |

Per-policy thresholds: `metadata`, `stalled_no_seeders`, `stalled_seeders_seen`, `stalled_partial` default to `30m`; `completed_no_data` to `2m`; `stopped_arr_managed` to `10m`. `match_tags` is only valid on `stopped_arr_managed`. See [policies](policies.md) for conditions and actions.

### Timing

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `poll_interval` | duration | `30s` | Interval between completed polls; must be positive. |
| `max_observation_gap` | duration | `3 × poll_interval` (`90s` at default) | Maximum gap between observations that still counts as continuous. |
| `http_timeout` | duration | `10s` | Per-request timeout against the qBittorrent API. |
| `delete_confirmation_timeout` | duration | `2m` | How long to wait for a requested deletion to disappear before allowing a retry. |

All of these must be positive durations written in Go syntax (`30s`, `2h`, `90s`).

### Scope

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `include_categories` | list | `[]` | When non-empty, only these categories are considered. |
| `exclude_categories` | list | `[]` | Categories never considered; applied after `include_categories`. |
| `exclude_tags` | list | `["keep","qbt-watchdog-ignore"]` | Torrents carrying any of these tags are never considered. |

Lists are YAML/TOML sequences; values are trimmed and compared case-sensitively as exact strings, with no wildcards. Empty elements are ignored. Configuring `exclude_tags` replaces the defaults. Tags beginning with the reserved watchdog prefix (`qbtw-` by default) cannot be used as `exclude_tags` or `match_tags`.

### Sonarr/Radarr integrations

Disabled by default. See [integrations](integrations.md) for behavior.

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `integrations.sonarr.enabled` / `integrations.radarr.enabled` | bool | `false` | Turn the service on. |
| `integrations.<svc>.url` | string | `http://sonarr:8989` / `http://radarr:7878` | Base URL; required while enabled. |
| `integrations.<svc>.api_key` | string | `""` | Inline API key. |
| `integrations.<svc>.api_key_file` | string | `""` | File holding the API key; re-read on change. |
| `integrations.<svc>.categories` | list | `[]` | Empty matches every category; otherwise exact, case-sensitive. |
| `integrations.<svc>.mode` | string | `blocklist_and_search` | `blocklist_and_search`, `blocklist_only`, or `search_only`. |
| `integrations.<svc>.timeout` | duration | `10s` | Positive, at most `5m`. |

An enabled service requires a valid HTTP(S) URL and exactly one credential source. Structural settings (mode, timeout, URL syntax, credential exclusivity) are validated even while disabled.

### State

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `state_file` | string | `/data/state.json` | Where episodes, counters and history are persisted. **Restart required.** |
| `history_limit` | int | `100` | Retained history entries; `1`–`10000`. |

### Web interface

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `listen` | string | `:8080` | `host:port` for the pages, JSON API, health endpoints and metrics. **Restart required.** |
| `ui_refresh_interval` | duration | `5s` | Live-fragment refresh interval. |
| `readiness_max_age` | duration | `2m` | `/readyz` fails once the last successful poll is older than this. |

### TLS

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `tls_ca_file` | string | `""` | PEM bundle used to verify a qBittorrent instance with a private CA; re-read on change. |
| `tls_insecure_skip_verify` | bool | `false` | Development-only; disables certificate verification. Logged loudly. |

This configures the outbound qBittorrent client, not HTTPS for the UI.

### Logging

| Key | Type | Default | Meaning |
| --- | ---- | ------- | ------- |
| `log_level` | string | `info` | `debug`, `info`, `warn`, or `error`. |
| `log_format` | string | `json` | `json`, `text`, or `console`. `console` is human-readable terminal output. |
| `log_color` | string | `auto` | `auto`, `always`, or `never`; affects `console` only. `auto` colors when stderr is a terminal and `NO_COLOR` is unset. |

## Authentication modes

Three modes are enforced as mutually exclusive at startup:

- **API key** (qBittorrent >= 5.2.0, WebUI API >= 2.14.1): set `qbt_api_key` or `qbt_api_key_file`. The key is sent as a `Bearer` token; no cookie jar is created and no login endpoint is called. On HTTP 401/403 the error is reported immediately without retry.
- **Username + password**: set `qbt_username` and `qbt_password` (or `qbt_password_file`). The client logs in via `/api/v2/auth/login`, stores the SID cookie, and reauthenticates once on session expiry. Mutations are never retried after reauthentication.
- **Bypass**: leave all credentials empty; rely on qBittorrent's local-network authentication bypass.

## Environment substitution and `.env`

Configuration is not a shell script. After YAML/TOML is parsed, only **string values** are scanned for explicit `${NAME}` references; `$$` becomes a literal `$`. Bare `$HOME`, `$NAME`, backticks and `$(...)` stay literal. Substituted values are never recursively expanded. Keys, numbers, booleans and list structure are never rewritten.

An optional `.env` file is loaded from the directory containing the resolved config file (for example `/config/.env` for `--config /config/config.yaml`), not the working directory. Process environment wins over `.env`, including empty process values. On reload, a missing variable, malformed reference, invalid `.env` syntax or invalid candidate retains the last working configuration; during initial startup the same errors are fatal.

Supported `.env` syntax is deliberately small: one `NAME=value` per line; optional `export`; spaces around `=`; blank lines, comments, CRLF, and single/double quoted values. Duplicate names use the last value. Single quotes are literal. Double quotes decode only `\n`, `\r`, `\t`, `\\`, `\"` and `\$`; all other escapes, multiline quotes and trailing tokens are errors. Unquoted `#` starts a comment only at the start of the value or after horizontal whitespace. `.env` does not expand `$NAME`, `${NAME}`, `$$`, backticks or command substitutions.

Secret files (`*_file` keys), the CA bundle, and `.env` are watched through their parent directories and re-read when they change, so rotating a mounted secret needs no restart.

## Reloading

The file is re-read automatically whenever it or a referenced dependency changes; no restart or signal is needed. Atomic replacements (write-temp-then-rename) and Kubernetes ConfigMap/Secret symlink swaps are detected, and rapid writes are debounced into one reload. An invalid reload keeps the previous configuration in full, logs the problem, and surfaces the error in status and on `/readyz` until a good file is written.

**Restart required** — changing either of these is rejected and the whole candidate is discarded:

- `listen` (the socket is already bound)
- `state_file` (the episode store is already open)

**Applied live** — everything else: the qBittorrent endpoint, credentials and TLS settings (the HTTP client is rebuilt), all policies, every interval, exclusions, `tag_sync`, logging, readiness/UI settings, and Sonarr/Radarr integrations.

**Safety-affecting reloads reset timers.** Changing a policy, `dry_run`, an interval, the exclusions, the action cap, qBittorrent credentials, or TLS CA/insecure settings resets policy timers as applicable so no torrent inherits unsafe elapsed time. Sonarr/Radarr-only changes do not reset qBittorrent clocks. The finite attempt budget and a delete still awaiting confirmation are deliberately preserved across ordinary safety reloads. Exception: changing the qBittorrent endpoint fences old endpoint state by clearing per-torrent clocks, pending-request markers, retry budgets and seeder observations.

## State file and schema

State is a schema-versioned JSON file (current: **schema 4**) written with a same-directory temporary file, mode `0600`, flush, atomic rename, and directory sync. It stores tracking keys (full hashes), first/last observations, pending timestamps, attempt counts, per-episode dry-run markers, seed-observation flags, the claimed tag prefix, lifetime counters, bounded audit history, and bounded recovery jobs.

- **Audit history stores torrent names and short hashes.** Treat the state volume and backups as private. Credentials, cookies, magnet URIs and save paths are not intentionally stored.
- `history_limit` is enforced on append, reload and load. Each event's name/error text is capped at 256 bytes with a 12-character short hash; the state file is capped at 32 MiB on read and write.
- Keep only **one watchdog process per state file**; there is no cross-process locking. Back up state while the daemon is stopped.
- A missing file starts empty. Invalid JSON, an unsupported schema, invalid records or oversized state are preserved as `state.json.<UTC timestamp>.corrupt`, then tracking restarts empty with a visible warning. If the corrupt original cannot be preserved, the daemon stays operational but blocks state writes and deletion until operator intervention and restart.
- Older schemas (1 and 2) migrate pessimistically: history is dropped, clocks reset, partitions cleared; lifetime counters, the attempt budget, and a delete awaiting confirmation survive. Schema 3 to 4 preserves clocks, seed observations, history, counters, pending deletions, retry budgets, and initializes an empty recovery-job set. Unknown/future versions are corrupt.

## Logging

Logging uses `log/slog`. The default is JSON on stderr; `text` and `console` are alternatives. `log_level`, `log_format` and `log_color` apply live. Use `log_level: debug` when investigating tracking and safety decisions. Logs and public status use short hashes, but torrent names, categories and tags are intentionally visible; protect log output accordingly.
