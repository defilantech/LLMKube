#!/usr/bin/env bash
#
# Regression test for scripts/check-docs-links.py, which checks the links and
# the markup of docs/site before llmkube.com builds them.
#
# testdata/docs-links/good is a docs tree that must pass: links resolved the
# way the site resolves them, anchors, repo links, a stub and a `source: web`
# nav entry, and "<", braces and <name> where they are code. bad/ holds one
# case per check and must fail with each message. site/ is a built site with
# one missing page and two missing anchors for --site.
#
# Usage: scripts/check-docs-links_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-docs-links.py"
DATA="$SCRIPT_DIR/testdata/docs-links"

fail=0
pass() { echo "PASS - $1"; }
bad() { echo "FAIL - $1"; fail=1; }

if out="$(python3 "$CHECK" "$DATA/good" 2>&1)"; then
  pass "a clean docs tree passes"
else
  bad "a clean docs tree fails:"$'\n'"$out"
fi

if out="$(python3 "$CHECK" "$DATA/bad" 2>&1)"; then
  bad "a docs tree with broken links passes"
else
  pass "a docs tree with broken links fails"
fi
expect() {
  if grep -qF -- "$2" <<<"$out"; then pass "$1"; else bad "$1: no line containing: $2"$'\n'"$out"; fi
}
expect "a page not in nav.yaml" "/docs/guides/nope is not in nav.yaml"
expect "a missing anchor on another page" "no heading #step-three on /docs/guides/guide"
expect "a link to a .md file" "./guides/guide.md: links to a .md file"
expect "a missing anchor on the same page" "no heading #nowhere on this page"
expect "a repo file that does not exist" "no/such/file.md does not exist on main"
expect "a line that starts with <name>" "getting-started.md:10: <name> starts a line"
expect "a bare < in text" "getting-started.md:12: bare '<' outside code"
expect "a bare < between code spans" "getting-started.md:14: bare '<' outside code"
expect "a brace in text" "getting-started.md:16: brace outside code"
n="$(grep -c '^docs/site/' <<<"$out" || true)"
[[ "$n" == 9 ]] && pass "exactly the nine planted problems" || bad "expected 9 problems, got $n"

if out="$(python3 "$CHECK" --site "$DATA/site" "$DATA/good" 2>&1)"; then
  bad "a built site with broken links passes"
else
  expect "a missing built page" "/: /docs/missing: no page /docs/missing"
  expect "a missing anchor on another built page" "/: /docs/a#gone: no element #gone on /docs/a"
  expect "a missing anchor on the same built page" "/docs/a: #nope: no element #nope on this page"
  n="$(grep -c ': no ' <<<"$out" || true)"
  [[ "$n" == 3 ]] && pass "exactly the three planted site problems" || bad "expected 3 site problems, got $n"
fi

exit "$fail"
