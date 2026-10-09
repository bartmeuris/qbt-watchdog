# qbt-watchdog

A small, stateful Go daemon that watches qBittorrent for torrents stuck fetching
metadata, stalled downloads, completed-looking torrents with no payload, and
operator-stopped Arr-managed torrents. It measures how long each torrent has been
*continuously* observed under a policy, reports overdue torrents in a web UI, and
can remove them after a configurable threshold or on demand. It is not a general
torrent manager: it does not implement BitTorrent, work with Prowlarr, or manage
multiple qBittorrent instances in one process. Optional Sonarr/Radarr recovery
makes one best-effort blocklist attempt while the torrent is still present,
before the watchdog deletes it, then searches for a replacement only after a
later poll confirms the torrent disappeared. It never manages import libraries
or deletes imported media.

**It starts in dry-run mode.** Dry-run never calls the delete endpoint and never
writes qBittorrent watchdog tags. Enabling deletion requires the deliberate
setting `dry_run: false` plus a destructive action on a policy.

## Why use it

- **Safe by default.** Nothing is removed until you explicitly opt in, and every
  policy is reported before it can act.
- **Six focused policies.** Metadata stalls, stalled downloads with or without
  observed seeders, partial downloads, completed-with-no-data, and
  operator-stopped Arr torrents are tracked independently.
- **Continuity, not age.** A torrent must match the same policy for the whole
  threshold; a gap longer than `max_observation_gap` restarts the clock.
- **Operable.** Atomic state, bounded retries, `/healthz` + `/readyz`, Prometheus
  metrics, live web UI, and per-torrent protection tags.

## Quick start

Prerequisites: qBittorrent's WebUI enabled and reachable from the watchdog, and
Docker with Compose. The examples assume qBittorrent is reachable as
`qbittorrent` on the shared `qbt` network (attach your qBittorrent container to
it, or set `QBT_NETWORK` to an existing network).

**1. Create `config/config.yaml`:**

```yaml
qbt_url: 'http://qbittorrent:8080'
qbt_username: 'admin'
qbt_password: '${QBT_PASSWORD}'
dry_run: true
```

**2. Create `config/.env`** with the password referenced above:

```sh
QBT_PASSWORD=change-me
```

**3. Start it:**

```sh
docker compose up -d --build
```

**4. Inspect it.** Open **http://127.0.0.1:8080** and confirm the header shows the
dry-run badge and healthy indicators: qBittorrent versions detected, polling
succeeding, and persistence healthy. A torrent must be continuously observed for
its policy threshold (30 minutes for most policies) before it is reported overdue.

**5. Enable deletion only after reviewing dry-run.** Set `dry_run: false` and set
each policy's `action` (for example `action: 'delete'`). See
[docs/policies.md](docs/policies.md). `delete` removes the torrent but keeps
downloaded files; `delete_file` removes the data too and is irreversible.

## Documentation

| Guide | Contents |
| ----- | -------- |
| [Configuration](docs/configuration.md) | Every key with type and default, CLI flags, env substitution, reloading, state and logging. |
| [Policies](docs/policies.md) | The six policies, actions, exclusions and protection, tag sync, deletion and retry semantics. |
| [Deployment](docs/deployment.md) | Compose, Docker without Compose, bare binary, networking, storage, TLS and reverse proxy. |
| [Integrations](docs/integrations.md) | Sonarr/Radarr recovery schema, modes, limits and safety. |
| [Monitoring](docs/monitoring.md) | Logging, `/healthz` vs `/readyz`, Prometheus scrape config and the metrics table. |
| [API](docs/api.md) | HTTP endpoints, CSRF requirements and authentication. |
| [Troubleshooting](docs/troubleshooting.md) | Symptom / check / fix for network, auth, state, TLS, readiness and deletion issues. |
| [Development](docs/development.md) | Build and test commands, Makefile targets, container images, releases and architecture. |

## Security

The web UI has **no built-in authentication**. Every page, the JSON API, the
settings editor, manual actions and `/metrics` are served to anyone who can reach
`listen`. Keep it bound to loopback (as the example does) or deploy behind a
reverse proxy that terminates TLS and enforces access control. See
[docs/deployment.md](docs/deployment.md).
