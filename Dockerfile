# syntax=docker/dockerfile:1
# Packaging only: the binary is built once per platform by `make container-binaries`
# (or the CI `binaries` job) and copied in here. No Go toolchain runs in this image.
FROM --platform=$BUILDPLATFORM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS prep
RUN apk add --no-cache ca-certificates \
    && mkdir -p /data \
    && chown 65532:65532 /data \
    && chmod 0700 /data

FROM scratch
ARG TARGETOS
ARG TARGETARCH
LABEL org.opencontainers.image.title="qbt-watchdog" \
      org.opencontainers.image.description="qBittorrent metadata and stalled-download watchdog with a web UI"
COPY --from=prep /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chmod=0555 dist/${TARGETOS}/${TARGETARCH}/qbt-watchdog /qbt-watchdog
COPY --from=prep --chown=65532:65532 /data /data
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/qbt-watchdog", "healthcheck", "--url", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/qbt-watchdog"]
