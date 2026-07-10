#!/usr/bin/env bash
# Render Formula/llmkube.rb from the release checksums and publish it to the
# Homebrew tap.
#
# Why a hand-maintained formula: GoReleaser removed the `brews` option in v2.16
# (formulae are meant to build from source, so it steers binary distribution to
# `homebrew_casks`). But casks are macOS-only, which drops Linux Homebrew for a
# cross-platform CLI. A binary-download formula selects the right prebuilt
# archive per OS/arch and works on macOS AND Linux from one artifact and one
# `brew install defilantech/tap/llmkube`. See defilantech/LLMKube#1039 / #1040.
#
# Usage: publish-homebrew-formula.sh <version> [checksums-file]
#   DRY_RUN=1 prints the rendered formula and exits (no clone, no push).
set -euo pipefail

VERSION="${1:?usage: publish-homebrew-formula.sh <version> [checksums-file]}"
CHECKSUMS="${2:-dist/checksums.txt}"
TAP_SLUG="defilantech/homebrew-tap"

# sha_for prints the sha256 of the CLI archive for the given os_arch, reading
# GoReleaser's "<sha256>  <filename>" checksums file.
sha_for() {
  local arch="$1" line
  line=$(grep -E "[[:space:]]LLMKube_${VERSION}_${arch}\.tar\.gz\$" "$CHECKSUMS" || true)
  [ -n "$line" ] || { echo "no checksum for LLMKube_${VERSION}_${arch}.tar.gz in $CHECKSUMS" >&2; exit 1; }
  echo "${line%% *}"
}

DARWIN_ARM=$(sha_for darwin_arm64)
DARWIN_AMD=$(sha_for darwin_amd64)
LINUX_ARM=$(sha_for linux_arm64)
LINUX_AMD=$(sha_for linux_amd64)

render() {
  cat <<EOF
class Llmkube < Formula
  desc "GPU-accelerated Kubernetes operator for local LLM inference"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_linux_arm64.tar.gz"
      sha256 "${LINUX_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_linux_amd64.tar.gz"
      sha256 "${LINUX_AMD}"
    end
  end

  def install
    bin.install "llmkube"
  end

  test do
    assert_match "llmkube", shell_output("#{bin}/llmkube version 2>&1")
  end
end
EOF
}

if [ "${DRY_RUN:-}" = "1" ]; then
  render
  exit 0
fi

: "${HOMEBREW_TAP_TOKEN:?HOMEBREW_TAP_TOKEN is required to push the tap}"
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
git clone --depth 1 "https://x-access-token:${HOMEBREW_TAP_TOKEN}@github.com/${TAP_SLUG}.git" "$workdir/tap"

mkdir -p "$workdir/tap/Formula"
render >"$workdir/tap/Formula/llmkube.rb"
# Retire the macOS-only cask; the cross-platform formula supersedes it.
git -C "$workdir/tap" rm -f --ignore-unmatch Casks/llmkube.rb
git -C "$workdir/tap" add Formula/llmkube.rb

if git -C "$workdir/tap" diff --cached --quiet; then
  echo "llmkube formula already at v${VERSION}; nothing to publish"
  exit 0
fi

git -C "$workdir/tap" \
  -c user.name="github-actions[bot]" \
  -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
  commit -m "Update llmkube to v${VERSION} (cross-platform formula)"
git -C "$workdir/tap" push
echo "published llmkube formula v${VERSION} to ${TAP_SLUG}"
