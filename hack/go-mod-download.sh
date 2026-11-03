#!/bin/sh
#
# Downloads Go modules, retrying only when go fails with a network error
# (#2380).
#
#   hack/go-mod-download.sh                      # go mod download
#   hack/go-mod-download.sh install <pkg>@<ver>  # go install <pkg>@<ver>
#
# proxy.golang.org and sum.golang.org now and then reset an HTTP/2 stream in
# the middle of a download:
#
#   reading https://sum.golang.org/tile/8/1/312: stream error: stream ID 51; INTERNAL_ERROR; received from peer
#
# About 11 CI jobs failed that way in the 30 days to 2026-11-03 (#2376), with
# nothing wrong in the change under test. Such a failure is retried, up to
# three attempts, 10s and then 20s apart.
#
# Only network errors are retried. A checksum mismatch, a module the proxy
# does not have (404 or 410), or a go.mod that does not parse fails on the
# first attempt with go's own message: another attempt would fail the same
# way, later. An error that matches neither list fails at once too, so a real
# problem is never held behind retries; a transient error in new wording fails
# the job as it did before this script, and belongs in the network list.
#
# POSIX sh: the Dockerfiles' golang:*-alpine builder stages run it, and they
# have no bash. Tested by scripts/go-mod-download_test.sh (make test-scripts).
set -u

attempts=3
delay=10

[ "$#" -gt 0 ] || set -- mod download

# Errors that fail the same way on every attempt. Checked first: a proxy's 404
# or 410 response can quote its own failed fetch, which reads like a network
# error on our side.
permanent='checksum mismatch|SECURITY ERROR|: 404 Not Found|: 410 Gone|missing go.sum entry|errors parsing go.mod'
# Transport failures between this host and the proxy or checksum database, in
# the wording of go's HTTP client.
network='stream error|http2: |connection reset by peer|dial tcp|i/o timeout|TLS handshake timeout|unexpected EOF|Client\.Timeout exceeded|context deadline exceeded|: (429|500|502|503|504) '

# say <level> <message>: an annotation on the run page under GitHub Actions,
# so a retry is never silent, and a plain line anywhere else.
say() {
	if [ "${GITHUB_ACTIONS:-}" = true ]; then
		printf '::%s::%s\n' "$1" "$2" >&2
	else
		printf 'go-mod-download: %s: %s\n' "$1" "$2" >&2
	fi
}

errlog="$(mktemp "${TMPDIR:-/tmp}/go-mod-download.XXXXXX")" || exit 1
trap 'rm -f "$errlog"' EXIT

attempt=1
while :; do
	if go "$@" 2>"$errlog"; then rc=0; else rc=$?; fi
	cat "$errlog" >&2
	[ "$rc" -eq 0 ] && exit 0

	if grep -Eq "$permanent" "$errlog" || ! grep -Eq "$network" "$errlog"; then
		say error "go $* failed with an error that is not a network error, so it is not retried"
		exit "$rc"
	fi
	if [ "$attempt" -ge "$attempts" ]; then
		say error "go $* failed with a network error on all $attempts attempts"
		exit "$rc"
	fi
	say warning "go $* failed with a network error (attempt $attempt of $attempts); retrying in ${delay}s"
	sleep "$delay"
	attempt=$((attempt + 1))
	delay=$((delay * 2))
done
