# Development

Build, test, package and release qbt-watchdog. See also [the README](../README.md).

## Build and test

Normal Go commands are sufficient; the Makefile only wraps them.

```sh
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
make build
./qbt-watchdog version
```

| Make target | Runs |
| ----------- | ---- |
| `fmt` | `gofmt -w .` |
| `vet` | `go vet ./...` |
| `test` | `go test ./...` |
| `race` | `go test -race ./...` |
| `build` | `CGO_ENABLED=0 go build -trimpath ./cmd/qbt-watchdog` |
| `container-binaries` | Cross-compiles `dist/linux/amd64/qbt-watchdog` and `dist/linux/arm64/qbt-watchdog`. |
| `docker-build` | Runs `container-binaries`, then `docker build -t qbt-watchdog:test .` |

Race tests require a supported platform and a C compiler; do not disable CGO for that command. Tests use fake clients/clocks, `httptest`, and temporary directories — no real qBittorrent instance and, once modules are available, no internet. They cover configuration, authentication/session handling (SID and API key), state and retry semantics, persistence, web escaping/CSRF/readiness, metrics, and cancellation. They are not a substitute for a dry-run integration check against your own server.

## Build metadata

Binaries self-describe through Go's native build information (`runtime/debug.ReadBuildInfo`); there is no `-ldflags` version injection. `./qbt-watchdog version` prints JSON with:

- `version` — release tag (e.g. `v1.2.3`) or pseudo-version, with a `+dirty` suffix when the checkout has uncommitted changes;
- `revision` — the full VCS commit hash;
- `commit_time` — the commit time of that revision, not the image build time;
- `modified` — whether the working tree was dirty at build time (omitted when unknown);
- `vcs`, `go_version`, `platform` — VCS, Go toolchain, and target `GOOS/GOARCH`.

Build from a clean Git working tree to get `modified: false` and a real revision.

## Container images

There are two Dockerfiles. `Dockerfile.dev` builds the binary from source in a multi-stage build and is what local Compose uses, so `docker compose up --build` works from a fresh clone with only Docker installed. The packaging-only `Dockerfile` is used by CI and releases: it copies a prebuilt Linux binary into a `scratch` image and never runs a Go toolchain, so it requires `make container-binaries` first.

```sh
make container-binaries   # writes dist/linux/{amd64,arm64}/qbt-watchdog
make docker-build         # depends on container-binaries
docker run --rm qbt-watchdog:test version
```

`make container-binaries` cross-compiles Linux binaries even on macOS. The packaging-only image contains no shell, curl, package manager or Go runtime; it includes CA certificates and runs from `scratch` as `65532:65532`.

Build each supported architecture into the local image store (after `make container-binaries`):

```sh
docker buildx build --platform linux/amd64 --load -t qbt-watchdog:amd64 .
docker buildx build --platform linux/arm64 --load -t qbt-watchdog:arm64 .
```

For a multi-platform registry image, replace the destination with a repository you control and authenticate first:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t registry.example.com/your-project/qbt-watchdog:dev --push .
```

Building a cross-platform image does not establish runtime compatibility on that architecture; running a foreign-architecture image requires emulation or a matching host.

## Release process

Releases are manual: create and push a valid `vX.Y.Z` Git tag. CI builds the binaries from that tag, so the embedded `version` is the tag and `revision`/`commit_time` come from the tagged commit. Published image tags include `sha-<full-sha>`, SemVer patterns (`1.2.3`, `1.2`, `1`) and `latest` on the default branch only. See [deployment](deployment.md#ci-and-container-images).

## CI workflow

`.github/workflows/ci.yml`:

1. **Go checks** on every PR and push to `main`: formatting (fails without modifying sources), vet, test, race.
2. **Binaries**: cross-compile `linux/amd64` and `linux/arm64`, verify embedded VCS metadata (`vcs.revision`, `vcs.modified`, `GOOS`/`GOARCH`, `vcs.time`) with `go version -m`, and upload each binary with a SHA-256 checksum.
3. **Image**: build a multi-platform image from the verified binaries, smoke-test the native image, and confirm the packaged binary's checksum matches the artifact.
4. **Publish**: push to GHCR on pushes to `main` or valid `v`-prefixed SemVer tags, and on `workflow_dispatch` from the default branch only. PRs validate but never publish.

The workflow uses pinned action versions, read-only `contents` permissions at the top level, and `packages: write` only on the publish job.

## Architecture

```text
cmd/qbt-watchdog/        CLI entry point and subcommands
internal/config/         strict config parsing, hot reload, settings/editor seam
internal/qbt/            qBittorrent WebUI client (auth, list, get, delete)
internal/watchdog/       poll loop, policy engine, state transitions, recovery
internal/store/          versioned JSON state, schema migration, recovery jobs
internal/web/            embedded templates/assets, HTTP handlers, CSRF
internal/observability/  build metadata and Prometheus metrics
internal/arr/            Sonarr/Radarr queue, history and command client
```

The configuration is parsed once at the boundary into an immutable, validated snapshot that is published through a manager on reload; there are no per-setting flags or `QBTW_*` variables. The service owns the non-overlapping poll loop and persists state atomically. The web layer serves immutable snapshots and never initiates qBittorrent requests itself.
