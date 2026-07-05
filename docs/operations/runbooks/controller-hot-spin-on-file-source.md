# Controller pod CPU pegged at 100% (file:// hot-spin)

The LLMKube controller-manager pod is consuming an entire CPU core continuously and emitting the same error log line every few milliseconds. Running this for hours can pin a kind/k3s cluster's only CPU at 200-300% (multiple cores), trigger liveness probe failures, and starve other reconcilers.

**Scope as of v0.9.0**: local sources are gated by the host-path allowlist (`modelSource.allowedHostPathRoots` / `--allowed-host-path-roots`, GHSA-jw3m-8q7m-f35r). A local source that is NOT inside an allowlisted root never reaches the fetch path; it fails fast with phase `Failed` and a `SourceNotAllowed` Degraded condition, and cannot hot-spin. On v0.9.0+ this runbook therefore only applies to a source that IS allowlisted but points at a file the controller pod cannot see.

## Trigger

One or more of:

- Alert: `ControllerHighCPU` on the LLMKube controller-manager Deployment (sustained > 80% CPU on a single replica for > 5 minutes)
- Operator notices: `kubectl --context <ctx> top pods -n llmkube-system` shows controller-manager CPU > 1000m steady
- Log signal: the controller-manager log shows the same `Reconciler error` repeating thousands of times per second:

  ```
  ERROR Reconciler error  "controller":"model","name":"<model-name>"
    "error":"failed to open local model file:
     open /Users/<host>/llmkube-models/<dir>/<file>.gguf: no such file or directory"
  ```

## Diagnose

1. **Confirm CPU is actually pinned and find the offending Model.**

   ```bash
   kubectl --context <ctx> -n llmkube-system top pod \
     -l control-plane=controller-manager
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     --tail=50 | grep -E "Reconciler error|failed to open"
   ```

   The log lines will name the offending Model (`"name":"<model-name>"`).

2. **Inspect the Model spec.**

   ```bash
   kubectl --context <ctx> get model <model-name> -o yaml
   ```

   Look at `spec.source`. The hot-spin reproduces specifically when:
   - `spec.source` is a `file://` URL OR an absolute path
   - On v0.9.0+: the path is inside a root listed in `modelSource.allowedHostPathRoots` (a non-allowlisted source is rejected up front and cannot hot-spin)
   - The path exists on the **host** that runs the metal-agent (or wherever you want it to run)
   - The path does NOT exist inside the controller-manager pod's filesystem

   This is the canonical Mac kind / k3s + out-of-cluster metal-agent topology.

   If instead the Model shows phase `Failed` with a `SourceNotAllowed` Degraded condition, the allowlist rejected the source and there is no hot-spin; this runbook does not apply. Either add the source's root to `modelSource.allowedHostPathRoots` (only if you trust its contents) or fix `spec.source`:

   ```bash
   kubectl --context <ctx> get model <model-name> \
     -o jsonpath='{.status.phase} {.status.conditions[?(@.type=="Degraded")].reason}{"\n"}'
   ```

3. **Confirm fix #405 is deployed.** Run `kubectl --context <ctx> -n llmkube-system get pods -o jsonpath='{.items[*].spec.containers[*].image}'` and confirm the controller image tag is at or after the version in which [PR #412](https://github.com/defilantech/LLMKube/pull/412) merged. If the image predates that PR, the fix is missing and the runbook below applies as historical context only; upgrade the chart instead.

## Mitigate (immediate, gets CPU off the floor)

1. **(v0.9.0+) Reconsider the allowlist entry.** The spinning source is by definition inside an allowlisted root that the controller pod cannot actually read; the allowlist entry may be wrong or overly broad. If the controller was never meant to serve this path, remove the root from `modelSource.allowedHostPathRoots` (or narrow it) and upgrade the Helm release: the Model then fails fast with `SourceNotAllowed` and stops retrying, which also gets CPU off the floor.

