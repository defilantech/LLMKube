#!/usr/bin/env bash
# check-go-version.sh: every Go builder image in the repo must use the Go
# version go.mod pins (#2211).
#
# CI, goreleaser and the released binaries read the version from go.mod
# (actions/setup-go go-version-file). The Dockerfiles that build dev, e2e and
# coder images name their own golang tag, and before this check they drifted:
# go.mod said 1.26.10 while most builders used a floating golang:1.27 and the
# e2e stub used golang:1.26-alpine, so a local or e2e image was built by a
# different Go than the release.
#
# Checked: the FROM lines of every tracked Dockerfile, and the devcontainer
# image. A tag passes when its version, before any "-variant" suffix, equals
# go.mod's go line exactly ("1.27.4", "1.27.4-alpine"). Go source that names
# a golang image for other repos (the Foreman gate and scan presets) is not
# a builder of this repo and is not checked.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

want="$(awk '$1 == "go" { print $2; exit }' go.mod)"
if [ -z "$want" ]; then
  echo "❌ no go line in go.mod"
  exit 2
fi

failed=0
checked=0

check_tag() {
  local where="$1" tag="$2"
  local version="${tag%%-*}"
  checked=$((checked + 1))
  if [ "$version" != "$want" ]; then
    echo "❌ $where uses golang:$tag, but go.mod pins go $want"
    failed=1
  fi
}

while IFS= read -r file; do
  while IFS= read -r hit; do
    line="${hit%%:*}"
    tag="$(printf '%s\n' "${hit#*:}" | sed -nE 's/.*golang:([^[:space:]@]+).*/\1/p')"
    [ -n "$tag" ] && check_tag "$file:$line" "$tag"
  done < <(grep -nE '^[[:space:]]*FROM[[:space:]].*golang:' "$file" || true)
done < <(git ls-files -- 'Dockerfile*' ':(glob)**/Dockerfile*')

devcontainer=".devcontainer/devcontainer.json"
if [ -f "$devcontainer" ]; then
  while IFS= read -r hit; do
    line="${hit%%:*}"
    tag="$(printf '%s\n' "${hit#*:}" | sed -nE 's/.*"golang:([^"]+)".*/\1/p')"
    [ -n "$tag" ] && check_tag "$devcontainer:$line" "$tag"
  done < <(grep -nE '"image"[[:space:]]*:[[:space:]]*"golang:' "$devcontainer" || true)
fi

if [ "$checked" -eq 0 ]; then
  echo "❌ found no golang builder images to check"
  exit 2
fi
if [ "$failed" -ne 0 ]; then
  echo "Set each tag above to $want (or $want-<variant>), or move go.mod."
  exit 1
fi
echo "✅ $checked golang builder image(s) match go.mod's go $want."
