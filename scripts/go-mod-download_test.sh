#!/usr/bin/env bash
#
# Regression test for hack/go-mod-download.sh (#2380), which runs a Go module
# download and retries it only when go fails with a network error.
#
# A fake `go` first on PATH fails a set number of times with error text taken
# from failed CI jobs and from real go output, then succeeds; a fake `sleep`
# records the backoff instead of waiting. The cases pin both halves of the
# contract: a transient proxy error is retried, up to three attempts, and a
# real go.mod/go.sum problem fails on the first attempt with go's own message.
# The last cases run the real go against an unreachable proxy and a local file
# proxy, so a change in go's own error text shows up here rather than in CI.
#
# Usage: scripts/go-mod-download_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
RETRY="$REPO_ROOT/hack/go-mod-download.sh"
[[ -f "$RETRY" ]] || { echo "FAIL - cannot find hack/go-mod-download.sh"; exit 1; }

REAL_GO="$(command -v go || true)"

WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin" "$WORK/realbin"
# Fails the first FAKE_GO_FAILS calls with FAKE_GO_ERROR on stderr and exit
# status FAKE_GO_EXIT, then succeeds. Every call appends its arguments to
# FAKE_GO_CALLS.
cat >"$WORK/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_GO_CALLS"
if (( $(wc -l <"$FAKE_GO_CALLS") <= FAKE_GO_FAILS )); then
  printf '%s\n' "$FAKE_GO_ERROR" >&2
  exit "${FAKE_GO_EXIT:-1}"
fi
echo "fake go ok: $*"
EOF
# Records the backoff instead of waiting.
cat >"$WORK/bin/sleep" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_SLEEPS"
EOF
# Counts calls, then hands them to the real go.
cat >"$WORK/realbin/go" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_GO_CALLS"
exec "$REAL_GO" "$@"
EOF
cp "$WORK/bin/sleep" "$WORK/realbin/sleep"
chmod +x "$WORK/bin/go" "$WORK/bin/sleep" "$WORK/realbin/go" "$WORK/realbin/sleep"

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -20
  fails=$((fails+1))
}

# collect: sets CALLS (how many times go ran) and SLEEPS (the backoffs, in
# seconds, space-separated) from the last run.
collect() {
  CALLS="$(wc -l <"$WORK/calls" | tr -d ' ')"
  SLEEPS="$(tr '\n' ' ' <"$WORK/sleeps" | sed 's/ $//')"
}

# run <fails> <error> [args...]: runs the script under the fake go, which fails
# <fails> times with <error> and then succeeds. Sets CODE, OUT, CALLS, SLEEPS.
# GITHUB_ACTIONS is cleared so the messages are the plain-text form; the
# annotation case sets it back.
run() {
  local n="$1" err="$2"
  shift 2
  : >"$WORK/calls"
  : >"$WORK/sleeps"
  OUT="$(PATH="$WORK/bin:$PATH" FAKE_GO_FAILS="$n" FAKE_GO_ERROR="$err" \
    FAKE_GO_CALLS="$WORK/calls" FAKE_SLEEPS="$WORK/sleeps" \
    GITHUB_ACTIONS="${GITHUB_ACTIONS_FOR_RUN:-}" sh "$RETRY" "$@" 2>&1)" && CODE=0 || CODE=$?
  collect
}

# Error lines from the failed CI jobs (#2380): sum.golang.org, a module zip
# from proxy.golang.org, and a tool zip from `go install` in the Makefile.
SUMDB_RESET='go: reading https://sum.golang.org/tile/8/1/312: stream error: stream ID 51; INTERNAL_ERROR; received from peer'
ZIP_RESET='go: github.com/klauspost/compress@v1.19.1: read "https://proxy.golang.org/github.com/klauspost/compress/@v/v1.19.1.zip": stream error: stream ID 1; INTERNAL_ERROR'
TOOL_RESET='go: sigs.k8s.io/kustomize/kustomize/v5@v5.8.3: read "https://proxy.golang.org/sigs.k8s.io/kustomize/kustomize/v5/@v/v5.8.3.zip": stream error: stream ID 9; INTERNAL_ERROR'

# Real go output for the failures that must not be retried.
CHECKSUM_MISMATCH='verifying golang.org/x/text@v0.21.0/go.mod: checksum mismatch
	downloaded: h1:4IBbMaMmOPCJ8SecivzSH54+73PCFmPWxNTLm+vZkEQ=
	go.sum:     h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=

