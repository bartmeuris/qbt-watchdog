# Integrations

Optional Sonarr/Radarr recovery that blocklists a release before a watchdog-controlled qBittorrent cleanup and/or searches for a replacement after it. See also [the README](../README.md) and [configuration](configuration.md).

## Schema

Both services are disabled by default. They always exist as stubs, so a file that never mentions them simply leaves them disabled.

```yaml
integrations:
  sonarr:
    enabled: false
    url: 'http://sonarr:8989'
    api_key: ''
    api_key_file: ''
    categories: []
    mode: blocklist_and_search
    timeout: '10s'
  radarr:
    enabled: false
    url: 'http://radarr:7878'
    api_key: ''
    api_key_file: ''
    categories: []
    mode: blocklist_and_search
    timeout: '10s'
```

An **enabled** integration requires a valid HTTP(S) `url` and exactly one credential source: `api_key` or `api_key_file`. Disabled stubs may leave credentials empty, but malformed URLs, bad modes, invalid timeouts and conflicting credential sources are still rejected. `api_key_file` is re-read when its file changes and trailing CR/LF is stripped. Empty `categories` matches every qBittorrent category; non-empty lists are exact, case-sensitive matches.

## Modes

| Mode | Behavior |
| ---- | -------- |
| `blocklist_and_search` | Remove the matching Arr queue item with `blocklist=true`, `removeFromClient=false`, `skipRedownload=true`, `changeCategory=false`, then send a targeted `EpisodeSearch` (Sonarr) or `MoviesSearch` (Radarr). |
| `blocklist_only` | Remove and blocklist the release the same way, without requesting a replacement. |
| `search_only` | Send only the targeted search; does not blocklist or remove the queue item. |

Service-level `integrations.<service>.mode` accepts only those three. Each policy may override recovery with `arr_mode`: `inherit` (default), `none`, `blocklist_and_search`, `blocklist_only`, or `search_only`.

## Opt-in requirements

Recovery requires **all** of:

- an enabled integration whose `categories` matches the torrent's category;
- `dry_run: false`;
- a destructive effective policy (`delete` or `delete_file`);
- `max_actions_per_poll` greater than zero.

Warn-only and dry-run configurations never mutate Sonarr/Radarr. qBittorrent cleanup is not blocked by Arr outages: the watchdog captures recovery identity before deletion and makes exactly one best-effort blocklist attempt between the final qBittorrent safety read and the qBittorrent delete request. A failed attempt is recorded and the delete proceeds; it is never retried.

## Recovery identity

Recovery identity is the exact qBittorrent torrent hash / Arr `downloadId`, never a torrent or release name. Sonarr season packs are aggregated into one recovery job containing all matching queue rows and episode IDs. The one best-effort blocklist attempt happens before deletion; the replacement search starts only after qBittorrent accepts deletion **and** a later successful full poll confirms the hash disappeared. The Arr call never deletes imported media files; even blocklisting uses `removeFromClient=false`.

## Queue snapshots and limits

- Queue snapshots are refreshed every **30 seconds** and considered fresh for **2 minutes**.
- Up to **500** recovery jobs are persisted, each with at most **200** queue/history/media IDs, a **24-hour** lifetime, and **5 attempts per stage**.
- Retries are spaced with bounded backoff.

## Safety details

- For jobs created by the current version, a queue row that is already gone at blocklist time is recorded as `not_found`; that single best-effort attempt is not retried. The legacy `queue_vanished` outcome now applies only to in-flight jobs created by the previous version. When the mode also searches, a vanished item still proceeds to search if the media identity is known and the media is not already imported.
- Ambiguous mutation outcomes are not blindly replayed. Jobs enter an uncertain state after outcomes such as mutation timeouts or a restart during an intent stage.
- Search command completion only means the Arr command finished; it does **not** prove a replacement was grabbed. Replacement/import checks conservatively ignore the captured pack itself.
- The watchdog makes no exactly-once promise against Sonarr/Radarr's own recovery/import behavior; Arr may import, remove queue rows, or start searches independently.
- Endpoint changes fence old recovery jobs. Rotating credentials for the same endpoint is adopted live and keeps endpoint-scoped jobs valid.
- `--once` persists recovery work created by the poll but does not run the background recovery workers; continuous daemon mode is required to process those jobs.
