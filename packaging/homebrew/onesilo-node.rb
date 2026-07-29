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
# Staged in-repo rather than published; move to onesilo/homebrew-tap as
# Formula/onesilo-node.rb to enable:
#
#   brew tap onesilo/tap && brew install onesilo-node
#
# Until then:
#
#   brew install --build-from-source ./packaging/homebrew/onesilo-node.rb
class OnesiloNode < Formula
  desc "Open-source Silo node: on-device memory and compute for One Silo"
  homepage "https://github.com/onesilo/onesilo-node"
  url "https://github.com/onesilo/onesilo-node/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"
  head "https://github.com/onesilo/onesilo-node.git", branch: "main"

  # go.mod requires 1.25. Homebrew's `go` is well ahead of that, and
  # GOTOOLCHAIN=auto would fetch a matching toolchain anyway — but a build
  # dependency that reaches the network mid-build is a bad neighbour in a
  # formula, so depend on the real thing.
  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X github.com/onesilo/onesilo-node/internal/version.Version=v#{version}
    ]
    # -trimpath matches scripts/verify-builds.sh, which asserts the release
    # build is reproducible. Keeping the flags aligned means a Homebrew build
    # is byte-comparable to the released one for the same commit, rather than
    # a third variant nobody verifies.
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
