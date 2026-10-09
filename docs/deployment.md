# Deployment

How to run qbt-watchdog with Docker Compose, plain Docker, or a bare binary, and how to secure it. See also [the README](../README.md).

## Docker Compose

The supplied [`compose.yaml`](../compose.yaml) deploys only the watchdog, not qBittorrent. It builds the local image (`build: .`, tagged `qbt-watchdog:local`) from source via [`Dockerfile.dev`](../Dockerfile.dev), so no prebuilt binaries are needed.

Key properties:

- joins the compose-managed network `qbt` (override its name with `QBT_NETWORK`);
- publishes the UI on loopback only: `127.0.0.1:8080:8080`;
- mounts `./config:/config` **as a writable directory** and the named volume `qbt-watchdog-data` at `/data`;
- runs with `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges`, and `restart: unless-stopped`;
- starts with `--config /config/config.yaml`.

Typical use after creating `config/config.yaml` and `config/.env` (see the [README quick start](../README.md)):

```sh
docker compose up -d --build
docker compose logs -f qbt-watchdog
```

The config mount must be a writable **directory**, not a single read-only file. The Settings editor saves `config.yaml` and `.env` with a temp-file-and-rename in the config directory, so a read-only single-file bind mount makes saving impossible and atomic replacements invisible.

## Networking

qBittorrent must resolve on the shared network as **`qbittorrent`**, either through its Compose service name or a network alias; the example connects to `http://qbittorrent:8080`. Attach your qBittorrent container to the `qbt` network, or set `QBT_NETWORK` to an existing network that already contains it.

Use qBittorrent's **container** WebUI port, not its host-published port. Inside the watchdog container, `localhost` is the watchdog itself. HTTP and HTTPS are both supported, including a reverse-proxy base path; use the base URL, not one ending in `/api/v2`.

## Docker without Compose

```sh
docker build -f Dockerfile.dev -t qbt-watchdog:local .
docker volume create qbt-watchdog-data
docker run -d --name qbt-watchdog \
  --restart unless-stopped \
  --network "${QBT_NETWORK:-qbt}" \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -p 127.0.0.1:8080:8080 \
  --mount type=volume,src=qbt-watchdog-data,dst=/data \
  --mount "type=bind,src=$(pwd)/config,dst=/config" \
  qbt-watchdog:local --config /config/config.yaml
```

`Dockerfile.dev` builds the binary from source, so only Docker is required. The packaging-only `Dockerfile` is the CI/release image: it copies a prebuilt Linux binary from `dist/` and requires `make container-binaries` first.

## Bare binary

Run as an unprivileged account with a private, writable state directory. A local process uses your account's UID, so its secret file only needs to be readable by that account:

```sh
CGO_ENABLED=0 go build -trimpath -o qbt-watchdog ./cmd/qbt-watchdog
mkdir -p data && chmod 700 data
```

Point the state file and listen address at the defaults the healthcheck expects:

```yaml
state_file: './data/state.json'
listen: '127.0.0.1:8080'
```

Then start it. `SIGINT` and `SIGTERM` stop polling and shut down the HTTP server gracefully; the first poll is immediate and polls never overlap.

```sh
./qbt-watchdog --config config.yaml
```

Add `--once` for one poll/action cycle, state persistence, and a JSON summary on stdout without a web server. A fresh empty state normally only starts tracking, and `--once` does not wait out the metadata timeout; repeated invocations must be close enough to preserve observation continuity. `--once` does not run the background Sonarr/Radarr recovery workers.

## Storage and permissions

The image prepares `/data` with owner `65532:65532` and mode `0700`; a newly initialized named volume inherits this. The container runs as **UID/GID 65532**. For a Linux bind-mounted state directory, prepare it before starting:

```sh
mkdir -p data
sudo chown 65532:65532 data
sudo chmod 700 data
```

Then replace the volume mount with `--mount "type=bind,src=$(pwd)/data,dst=/data"`. Restored state files must also be accessible to UID 65532; stop the service before changing ownership or restoring files. Use mapped ownership where user namespaces apply. Keep `/data` writable even when the root filesystem is read-only. Do not run `docker compose down -v` if you intend to keep tracking and action history.

Secret files read by the process (API key, password, TLS CA) must be readable by UID/GID 65532 and every parent directory must be traversable. On native Linux you can grant group access without making the secret world-readable:

```sh
sudo chgrp 65532 secrets secrets/qbt_api_key
chmod 750 secrets
chmod 640 secrets/qbt_api_key
```

Compose secrets are file mounts, not an encrypted store. Ownership behavior varies with Docker Desktop, rootless Docker, and user-namespace mappings; arrange equivalent readable permissions for the mapped container identity. Do not solve permission errors by making secrets public.

## Reverse proxy, TLS and security

The HTTP server has **no built-in authentication and no built-in TLS listener**. Every page, the JSON API, the settings editor, manual actions and `/metrics` are served to anyone who can reach `listen`. Deploy behind a reverse proxy that terminates TLS and enforces access control for every path you want to protect, including `/metrics`, and never expose `listen` directly to an untrusted network. The Compose loopback binding does not prevent access from other containers on the shared network.

Mutation endpoints (`POST /api/v1/config`, `POST /api/v1/settings`, `POST /api/v1/actions`) require a same-origin CSRF check — an `HX-Request` header, a `Sec-Fetch-Site` of `same-origin`/`none`, and an `Origin` host matching the request host — but no built-in username. See [api](api.md) for the full endpoint list.

Response hardening includes a restrictive CSP, framing denial, no-referrer and nosniff headers, server timeouts, and bounded header sizes. UI/status responses disable caching, and torrent-controlled text is escaped without HTML injection.

For qBittorrent over HTTPS, the image includes system CA certificates; mount a PEM bundle read-only and set `tls_ca_file` for a private CA. That option configures the outbound client, not HTTPS for the UI.

A file that still carries the former built-in web-auth keys `web_username`, `web_password`, `web_password_file` or `metrics_public` is rejected at startup and on reload with a migration error; delete those keys and enforce access control at the proxy instead.

## CI and container images

The GitHub Actions workflow (`.github/workflows/ci.yml`):

1. runs Go formatting, vet, test and race checks on every pull request and push to `main`;
2. cross-compiles `linux/amd64` and `linux/arm64` binaries in a matrix, verifies each binary's embedded VCS metadata with `go version -m`, and uploads the binary plus a SHA-256 checksum;
3. builds a multi-platform image that packages only those verified binaries, then smoke-tests the native image and confirms the packaged checksum matches the artifact;
4. publishes to the GitHub Container Registry on pushes to `main` or valid `v`-prefixed SemVer tags, and on manual `workflow_dispatch` from the default branch only.

Pull requests run the checks and image build validation but publish nothing. Published tags include `sha-<full-sha>`, SemVer patterns (`1.2.3`, `1.2`, `1`), and `latest` on the default branch only:

```sh
docker pull ghcr.io/<owner>/<repo>:latest
```

See [development](development.md) for building images locally.
