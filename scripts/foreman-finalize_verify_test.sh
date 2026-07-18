#!/usr/bin/env bash
#
# Regression test for foreman-finalize.sh's verify-verdict gate (#1150).
#
# Self-contained: stubs `kubectl` and `gh` on PATH and drives the real script
# through the preflight verify check, asserting each decision path. Does not
# touch the network or git remotes (the script dies at the verify check, or is
# asserted to have passed it, before any fetch).
#
# Usage: scripts/foreman-finalize_verify_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FINALIZE="$SCRIPT_DIR/foreman-finalize.sh"
[[ -x "$FINALIZE" || -f "$FINALIZE" ]] || { echo "cannot find foreman-finalize.sh"; exit 1; }

STUB="$(mktemp -d)"
trap 'rm -rf "$STUB"' EXIT

cat >"$STUB/gh" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"$STUB/kubectl" <<'SH'
#!/usr/bin/env bash
if [[ -n "${FAKE_VERDICT:-}" ]]; then
  cat <<JSON
{"items":[{"metadata":{"creationTimestamp":"2026-07-17T10:00:00Z"},"spec":{"kind":"verify","payload":{"branch":"foreman/t/issue-9"}},"status":{"verdict":"${FAKE_VERDICT}"}}]}
JSON
else
  echo '{"items":[]}'
fi
SH
chmod +x "$STUB/gh" "$STUB/kubectl"

fails=0
# check <desc> <grep-pattern> <FAKE_VERDICT> [extra script args...]
check() {
  local desc="$1" pat="$2" verdict="${3:-}"; shift 3
  local out
  out="$(FAKE_VERDICT="$verdict" PATH="$STUB:$PATH" bash "$FINALIZE" \
    --branch foreman/t/issue-9 --issue 9 "$@" 2>&1 || true)"
  if grep -qiE "$pat" <<<"$out"; then
    echo "ok   - $desc"
  else
    echo "FAIL - $desc (wanted /$pat/)"; echo "$out" | sed 's/^/       /' | head -4; fails=$((fails+1))
  fi
}

check "NO-GO verdict is refused"           "did not pass.*refusing"            NO-GO
check "no verify task is refused"          "no Foreman verify.*refusing"       ""
check "GATE-PASS passes the check"         "verify verdict.*GATE-PASS"         GATE-PASS
check "--force warns and skips"            "force set.*skipping"               NO-GO --force
check "--full-test defers to local gate"   "full-test set.*authoritative gate" NO-GO --full-test

if ((fails)); then
  echo "FAILED: $fails case(s)"; exit 1
fi
echo "PASS: all verify-gate cases"