SECURITY ERROR
This download does NOT match an earlier download recorded in go.sum.'
# A proxy 404 quotes the proxy's own failed fetch, which can read like a
# network error on our side; the 404 is what decides.
PROXY_404='go: example.com/missing@v1.0.0: reading https://proxy.golang.org/example.com/missing/@v/v1.0.0.mod: 404 Not Found
	server response:
	not found: example.com/missing@v1.0.0: unrecognized import path "example.com/missing": https fetch: Get "https://example.com/missing?go-get=1": dial tcp 93.184.215.14:443: i/o timeout'
GOMOD_PARSE='go: errors parsing go.mod:
/workspace/go.mod:5: unknown directive: bogus'

for err in "$SUMDB_RESET" "$ZIP_RESET" "$TOOL_RESET"; do
  name="a stream reset (${err:0:60}...) is retried and the second attempt succeeds"
  run 1 "$err"
  if [[ $CODE == 0 && $CALLS == 2 && $SLEEPS == "10" && "$OUT" == *"INTERNAL_ERROR"* \
        && "$OUT" == *"attempt 1 of 3"* && "$OUT" == *"fake go ok: mod download"* ]]; then
    pass "$name"
  else
    fail "$name" "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
  fi
done

run 2 "$SUMDB_RESET"
if [[ $CODE == 0 && $CALLS == 3 && $SLEEPS == "10 20" ]]; then
  pass "two stream resets in a row are retried with a doubling backoff and the third attempt succeeds"
else
  fail "two stream resets in a row are retried with a doubling backoff and the third attempt succeeds" \
    "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
fi

FAKE_GO_EXIT=1 run 99 "$ZIP_RESET"
if [[ $CODE == 1 && $CALLS == 3 && $SLEEPS == "10 20" && "$OUT" == *"stream error"* \
      && "$OUT" == *"network error on all 3 attempts"* && "$OUT" != *"fake go ok"* ]]; then
  pass "a stream reset on every attempt fails after exactly three attempts, with go's error shown"
else
  fail "a stream reset on every attempt fails after exactly three attempts, with go's error shown" \
    "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
fi

# Other transport failures between a runner and the proxy, in go's wording.
for err in \
  'go: golang.org/x/text@v0.21.0: Get "https://proxy.golang.org/golang.org/x/text/@v/v0.21.0.mod": dial tcp 127.0.0.1:9: connect: connection refused' \
  'go: golang.org/x/text@v0.21.0: Get "https://proxy.golang.org/golang.org/x/text/@v/v0.21.0.mod": read tcp 10.1.0.4:41234->142.250.72.17:443: read: connection reset by peer' \
  'go: golang.org/x/text@v0.21.0: Get "https://proxy.golang.org/golang.org/x/text/@v/v0.21.0.mod": net/http: TLS handshake timeout' \
  'go: golang.org/x/text@v0.21.0: read "https://proxy.golang.org/golang.org/x/text/@v/v0.21.0.zip": unexpected EOF' \
  'go: golang.org/x/text@v0.21.0: reading https://proxy.golang.org/golang.org/x/text/@v/v0.21.0.mod: 502 Bad Gateway' \
  'go: golang.org/x/text@v0.21.0: reading https://proxy.golang.org/golang.org/x/text/@v/v0.21.0.info: 503 Service Unavailable'; do
  name="a network error (${err##*: }) is retried"
  run 1 "$err"
  if [[ $CODE == 0 && $CALLS == 2 ]]; then
    pass "$name"
  else
    fail "$name" "exit $CODE, $CALLS go call(s)"
  fi
done

FAKE_GO_EXIT=1 run 99 "$CHECKSUM_MISMATCH"
if [[ $CODE == 1 && $CALLS == 1 && -z $SLEEPS && "$OUT" == *"SECURITY ERROR"* \
      && "$OUT" == *"not a network error"* ]]; then
  pass "a checksum mismatch fails on the first attempt, with go's error shown"
else
  fail "a checksum mismatch fails on the first attempt, with go's error shown" \
    "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
fi

run 99 "$PROXY_404"
if [[ $CODE != 0 && $CALLS == 1 && -z $SLEEPS && "$OUT" == *"404 Not Found"* ]]; then
  pass "a module the proxy does not have fails on the first attempt, even when its 404 quotes a network error"
else
  fail "a module the proxy does not have fails on the first attempt, even when its 404 quotes a network error" \
    "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
fi

run 99 "$SUMDB_RESET
$CHECKSUM_MISMATCH"
if [[ $CODE != 0 && $CALLS == 1 ]]; then
  pass "a checksum mismatch alongside a stream reset still fails on the first attempt"
else
  fail "a checksum mismatch alongside a stream reset still fails on the first attempt" "exit $CODE, $CALLS go call(s)"
fi

