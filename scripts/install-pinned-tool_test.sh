#!/usr/bin/env bash
#
# Regression test for hack/install-pinned-tool.sh, which installs the pinned
# trivy and crane for the release workflows. A download that does not match
# the pinned SHA-256 fails and installs nothing, and so does a failed
# download: both tools are required, unlike rcodesign.
#
# The downloads are file:// URLs to tarballs built here, through the
# PINNED_TOOL_URL and PINNED_TOOL_SHA256 overrides.
#
# Usage: scripts/install-pinned-tool_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
INSTALL="$REPO_ROOT/hack/install-pinned-tool.sh"
[[ -f "$INSTALL" ]] || { echo "FAIL - cannot find hack/install-pinned-tool.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

sha256_of() { { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1"; } | cut -d' ' -f1; }
perm_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# A tarball laid out like each release asset: the binary at the top level,
# beside files the install must leave behind.
for tool in trivy crane; do
  mkdir -p "$WORK/src-$tool"
  printf '#!/bin/sh\necho "%s stub $*"\n' "$tool" >"$WORK/src-$tool/$tool"
  chmod 0755 "$WORK/src-$tool/$tool"
  echo license >"$WORK/src-$tool/LICENSE"
  tar -czf "$WORK/$tool.tar.gz" -C "$WORK/src-$tool" "$tool" LICENSE
done

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -12
  fails=$((fails+1))
}

# install_from <tool> <url> <sha256> <dest>: sets CODE and OUT.
install_from() {
  OUT="$(PINNED_TOOL_URL="$2" PINNED_TOOL_SHA256="$3" bash "$INSTALL" "$1" "$4" 2>&1)" && CODE=0 || CODE=$?
}

for tool in trivy crane; do
  dest="$WORK/bin-good-$tool"
  install_from "$tool" "file://$WORK/$tool.tar.gz" "$(sha256_of "$WORK/$tool.tar.gz")" "$dest"
  if [[ $CODE == 0 && -x "$dest/$tool" && "$(perm_of "$dest/$tool")" == 755 && "$OUT" == *"$tool stub version"* && ! -e "$dest/LICENSE" ]]; then
    pass "a matching $tool download installs only the binary, executable, and runs it"
  else
    fail "a matching $tool download installs only the binary, executable, and runs it" "exit $CODE"
  fi
done

dest="$WORK/bin-mismatch"
good_sha="$(sha256_of "$WORK/trivy.tar.gz")"
install_from trivy "file://$WORK/trivy.tar.gz" "0000000000000000000000000000000000000000000000000000000000000000" "$dest"
if [[ $CODE != 0 && "$OUT" == *"::error::"*"$good_sha"* && ! -e "$dest" ]]; then
  pass "a download that does not match the pinned SHA-256 fails and installs nothing"
else
  fail "a download that does not match the pinned SHA-256 fails and installs nothing" "exit $CODE"
fi

dest="$WORK/bin-missing"
install_from crane "file://$WORK/does-not-exist.tar.gz" "$good_sha" "$dest"
if [[ $CODE != 0 && "$OUT" == *"::error::"*"could not download"* && ! -e "$dest" ]]; then
  pass "a failed download fails and installs nothing"
else
  fail "a failed download fails and installs nothing" "exit $CODE"
fi

printf 'not a tarball' >"$WORK/bad.tar.gz"
dest="$WORK/bin-bad"
install_from trivy "file://$WORK/bad.tar.gz" "$(sha256_of "$WORK/bad.tar.gz")" "$dest"
if [[ $CODE != 0 && ! -e "$dest/trivy" ]]; then
  pass "a matching but unreadable tarball fails"
else
  fail "a matching but unreadable tarball fails" "exit $CODE"
fi

dest="$WORK/bin-unknown"
install_from cosign "file://$WORK/trivy.tar.gz" "$good_sha" "$dest"
if [[ $CODE == 2 && "$OUT" == *"usage:"* && ! -e "$dest" ]]; then
  pass "a tool without a pin is refused"
else
  fail "a tool without a pin is refused" "exit $CODE"
fi

# The pins themselves: a truncated or mistyped SHA-256 would otherwise
# surface only in a release run.
for tool in trivy crane; do
  pin="$(awk -v t="$tool)" '$1 == t {on=1; next} on && /;;/ {exit} on && /want=/ {sub(/.*want=/, ""); print}' "$INSTALL")"
  if [[ "$pin" =~ ^[0-9a-f]{64}$ ]]; then
    pass "the $tool pin is a 64-digit lowercase SHA-256"
  else
    OUT="pin: '$pin'"
    fail "the $tool pin is a 64-digit lowercase SHA-256" "bad pin"
  fi
done

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all install-pinned-tool checks passed"