2. **Edit `spec.source` to remove the unreachable `file://` reference.** Two safe options:
   - Replace with the equivalent `https://huggingface.co/.../<file>.gguf` URL. The metal-agent uses `filepath.Base(source)` to compute the local path, so a HTTPS URL with the same filename results in the same on-disk lookup; no re-download.
   - Replace with a `pvc://<claim>/<path>.gguf` source backed by a PVC the controller pod CAN mount.

   ```bash
   kubectl --context <ctx> edit model <model-name>
   ```

3. **Verify CPU drops within 1-2 minutes.**

   ```bash
   kubectl --context <ctx> -n llmkube-system top pod \
     -l control-plane=controller-manager --no-headers
   ```

   Should fall to single-digit-percent CPU. If it does not, see "Resolve" below.

## Resolve (structural)

The fix landed in [PR #412](https://github.com/defilantech/LLMKube/pull/412): the Model controller now detects `fs.ErrNotExist` and `fs.ErrPermission` and returns `RequeueAfter: 5*time.Minute, nil` instead of an error. This stops the rate-limited workqueue from tight-retrying.

Since v0.9.0 the protection is two-layered: the host-path allowlist rejects non-allowlisted local sources outright (immediate `Failed` / `SourceNotAllowed`, no fetch attempt), and the #412 backoff covers the remaining case of an allowlisted source whose file is missing or unreadable from the controller pod.

If you are observing this on a controller image that includes the fix (post-#412):

1. **Confirm the failure is genuinely unrecoverable** (not a transient mount issue). Run the same `os.Stat` from inside the controller pod:

   ```bash
   kubectl --context <ctx> -n llmkube-system exec deploy/llmkube-controller-manager -- \
     ls -la "<path-from-spec.source>"
   ```

   Should return `No such file or directory`. If it returns a real listing, the fix is operating correctly and the file appeared after the controller decided to back off; force a reconcile by editing the Model with a noop annotation.

2. **If the file truly cannot be reached from the controller pod**, this is a topology problem (your model is hosted on a path only your metal-agent or other out-of-cluster runtime can read). Switch the `spec.source` to a scheme the controller can either (a) ignore (HTTP/HTTPS, deferred to the workload init container) or (b) read (PVC mounted into the controller's namespace).

## Verify

1. **Logs no longer spam.** No `failed to open local model file` entries in the last 5 minutes.

   ```bash
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     --since=5m | grep -c "failed to open local model file"
   ```

   Expect `0`.

2. **Model phase + condition reflect the new state.**

   ```bash
   kubectl --context <ctx> get model <model-name> \
     -o jsonpath='{.status.phase} {.status.conditions[?(@.type=="Degraded")].reason}{"\n"}'
   ```

   Either `Ready` (if you fixed the source), `Failed CopyFailed` with `RequeueAfter` showing 5 minutes between reconciles (if you left the Model in its broken state intentionally), or `Failed SourceNotAllowed` (if you removed the root from the allowlist; this state is stable and does not consume reconcile cycles).

3. **Controller CPU is sane.**

   ```bash
   kubectl --context <ctx> -n llmkube-system top pod \
     -l control-plane=controller-manager --no-headers
   ```

   Single-digit percent CPU is normal.

## Related

- Issue: [#405](https://github.com/defilantech/LLMKube/issues/405) (the bug report from a real-world incident)
- Fix: [PR #412](https://github.com/defilantech/LLMKube/pull/412)
- Allowlist: GHSA-jw3m-8q7m-f35r (v0.9.0) added `modelSource.allowedHostPathRoots` / `--allowed-host-path-roots`; empty by default, which disables local/hostPath sources entirely
- Companion runbook: `model-fetch-failure.md` (HTTPS source failures), `pvc-source-not-bound.md` (PVC source missing)
- Documentation: `Model.spec.source` GoDoc has the file:// caveat for hybrid topologies
