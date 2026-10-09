# Monitoring

Logging, health/readiness endpoints, and Prometheus metrics for qbt-watchdog. See also [the README](../README.md).

## Logs

The daemon logs structured events with `log/slog`: startup and effective non-secret configuration, qBittorrent auth and detected versions, poll success/failure and backoff, tracking start/reset/stop, exclusion and safety-guard decisions at debug level, action events `warn`/`action_requested`/`action_confirmed`/`action_skipped`/`action_failed` (plus a separate `delete_succeeded` line when a requested deletion is confirmed), persistence errors, and graceful shutdown.

Use `log_level: debug` when investigating tracking and safety decisions. Logs use short hashes, but torrent names, categories and tags are intentionally visible; review log output before sharing. Credentials, cookies and full request bodies are never logged.

## Health and readiness

| Endpoint | Meaning |
| -------- | ------- |
| `GET /healthz` | **Liveness.** HTTP 200 whenever the HTTP server is alive, independent of qBittorrent. |
| `GET /readyz` | **Readiness.** HTTP 200 only after a successful, recent poll with healthy persistence and an accepted configuration; otherwise 503 with a reason. |

Readiness follows the **age of the last successful poll**, not just the latest `qbt_up` value: it can remain ready briefly after a poll failure until `readiness_max_age` elapses. A rejected configuration reload degrades readiness deliberately — the file on disk is not what the process is running — while liveness is unaffected because the last known good configuration keeps working. Recovery/integration health is separate: a stale Sonarr/Radarr queue does not by itself make `/readyz` fail.

Docker's built-in `HEALTHCHECK` tests **liveness**, not readiness. If you change the internal listen port, update or override the image healthcheck too.

## Prometheus

For a Prometheus container on the same Docker network, target the service on its internal port:

```yaml
scrape_configs:
  - job_name: qbt-watchdog
    scrape_interval: 30s
    static_configs:
      - targets: ['qbt-watchdog:8080']
```

A Prometheus process on the Docker host can instead target `127.0.0.1:8080`. `/metrics` is always served **without credentials** by the watchdog, so protect that path at the reverse proxy if the exposition must stay private, and use HTTPS across untrusted networks.

### Metrics

| Metric | Type | Labels | Description |
| ------ | ---- | ------ | ----------- |
| `qbt_watchdog_build_info` | gauge | `version`, `revision`, `go_version` | Build information. |
| `qbt_watchdog_qbt_up` | gauge | | Whether the latest poll succeeded. |
| `qbt_watchdog_last_successful_poll_timestamp_seconds` | gauge | | Last successful poll UTC Unix time. |
| `qbt_watchdog_poll_duration_seconds` | histogram | | Poll cycle duration. |
| `qbt_watchdog_poll_errors_total` | counter | | Failed poll cycles. |
| `qbt_watchdog_torrents_total` | gauge | | Current torrents. |
| `qbt_watchdog_episodes_tracked` | gauge | | Tracked policy episodes and pending requests. |
| `qbt_watchdog_episodes_overdue` | gauge | | Overdue policy episodes. |
| `qbt_watchdog_actions_total` | counter | `action`, `outcome`, `dry_run` | Watchdog actions since process startup. |
| `qbt_watchdog_state_write_errors_total` | counter | | Failed state writes. |
| `qbt_watchdog_config_reload_healthy` | gauge | | Whether the latest configuration reload was accepted. |
| `qbt_watchdog_config_generation` | gauge | | Configurations successfully applied, starting at one. |
| `qbt_watchdog_policy_tracked` | gauge | `policy` | Current matching policy episodes. |
| `qbt_watchdog_policy_overdue` | gauge | `policy` | Overdue matching policy episodes. |
| `qbt_watchdog_policy_threshold_seconds` | gauge | `policy` | Active policy thresholds. |
| `qbt_watchdog_policy_effective_action` | gauge | `policy`, `effective_action` | One for each effective action, including the dry-run override. |
| `qbt_watchdog_policy_actions_total` | counter | `policy`, `effective_action`, `outcome` | Policy actions by bounded policy and effective action. |
| `qbt_watchdog_integration_healthy` | gauge | `kind` | Fresh successful queue snapshot; separate from readiness. |
| `qbt_watchdog_recovery_pending` | gauge | `kind`, `stage` | Durable recovery jobs by closed stage. |
| `qbt_watchdog_recovery_outcomes_total` | counter | `kind`, `stage`, `outcome` | Terminal recovery outcomes; search completion is not a grab. |

Labels never contain torrent names, hashes, categories, tags or error text. Prometheus counters are process-local; persisted lifetime action counters are available in the status payload separately.
