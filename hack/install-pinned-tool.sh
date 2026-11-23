#!/usr/bin/env bash
#
# Installs a pinned release tool into <dir> for the release workflows:
# trivy, which hack/scan-images.sh runs in the release job and in the
# release PR's scan gate (release-scan.yml), and crane, which reads the
# pushed image digests that the release job signs and attests.
#
#   hack/install-pinned-tool.sh <trivy|crane> <dir>
#
# Each tool is pinned by version and by the SHA-256 of its Linux x86_64
# release archive, as hack/install-rcodesign.sh pins rcodesign: the release
# job also holds the macOS signing material and the Homebrew tap token, so a
# download that does not match the pin fails and installs nothing. Unlike
# rcodesign, both tools are required, so a failed download fails too.
#
# To bump a tool, change its version and SHA-256 together. Take the SHA-256
# from the release's checksums file and check it against the release's
# provenance: `gh attestation verify <archive> --repo aquasecurity/trivy` for
# trivy, the multiple.intoto.jsonl asset for go-containerregistry. The Foreman
# scan gate pins trivy separately (TrivyVersion in
# pkg/foreman/agent/tools/run_scan_job.go).
#
# PINNED_TOOL_URL and PINNED_TOOL_SHA256 override the pin for
# scripts/install-pinned-tool_test.sh only.
set -euo pipefail

usage() {
  echo "usage: $0 <trivy|crane> <dir>" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
tool="$1"
dest="$2"

case "$tool" in
  trivy)
    version=0.76.1
    url="https://github.com/aquasecurity/trivy/releases/download/v${version}/trivy_${version}_Linux-64bit.tar.gz"
    want=c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0
    ;;
  crane)
    version=0.22.1
    url="https://github.com/google/go-containerregistry/releases/download/v${version}/go-containerregistry_Linux_x86_64.tar.gz"
    want=0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0
    ;;
  *)
    usage
    ;;
esac
url="${PINNED_TOOL_URL:-$url}"
want="${PINNED_TOOL_SHA256:-$want}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if ! curl -fsSL --retry 3 -o "$tmp/archive.tar.gz" "$url"; then
  echo "::error::could not download ${tool} ${version} from ${url}" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/archive.tar.gz" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/archive.tar.gz" | cut -d' ' -f1)"
fi
if [[ "$got" != "$want" ]]; then
  echo "::error::${tool} ${version} download has SHA-256 ${got}, want ${want}" >&2
  exit 1
fi

tar -xzf "$tmp/archive.tar.gz" -C "$tmp" "$tool"
mkdir -p "$dest"
install -m 0755 "$tmp/$tool" "$dest/$tool"
"$dest/$tool" version
