# Homebrew formula for onesilo-node.
#
# This does NOT contradict the README's "there are no binary downloads,
# deliberately" — that rule is about shipping loose tarballs, whose problem
# is macOS Gatekeeper: a downloaded unsigned binary carries a quarantine
# attribute and is refused, which would make us an Apple-notarized
# distributor to fix. A formula that builds from source has no such problem.
# Homebrew compiles locally, nothing is quarantined, and the user gets the
# same binary `go install` would produce, with an upgrade path.
#
# The tap is live at onesilo/homebrew-tap, which holds the authoritative copy
# of this formula:
#
#   brew tap onesilo/tap && brew install onesilo-node
#
# The `url` below points at a tag archive; bump it and the `sha256` together on
# every release. The checksum is of GitHub's generated tarball for the tag:
#
#   curl -sL https://github.com/onesilo/onesilo-node/archive/refs/tags/vX.Y.Z.tar.gz \
#     | shasum -a 256
class OnesiloNode < Formula
  desc "Open-source Silo node: on-device memory and compute for One Silo"
  homepage "https://github.com/onesilo/onesilo-node"
  url "https://github.com/onesilo/onesilo-node/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "9512db188116b25ce149f64f7c3090aa572eeb88ec4995b4f0f662b45cf0bf10"
  license "Apache-2.0"
  head "https://github.com/onesilo/onesilo-node.git", branch: "main"

  # go.mod requires 1.25. Homebrew's `go` is well ahead of that, and
  # GOTOOLCHAIN=auto would fetch a matching toolchain anyway — but a build
  # dependency that reaches the network mid-build is a bad neighbour in a
  # formula, so depend on the real thing.
  depends_on "go" => :build

  def install
    # Go can fetch a newer toolchain mid-build when GOTOOLCHAIN=auto and the
    # installed go is older than go.mod's requirement. Pinning to local makes
    # the build deterministic and offline: it fails fast on too-old Go rather
    # than quietly compiling with something nobody chose.
    ENV["GOTOOLCHAIN"] = "local"
    ENV["CGO_ENABLED"] = "0"

    ldflags = %W[
      -s -w
      -buildid=
      -X github.com/onesilo/onesilo-node/internal/version.Version=v#{version}
    ]
    # These flags match scripts/verify-builds.sh (-trimpath, -s -w -buildid=,
    # CGO_ENABLED=0) so a brewed build isn't a third configuration nobody
    # verifies.
    #
    # It is NOT byte-identical to a release artifact, and doesn't claim to be:
    # the release build also injects version.Commit from the git SHA, which a
    # source tarball doesn't carry. Reproducibility against released binaries
    # is verify-builds.sh's job, not this formula's.
    system "go", "build", *std_go_args(ldflags: ldflags.join(" "), output: bin/"onesilo-node"),
           "./cmd/onesilo-node"
  end

  # No `service` block. The node's lifecycle is deliberately the operator's:
  # `setup` asks whether it should be a local node or a gateway, whether to
  # open a remote tunnel, and signs into One Silo — none of which should
  # happen from a background launchd job the user never saw. Silo Desktop is
  # the supervised path for people who want one.

  def caveats
    <<~EOS
      Configure the node before first run:
        onesilo-node setup

      Then start it:
        onesilo-node

      Re-running setup is safe — it keeps your previous answers as defaults.
      Config, admin token, and keys live in ~/.onesilo-node/.
    EOS
  end

  test do
    assert_match "onesilo-node", shell_output("#{bin}/onesilo-node -version")
    # `setup -yes` writes to ~/.onesilo-node and may download a model, so the
    # test stays with the version banner — the part that proves the ldflags
    # path resolved and the binary runs at all.
  end
end
