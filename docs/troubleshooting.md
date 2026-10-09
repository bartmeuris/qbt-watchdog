# Troubleshooting

Symptom / check / fix for the most common qbt-watchdog problems. See also [the README](../README.md).

| Symptom | Check | Fix |
| ------- | ----- | --- |
| `qbittorrent` does not resolve / connection refused | Is qBittorrent attached to the `qbt` network under the `qbittorrent` name (service name or alias)? Are you using the container WebUI port, not the host-published one? | Attach qBittorrent to the network or set `QBT_NETWORK` to the existing network; correct `qbt_url`. |
| Authentication rejected / HTTP 401 or 403 | Credentials correct? Secret file readable by UID/GID 65532? WebUI host/reverse-proxy settings allow this client? Temporary IP ban from repeated failures? | Fix credentials/permissions; wait out a ban. HTTP 200 alone is not enough for SID login — `Ok.` and a usable SID cookie are required. For API-key mode, check key validity and server support. |
| Credential configuration conflict | API-key and username/password mixed? Password and password-file both set? | Use exactly one auth mode and exactly one password source. |
| Cannot create or replace state | Is `/data` writable by UID/GID 65532? Free space? Directory permissions (atomic writes create and rename in the directory, not just the file)? | Fix `/data` ownership/permissions; free space. Keep `/data` writable even with a read-only root filesystem. |
| State could not be safely loaded | Permission error on the corrupt-file backup, or an unrecoverable startup load error? | Preserve the original, fix access, restart in dry-run. A load error intentionally stays blocked for that process lifetime. |
| Network or TLS failure to qBittorrent | Final HTTP(S) URL and internal port? DNS/connectivity? Certificate hostname, validity, system clock, CA mount? | Use the correct URL; mount a PEM bundle and set `tls_ca_file` for a private CA. Do not disable verification in production. |
| qBittorrent down but container still healthy | Expected: `/healthz` is liveness. Inspect `/readyz`, `qbt_up`, poll errors, and last-success age. | Backoff delays retries; a long gap restarts the metadata clock. Investigate the WebUI. |
| Overdue torrent is not deleted | Dry-run is the default. Policy assigned? Exclusions? `max_actions_per_poll`? Fresh re-check? Persistence healthy? Pending timeout or retry budget exhausted? | Set `dry_run: false` and a destructive `action`; clear exclusions or raise the cap as intended. `retry limit reached` needs investigation, not state removal. |
| `action_requested` persists | HTTP 200 is only acceptance; the torrent may still be present in qBittorrent. | Wait for a later poll to confirm disappearance (at most three active attempts per episode), or check qBittorrent directly. |
| Time does not survive repeated `--once` runs | Are runs close enough to stay within `max_observation_gap`, using the same writable state file? | Run more often or use continuous daemon mode. |
| Dashboard stays ready after a recent API error | Readiness allows a recent successful poll until `readiness_max_age`. | Check `qbt_up` and `poll_error` in status; they show the current failure immediately. |
| Configuration reload rejected | Syntax error, unknown field, wrong type, or a restart-only change (`listen`, `state_file`)? | Fix the file; restart only if `listen`/`state_file` must change. Readiness stays degraded until a valid file is written. |
| Settings editor cannot save | Is `/config` mounted as a writable directory (not a read-only single file)? | Remount the config directory writable; the editor saves atomically in that directory. |
| Settings save returns 409 | Was the file edited out of band? | Reload the page/editor and retry; the conflict is intentional to avoid overwriting a newer edit. |

Use `log_level: debug` for tracking and safety decisions. Do not share raw state backups or status snapshots without reviewing torrent names and other private metadata.
