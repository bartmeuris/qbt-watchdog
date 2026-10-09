# Policies

How qbt-watchdog decides that a torrent is overdue, what each action does, and how deletion is tracked and retried. See also [the README](../README.md) and [configuration](configuration.md).

## The six policies

Policies are mutually exclusive: a torrent that the watchdog considers falls into exactly one of them. Each has an `action`, a `threshold`, and an optional `arr_mode`; `stopped_arr_managed` also requires `match_tags`. A torrent must match the same policy **continuously** for the whole threshold before its action is taken. Any change of state, progress, seeder observation, tag match or payload evidence restarts the clock.

| Policy | Condition | Notes |
| ------ | --------- | ----- |
| `metadata` | `state == "metaDL"` | Torrent stuck fetching metadata. |
| `stalled_no_seeders` | `state == "stalledDL"`, zero progress, seeders never observed | Most likely genuinely dead. |
| `stalled_seeders_seen` | `state == "stalledDL"`, zero progress, seeders observed before | The swarm may simply be idle. |
| `stalled_partial` | `state == "stalledDL"`, `0 < progress < 1` | The only policy that acts despite downloaded bytes; treat with the most caution. |
| `completed_no_data` | upload/seeding/stopped-UP state, `size == 0`, `total_size > 0`, `downloaded == 0`, `amount_left == 0` | Completed-looking with metadata but no payload. Default threshold `2m`. Takes precedence over `stopped_arr_managed`. |
| `stopped_arr_managed` | `stoppedUP`, `pausedUP`, `stoppedDL`, or `pausedDL` with any configured `match_tags` | Operator-stopped Arr-managed torrents. Default `warn` after `10m`. |

Torrents outside this closed set (for example normal downloads with payload) have no classification or timer.

## Actions

| Action | Effect |
| ------ | ------ |
| `warn` | Report only; never touch the torrent. This is the default. |
| `delete` | Remove the torrent; keep the downloaded files. |
| `delete_file` | Remove the torrent **and** its downloaded files; irreversible. |

**`dry_run` overrides every action to `warn`.** A destructive action requires both the configured `action` and `dry_run: false`. `delete_file` additionally requires `action: delete_file` on the relevant policy. `max_actions_per_poll` (default `10`) bounds actions per poll, warnings included; `0` disables all actions. In dry-run, warnings still count toward the cap.

## Exclusions and protection

Scope is configured with `include_categories`, `exclude_categories` and `exclude_tags`. Exclusion wins over inclusion. Category and tag names are compared case-sensitively as exact strings; lists are trimmed and empty elements ignored. There are no wildcards.

Two tags protect a torrent by default: **`keep`** and **`qbt-watchdog-ignore`**. Configuring `exclude_tags` replaces the defaults, so include them if you want to retain that behavior:

```yaml
include_categories: ['temporary']
exclude_categories: ['archive']
exclude_tags: ['keep', 'qbt-watchdog-ignore', 'manual']
```

Tags beginning with the reserved watchdog prefix (default `qbtw-`, see `tag_sync.prefix`) are ignored for protection and policy matching and cannot be configured as `exclude_tags` or `match_tags`. Case-insensitive prefix near-misses load with a warning. Protected torrents are excluded from tracking, so they accumulate no observed time; removing protection starts a fresh threshold clock with no inherited elapsed time.

## qBittorrent tag sync

`tag_sync` writes status tags under a reserved prefix:

```yaml
tag_sync:
  enabled: false
  prefix: 'qbtw-'
  max_writes_per_poll: 20
```

When enabled and not in dry-run, the watchdog writes `<prefix><policy>` for tracked torrents and `<prefix>due` once a threshold is met. It removes stale tags in that namespace from all listed torrents, bounded by `max_writes_per_poll` (independent of `max_actions_per_poll`). Tag API calls are made only for actual add/remove deltas. Dry-run suppresses all tag writes, including cleanup sweeps.

The prefix is claimed in the state file before the first external tag write and is intentionally not part of the qBittorrent endpoint key. Changing the prefix while sync is enabled and the state file already claims another prefix is refused at startup and on reload. Remediation: set `tag_sync.enabled: false` with the new prefix and `dry_run: false` (sweeps are suppressed in dry-run), let the watchdog sweep old tags, then re-enable. Partial sweeps keep the old prefix so re-enabling cannot orphan tags. Renaming, deleting or editing the state file is an escape hatch that loses tracking history, counters, retry budgets and recovery jobs.

## Seed history

The watchdog records whether connected seeders have ever been observed for each torrent (`seed_observed` in state). This drives the split between `stalled_no_seeders` and `stalled_seeders_seen`:

- `NumSeeds > 0` in any observation sets the flag immediately.
- Once set, it persists until the torrent disappears from qBittorrent or the endpoint changes.
- When a torrent transitions from `stalled_no_seeders` to `stalled_seeders_seen` (a seeder appears), the episode resets and the clock starts over under the new policy.

## Tracking, deletion and retry semantics

- Successful polls start or extend each torrent's timer under its current policy. Observing a different state, a disappearance, or a policy transition ends the episode. Time added to qBittorrent, daemon uptime and `time_active` are not used.
- A gap longer than `max_observation_gap` resets proven continuous time but **preserves pending-request state and the retry budget**. Long outages or restarts are not evidence that the torrent stayed in its previous state. The dry-run notification marker is tied to the episode and resets with the gap.
- Dry-run emits one `warn` event per episode and keeps tracking. Because a long gap starts a new episode, the same torrent can notify again if it later satisfies the full threshold.
- Candidates are processed oldest-first, with hash ordering to break ties. The cap limits candidates, so skips can result in fewer actions than the cap. Candidate reads complete before actions begin; active mode makes an additional targeted read immediately before each deletion and rechecks state, exclusions, progress, downloaded bytes and continuity.
- **A targeted GET and a delete POST are not atomic.** qBittorrent has no conditional-delete API, so metadata can arrive after the final check and before deletion. The recheck narrows this unavoidable race but cannot eliminate it. Dry-run is the only mode that avoids the deletion risk entirely.
- Each request targets one hash, never `all`. HTTP 200 means *delete requested*, not proven deletion. Only a later successful list poll observing the disappearance produces `action_confirmed`; disappearance does not prove who removed the torrent.
- A still-present request can be retried after `delete_confirmation_timeout`, subject to all safety checks. There are **three total active attempts per episode, including failures** (not three retries after an initial attempt); this bound is not configurable. Failed requests consume budget too and are subject to poll backoff rather than the confirmation timeout.
- The retry budget is saved **before** submitting deletion. If that save fails, the external action is blocked, the in-memory budget is rolled back/preserved, readiness is degraded, and the action is skipped until persistence recovers.
- Failed initial API reads do not trigger deletion or discard tracking. A failure later in an active batch stops further actions but cannot undo requests already accepted. Poll errors use bounded exponential backoff with jitter and remain visible in status and logs.

## Manual actions

The Overview page can queue a manual action per torrent: **Run policy now** (`run_now`, accelerate a still-tracking torrent through its threshold), **Remove torrent**, and **Remove torrent and files**. Manual actions only record the intent; the next poll executes it through the ordinary safety pipeline (exclusions, dry-run, cap, attempts, continuity). They name the torrent by short hash and never accept a full hash from the client.
