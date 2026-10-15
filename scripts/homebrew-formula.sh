#!/bin/sh
# Prints the Homebrew formula for an otag release. The formula builds from the tagged source
# tarball, so the binary is never downloaded pre-built and needs no signing or quarantine tricks.
#
#   scripts/homebrew-formula.sh <version> <source-tarball-sha256> <commit>
#
# The release workflow writes the output to Formula/otag.rb in nesiler/homebrew-tap.
set -eu

if [ "$#" -ne 3 ]; then
	echo "usage: $0 <version> <sha256> <commit>" >&2
	exit 2
fi
version=$1
sha256=$2
commit=$3

cat <<EOF
class Otag < Formula
  desc "Coordinate AI coding agents (Claude Code, Codex, OpenCode) through tmux"
  homepage "https://github.com/nesiler/otag"
  url "https://github.com/nesiler/otag/archive/refs/tags/v${version}.tar.gz"
  sha256 "${sha256}"
  license "MIT"
  head "https://github.com/nesiler/otag.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos
  depends_on "tmux"

  def install
    # std_go_args already adds -s -w.
    ldflags = %W[
      -X main.version=#{version}
      -X main.commit=${commit}
      -X main.date=#{time.iso8601}
    ]
    system "go", "build", *std_go_args(ldflags:), "./cmd/otag"
  end

  def caveats
    <<~EOS
      Run \`otag\` to get started: a short wizard sets up your agents and a project,
      and can install the background daemon (launchd; \`otag daemon install\`).
    EOS
  end

  test do
    assert_match "otag #{version}", shell_output("#{bin}/otag --version")
  end
end
EOF
