# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=$VERSION -X main.revision=$REVISION -X main.buildDate=$BUILD_DATE" -o /out/qbt-watchdog ./cmd/qbt-watchdog \
    && mkdir -p /out/data && chown 65532:65532 /out/data && chmod 0700 /out/data

FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="qbt-watchdog" \
      org.opencontainers.image.description="qBittorrent metadata and stalled-download watchdog with a web UI" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION \
      org.opencontainers.image.created=$BUILD_DATE
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/qbt-watchdog /qbt-watchdog
COPY --from=builder --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/qbt-watchdog", "healthcheck", "--url", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/qbt-watchdog"]
