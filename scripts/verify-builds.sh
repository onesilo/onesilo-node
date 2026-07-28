#!/usr/bin/env bash
# Cross-platform build check for onesilo-node.
#
# Builds every platform we support and proves each one is reproducible:
# each target is compiled twice and the script fails if the two binaries
# differ. Nothing is packaged or published — releases ship as a container
# image (see scripts/build-image.sh), so this exists to catch breakage, not
# to produce artifacts.
#
# Two things it catches that `go test ./...` does not:
#
#   * Code that only compiles on the host platform. SiloDesktop builds this
#     daemon for macOS, so darwin must keep compiling even though no macOS
#     binary is published.
#   * Non-determinism creeping into the build, which would quietly break the
#     reproducibility guarantee the image relies on.
#
# Build flags are identical to Dockerfile's. Matching flags alone do not
# make a local build byte-identical to the image — the Go toolchain has to
# match too, since different compiler versions emit different code. The
# release workflow pins its Go version to Dockerfile's builder image for
# that reason; locally, `go version` has to match it.
#
# CGO is off and the toolchain is pure Go, so every target cross-compiles
# from any host with no C toolchain.
#
# Usage:
#   scripts/verify-builds.sh              # build each target twice, compare
#   scripts/verify-builds.sh --no-verify  # single pass (compile check only)
#
# Env overrides: VERSION, COMMIT.
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

# Platforms that must keep building. linux/* is what the published image is
# built for; darwin/* is what SiloDesktop bundles.
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

work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

echo "==> onesilo-node ${VERSION} (commit ${COMMIT})"
echo

for target in "${TARGETS[@]}"; do
    goos="${target%/*}"
    goarch="${target#*/}"

    build_one "${goos}" "${goarch}" "${work}/${BINARY}"
    sum="$(sha256_of "${work}/${BINARY}")"

    if (( VERIFY )); then
        build_one "${goos}" "${goarch}" "${work}/${BINARY}.2"
        sum2="$(sha256_of "${work}/${BINARY}.2")"
        [[ "${sum}" == "${sum2}" ]] || \
            fail "${target} is NOT reproducible: ${sum} != ${sum2}"
    fi

    printf '  %-16s %s\n' "${goos}/${goarch}" "${sum}"
done

echo
if (( VERIFY )); then
    echo "OK: every target built twice and matched"
else
    echo "OK: every target compiled (--no-verify, reproducibility not checked)"
fi
