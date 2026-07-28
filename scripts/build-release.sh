#!/usr/bin/env bash
# Reproducible release binaries for onesilo-node.
#
# Cross-compiles every published target into dist/, archives each with the
# licence and README, and writes SHA256SUMS. Build flags are identical to
# the ones in Dockerfile, so a release binary and the one inside the image
# are byte-for-byte the same for a given commit.
#
# Like scripts/build-image.sh, this proves reproducibility rather than
# asserting it: every target is built twice and the script fails if the two
# binaries differ. CGO is off and the toolchain is pure Go, so all targets
# cross-compile from any host with no C toolchain.
#
# Usage:
#   scripts/build-release.sh              # build, verify, archive
#   scripts/build-release.sh --no-verify  # single pass (faster; CI uses full)
#
# Env overrides: VERSION, COMMIT, SOURCE_DATE_EPOCH, DIST.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

BINARY=onesilo-node
PKG=github.com/onesilo/onesilo-node
CMD=./cmd/onesilo-node

VERIFY=1
[[ "${1:-}" == "--no-verify" ]] && VERIFY=0

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
# Commit timestamp: the canonical reproducible-build clock, matching
# scripts/build-image.sh. Used for archive member mtimes.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"
DIST="${DIST:-dist}"

# Published targets. CGO is off, so these all cross-compile from one host.
# Windows is deliberately absent: the node's setup wizard, data-directory
# permissions (0700/0600) and cloudflared handling are POSIX-shaped. Adding
# it means testing those paths, not just adding a row here.
TARGETS=(
    "darwin/amd64"
    "darwin/arm64"
    "linux/amd64"
    "linux/arm64"
)

fail() { echo "ERROR: $*" >&2; exit 1; }

command -v go >/dev/null || fail "go not found"

# -trimpath strips local paths, -buildid= clears the non-deterministic build
# id, -s -w drop symbol and DWARF tables. Identical to Dockerfile.
build_one() {
    local goos="$1" goarch="$2" out="$3"
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
        go build -trimpath \
            -ldflags "-s -w -buildid= \
                -X ${PKG}/internal/version.Version=${VERSION} \
                -X ${PKG}/internal/version.Commit=${COMMIT}" \
            -o "${out}" "${CMD}"
}

sha256_of() {
    if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
    else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

rm -rf "${DIST}"
mkdir -p "${DIST}"

echo "==> onesilo-node ${VERSION} (commit ${COMMIT}, SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH})"
echo

for target in "${TARGETS[@]}"; do
    goos="${target%/*}"
    goarch="${target#*/}"
    staging="${DIST}/${goos}_${goarch}"
    mkdir -p "${staging}"

    build_one "${goos}" "${goarch}" "${staging}/${BINARY}"
    sum="$(sha256_of "${staging}/${BINARY}")"

    if (( VERIFY )); then
        tmp="$(mktemp -d)"
        build_one "${goos}" "${goarch}" "${tmp}/${BINARY}"
        sum2="$(sha256_of "${tmp}/${BINARY}")"
        rm -rf "${tmp}"
        [[ "${sum}" == "${sum2}" ]] || \
            fail "${target} is NOT reproducible: ${sum} != ${sum2}"
    fi

    cp LICENSE README.md "${staging}/"

    archive="${BINARY}_${VERSION}_${goos}_${goarch}.tar.gz"
    # --sort=name and a fixed mtime keep the archive itself deterministic,
    # not just the binary inside it. GNU tar only; on macOS this falls back
    # to a plain archive, which still contains an identical binary.
    if tar --version 2>/dev/null | grep -q GNU; then
        tar --sort=name --owner=0 --group=0 --numeric-owner \
            --mtime="@${SOURCE_DATE_EPOCH}" \
            -czf "${DIST}/${archive}" -C "${staging}" .
    else
        tar -czf "${DIST}/${archive}" -C "${staging}" .
    fi

    rm -rf "${staging}"
    printf '  %-22s %s\n' "${goos}/${goarch}" "${sum}"
done

echo
( cd "${DIST}" && \
  if command -v sha256sum >/dev/null; then sha256sum ./*.tar.gz > SHA256SUMS
  else shasum -a 256 ./*.tar.gz > SHA256SUMS; fi )

echo "==> ${DIST}/"
ls -1 "${DIST}"
echo
if (( VERIFY )); then
    echo "OK: every target built twice and matched"
else
    echo "NOTE: --no-verify, reproducibility not checked"
fi
