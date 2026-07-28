#!/usr/bin/env bash
# Reproducible Docker image build for onesilo-node.
#
# Wires SOURCE_DATE_EPOCH (commit timestamp) and BuildKit's
# rewrite-timestamp exporter option so that building the same commit twice
# produces the *same image ID*. By default the script proves it: it builds
# twice (second time with --no-cache) and fails if the IDs diverge.
#
# Usage:
#   scripts/build-image.sh            # build twice, assert identical image IDs
#   scripts/build-image.sh --single   # one build, print digest (no verification)
#
# Env overrides: IMAGE (default onesilo-node), VERSION, COMMIT, SOURCE_DATE_EPOCH.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

MODE=verify
[[ "${1:-}" == "--single" ]] && MODE=single

IMAGE="${IMAGE:-onesilo-node}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
# Commit timestamp: the canonical reproducible-build clock.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"

fail() { echo "ERROR: $*" >&2; exit 1; }

command -v docker >/dev/null || fail "docker not found"
docker buildx version >/dev/null 2>&1 || fail "docker buildx not available; reproducible builds need BuildKit (buildx >= 0.13)"

# rewrite-timestamp requires buildx/BuildKit >= 0.13.
BUILDX_VER="$(docker buildx version | grep -oE 'v[0-9]+\.[0-9]+' | head -1 | tr -d v)"
BUILDX_MAJOR="${BUILDX_VER%%.*}"
BUILDX_MINOR="${BUILDX_VER#*.}"
if (( BUILDX_MAJOR == 0 && BUILDX_MINOR < 13 )); then
    fail "buildx ${BUILDX_VER} is too old: the rewrite-timestamp exporter option needs buildx/BuildKit >= 0.13. Upgrade Docker/buildx — without it image timestamps are non-deterministic and double builds will NOT match."
fi

build() {
    local tag="$1"; shift
    docker buildx build \
        --build-arg VERSION="${VERSION}" \
        --build-arg COMMIT="${COMMIT}" \
        --build-arg SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
        --provenance=false \
        --output "type=docker,rewrite-timestamp=true" \
        -t "${tag}" \
        "$@" \
        .
}

image_id() { docker image inspect --format '{{.Id}}' "$1"; }

binary_sha256() {
    # Extract /onesilo-node from the (distroless, shell-less) image and hash it.
    local cid
    cid="$(docker create "$1")"
    docker cp "${cid}:/onesilo-node" - 2>/dev/null | tar -xO onesilo-node | shasum -a 256 | cut -d' ' -f1
    docker rm -f "${cid}" >/dev/null
}

echo "==> Building ${IMAGE}:${VERSION} (commit ${COMMIT}, SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH})"
build "${IMAGE}:${VERSION}"
ID1="$(image_id "${IMAGE}:${VERSION}")"
BIN1="$(binary_sha256 "${IMAGE}:${VERSION}")"

if [[ "${MODE}" == "single" ]]; then
    echo "image:          ${IMAGE}:${VERSION}"
    echo "image id:       ${ID1}"
    echo "binary sha256:  ${BIN1}"
    exit 0
fi

echo "==> Rebuilding with --no-cache to verify reproducibility"
build "${IMAGE}:verify" --no-cache
ID2="$(image_id "${IMAGE}:verify")"
BIN2="$(binary_sha256 "${IMAGE}:verify")"
docker rmi "${IMAGE}:verify" >/dev/null 2>&1 || true

echo
echo "image:            ${IMAGE}:${VERSION}"
echo "image id (1st):   ${ID1}"
echo "image id (2nd):   ${ID2}"
echo "binary sha (1st): ${BIN1}"
echo "binary sha (2nd): ${BIN2}"

if [[ "${BIN1}" != "${BIN2}" ]]; then
    fail "onesilo-node binary is NOT reproducible: ${BIN1} != ${BIN2}"
fi
if [[ "${ID1}" != "${ID2}" ]]; then
    fail "image IDs differ across builds (binary was identical, so a layer timestamp or metadata leaked): ${ID1} != ${ID2}"
fi

echo
echo "OK: two independent builds produced the identical image ${ID1}"