FAKE_GO_EXIT=2 run 99 "$GOMOD_PARSE"
if [[ $CODE == 2 && $CALLS == 1 && "$OUT" == *"unknown directive"* ]]; then
  pass "an error that is not a network error fails on the first attempt with go's exit status"
else
  fail "an error that is not a network error fails on the first attempt with go's exit status" \
    "exit $CODE, $CALLS go call(s)"
fi

run 0 ""
if [[ $CODE == 0 && $CALLS == 1 && "$(cat "$WORK/calls")" == "mod download" ]]; then
  pass "with no arguments it runs go mod download once"
else
  fail "with no arguments it runs go mod download once" "exit $CODE, calls [$(cat "$WORK/calls")]"
fi

run 1 "$TOOL_RESET" install golang.org/x/vuln/cmd/govulncheck@e672cbe8fbc8c76e28fcb6db0efd9ce354e0dac5
if [[ $CODE == 0 && $CALLS == 2 \
      && "$(sort -u "$WORK/calls")" == "install golang.org/x/vuln/cmd/govulncheck@e672cbe8fbc8c76e28fcb6db0efd9ce354e0dac5" ]]; then
  pass "with arguments it runs go with exactly those arguments on every attempt"
else
  fail "with arguments it runs go with exactly those arguments on every attempt" \
    "exit $CODE, calls [$(tr '\n' '|' <"$WORK/calls")]"
fi

GITHUB_ACTIONS_FOR_RUN=true run 1 "$SUMDB_RESET"
if [[ $CODE == 0 && "$OUT" == *"::warning::go mod download failed with a network error (attempt 1 of 3)"* ]]; then
  pass "under GitHub Actions a retry is a warning annotation, so it shows on the run page"
else
  fail "under GitHub Actions a retry is a warning annotation, so it shows on the run page" "exit $CODE"
fi

# The real go, so the patterns are checked against go's own wording. A module
# that requires one dependency, fetched through a proxy that refuses the
# connection, a file proxy whose go.mod does not match go.sum, and a file
# proxy that does not have the module.
if [[ -z "$REAL_GO" ]]; then
  echo "skip - go is not on PATH, so the real-go cases did not run"
else
  mod="$WORK/mod"
  mkdir -p "$mod" "$WORK/proxy/example.com/dep/@v" "$WORK/emptyproxy"
  printf 'module example.com/retrytest\n\ngo 1.21\n\nrequire example.com/dep v1.0.0\n' >"$mod/go.mod"
  printf 'example.com/dep v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n' >"$mod/go.sum"
  printf 'module example.com/dep\n' >"$WORK/proxy/example.com/dep/@v/v1.0.0.mod"
  printf '{"Version":"v1.0.0"}\n' >"$WORK/proxy/example.com/dep/@v/v1.0.0.info"

  # realrun <GOPROXY>: sets CODE, OUT, CALLS, SLEEPS. GOENV=off keeps a
  # developer's go env file out of it.
  realrun() {
    : >"$WORK/calls"
    : >"$WORK/sleeps"
    OUT="$(cd "$mod" && PATH="$WORK/realbin:$PATH" REAL_GO="$REAL_GO" \
      FAKE_GO_CALLS="$WORK/calls" FAKE_SLEEPS="$WORK/sleeps" GITHUB_ACTIONS='' \
      GOENV=off GOWORK=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-modcacherw \
      GOMODCACHE="$WORK/modcache" GOPROXY="$1" sh "$RETRY" 2>&1)" && CODE=0 || CODE=$?
    collect
  }

  realrun "http://127.0.0.1:9"
  if [[ $CODE != 0 && $CALLS == 3 && $SLEEPS == "10 20" && "$OUT" == *"connection refused"* ]]; then
    pass "real go: an unreachable proxy is retried three times, then fails with go's error"
  else
    fail "real go: an unreachable proxy is retried three times, then fails with go's error" \
      "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
  fi

  realrun "file://$WORK/proxy"
  if [[ $CODE != 0 && $CALLS == 1 && -z $SLEEPS && "$OUT" == *"checksum mismatch"* ]]; then
    pass "real go: a go.sum checksum mismatch fails on the first attempt"
  else
    fail "real go: a go.sum checksum mismatch fails on the first attempt" \
      "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
  fi

  realrun "file://$WORK/emptyproxy"
  if [[ $CODE != 0 && $CALLS == 1 && -z $SLEEPS && "$OUT" == *"example.com/dep@v1.0.0"* ]]; then
    pass "real go: a missing module fails on the first attempt"
  else
    fail "real go: a missing module fails on the first attempt" \
      "exit $CODE, $CALLS go call(s), sleeps [$SLEEPS]"
  fi
fi

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all go-mod-download checks passed"
