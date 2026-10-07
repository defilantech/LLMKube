# Proposal: a shared Go verify-then-publish helper for the init image (#1981)

**Status:** proposal, awaiting a decision. This is the design spike scoped by
[#1981](https://github.com/defilantech/LLMKube/issues/1981); no code changes
accompany it.
**Related:** [#1965](https://github.com/defilantech/LLMKube/issues/1965) (init-container
`spec.sha256` verification), [#1971](https://github.com/defilantech/LLMKube/pull/1971)
(the implementation), [#1978](https://github.com/defilantech/LLMKube/issues/1978)
(multi-file digests), [#1979](https://github.com/defilantech/LLMKube/issues/1979)
(`pvc://` verification), [#1980](https://github.com/defilantech/LLMKube/issues/1980)
(shared stamp format).

## Recommendation

**Do not build the helper yet.** Close the gap that motivates it first: the
generated shell is exercised on GNU userland in CI (the tests self-skip without
`stat -c` and `sha256sum`), while the container that runs it is
`docker.io/curlimages/curl` on Alpine/busybox. Add an in-image test that runs
the generated command inside the real init image. Revisit extraction only if
the shell gates drift from the Go paths, or a fourth copy appears.

The decision gate to build the helper: two release cycles in which the shell
and Go implementations diverge in behavior, or the in-image test cannot be made
reliable across architectures.

## The three copies

The invariant "hash the assembled bytes, and only publish on a match" is
implemented three times:

| Site | Language | Operation | Persistence |
|---|---|---|---|
| `ModelReconciler.verifySHA256` (`internal/controller/model_controller.go`) | Go, controller side | hash the controller's own copy, compare, set `Status.SHA256` | status only |
| `MetalExecutor.verifyAndPublish` / `verifyCachedDigest` (`pkg/agent/executor.go`) | Go, metal agent | hash, rename, write `<file>.sha256` (digest+size+mtime since #1980) | sidecar on the agent's store |
| `sha256VerifyFns` / `sha256MultiFileFns` (`internal/controller/model_storage.go`) | shell, init container | hash the partial, write a digest-keyed rejection marker and a triple stamp, per file | sidecar plus marker on the cache PVC |

They share the core step, but they differ in defaults and lifetimes: the
controller has no marker (a wrong pin fails the Model and retries by requeue),
the agent has an in-process mismatch memo and no marker file, and the shell has
the marker file, the per-file loop, and the OnChange "keep the pinned copy"
branch. Extracting one function would either flatten those differences or grow
flags for each.

This is the same reasoning `AGENTS.md` records for the two runtime-arg
builders: "Do not refactor the two arg builders into one shared function; they
serve different runtimes and have different defaults. The parity test is the
contract, not shared code." The same applies here.

## Cost of building it

A Go helper shipped in the init image means:

- A `Dockerfile.init` (`FROM curlimages/curl` plus a static binary), or a
  from-scratch image that also carries `curl`.
- A Makefile target, a goreleaser entry, a multi-arch build, and a cosign
  signature, matching the existing component images.
- A new default for `--init-container-image` and the Helm value, and an
  air-gap mirroring update: every fleet that mirrors `curlimages/curl` must now
  mirror another image, or the downloader stops running.
- The shell still drives `curl`, so only the verify-and-publish step moves; the
  other ~80 lines of loop, resume, and revalidation stay shell.

That is a cross-cutting operational change for a step that is already covered
by behavioral tests and a byte-for-byte golden pin.

## The risk that actually matters

The generated script is only tested on the host's GNU tools. The
`requireInitShellEnvironment` guard skips the whole suite when `stat -c` is
absent, and macOS and CI's Ubuntu both satisfy it or skip; neither runs inside
`curlimages/curl`. The busybox `stat -c '%s %Y'`, `sha256sum`, `find`, `sed`,
and `mktemp` behavior the script relies on is assumed, not proven in the image
it ships in. A future base-image change (musl, a trimmed busybox, dropping an
applet) breaks every digest-pinned download without a test going red.

The cheapest fix is a CI job (or a `make test-init-image` target) that builds
the init image, generates a representative command, and runs it in a container
against a local range server, asserting the published bytes, stamp and marker.
That directly covers the real risk and needs no image of our own.

## Options

1. **Keep the shell; add an in-image test.** Low cost, no new image, closes the
   actual test gap. Recommended.
2. **Extract a `llmkube-model-verify` binary into a new init image.** Removes
   the shell hash/stamp/marker code, at the operational cost above. The parity
   risk moves from script-versus-Go to client-versus-server and needs its own
   test regardless.
3. **Do nothing.** Leaves the in-image behavior unproven, which is the one
   failure mode the current tests cannot catch.

## Out of scope

- Changing the marker or stamp names (settled by #1980).
- The multi-file digest loop and the `pvc://` verify path (#1978, #1979).
- Any change to `--init-container-image` before a decision on option 2.

## Decision

Take option 1. File a follow-up to add the in-image test, and keep this
proposal open only as the reference for the extraction option if the gate above
is ever met.
