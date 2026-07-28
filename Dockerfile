# syntax=docker/dockerfile:1
#
# Reproducible multi-stage build for onesilo-node.
#
# The base images are pinned by digest and the Go build is stripped of all
# non-deterministic inputs (-trimpath, -buildid=, CGO off), so building the
# same commit twice yields byte-identical binaries. Full image-level
# reproducibility (identical image IDs) additionally needs BuildKit's
# rewrite-timestamp exporter option plus SOURCE_DATE_EPOCH — both wired up
# in scripts/build-image.sh (or `make image-verify`).
#
#   ./scripts/build-image.sh            # build twice, assert identical IDs
#   docker build -t onesilo-node .        # plain single build (not timestamp-normalized)

# ---------------------------------------------------------------------------
# Stage 1: build onesilo-node and fetch a checksum-pinned cloudflared
# ---------------------------------------------------------------------------
FROM golang:1.26.4-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS build

ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none

# cloudflared release binary, pinned by SHA-256 per architecture.
# Checksums verified against the GitHub release assets for 2026.6.1.
ARG CLOUDFLARED_VERSION=2026.6.1
ARG CLOUDFLARED_SHA256_AMD64=5861a10a438fe8ddcfebb3b830f83966cbf193edafce0fe2eeb198fbae1f7a22
ARG CLOUDFLARED_SHA256_ARM64=59816ce9b16db71f5bc2a86d59b3632a96c8c3ee934bde2bc8641ee83a6070eb

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) sha="${CLOUDFLARED_SHA256_AMD64}" ;; \
      arm64) sha="${CLOUDFLARED_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /cloudflared \
      "https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-${TARGETARCH}"; \
    echo "${sha}  /cloudflared" | sha256sum -c -; \
    chmod 0755 /cloudflared

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

RUN GOARCH="${TARGETARCH}" go build \
      -ldflags "-s -w -buildid= \
        -X github.com/onesilo/onesilo-node/internal/version.Version=${VERSION} \
        -X github.com/onesilo/onesilo-node/internal/version.Commit=${COMMIT}" \
      -o /onesilo-node ./cmd/onesilo-node

# ---------------------------------------------------------------------------
# Stage 2: distroless runtime (static binaries only; CA certs + tzdata included)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12@sha256:9c346e4be81b5ca7ff31a0d89eaeade58b0f95cfd3baed1f36083ddb47ca3160

COPY --from=build /onesilo-node   /onesilo-node
COPY --from=build /cloudflared /cloudflared

# All configuration is environment-driven (SILO_NODE_* vars; see
# `onesilo-node -h`). /data holds node state (device_id, pairing.key) and the
# optional persisted config.toml written by the admin API.
ENV SILO_NODE_DATA_DIR=/data \
    SILO_NODE_CONFIG=/data/config.toml \
    SILO_NODE_CLOUDFLARED_PATH=/cloudflared

VOLUME /data

# The admin API binds 127.0.0.1:8766 *inside* the container and is not
# reachable from outside; /healthz keeps Docker's health status accurate.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/onesilo-node", "healthcheck"]

ENTRYPOINT ["/onesilo-node"]
