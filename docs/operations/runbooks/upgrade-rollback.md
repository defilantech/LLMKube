# Upgrade and rollback (Helm-based)

The structural runbook for moving an LLMKube install from one minor or patch version to another, and for rolling back if the upgrade goes wrong. Covers the supported in-place Helm upgrade path; cluster-replacement upgrades (uninstall + reinstall) are out of scope here.

## When to use this

- Standard quarterly version bump on a production cluster
- Picking up a CVE-fix patch release
- Moving from one minor to the next (e.g., `0.7.x` → `0.8.x`); read the release notes first for breaking changes
- Recovery: a previously-attempted upgrade left the cluster in a bad state and you need to roll back

## Pre-flight (run before any upgrade)

1. **Read the release notes for the target version.** Specifically look for `BREAKING:` entries, CRD field removals, deprecations, and known-issue callouts.

   ```bash
   gh release view v<target-version> --repo defilantech/LLMKube
   ```

2. **Snapshot the current state.**

   ```bash
   # Current chart version + values
   helm get values llmkube -n llmkube-system > /tmp/llmkube-values-pre.yaml
   helm get manifest llmkube -n llmkube-system > /tmp/llmkube-manifest-pre.yaml
   helm history llmkube -n llmkube-system > /tmp/llmkube-history-pre.txt

   # Current InferenceServices, Models, and any custom CRDs
   kubectl get inferenceservices,models -A -o yaml > /tmp/llmkube-resources-pre.yaml

   # Current controller image + replicas (sanity)
   kubectl get deploy llmkube-controller-manager -n llmkube-system \
     -o jsonpath='{.spec.template.spec.containers[*].image}{"\n"}{.spec.replicas}{"\n"}'
   ```

3. **Confirm the operator is healthy now.** No point upgrading from a broken state; you cannot tell if the upgrade caused a regression.

   ```bash
   kubectl -n llmkube-system get pods
   kubectl -n llmkube-system logs deploy/llmkube-controller-manager --since=10m | grep -E "ERROR|panic"
   ```

   Should be `Running 1/1` with no recent errors.

4. **Verify the chart repo is current and the target version is published.**

   ```bash
   helm repo update llmkube
   helm search repo llmkube/llmkube --versions | head -10
   ```

5. **Decide on a maintenance window.** A standard upgrade rolls the controller-manager pod (~30 seconds of API unavailability for the LLMKube CRDs; reconcile pauses during the rollout). Inference pods are unaffected because the controller does not own them at runtime.

## Upgrade

### Standard in-place upgrade

```bash
# Dry-run first
helm upgrade llmkube llmkube/llmkube \
  -n llmkube-system \
  --version <target-version> \
  --values /tmp/llmkube-values-pre.yaml \
  --dry-run --debug | head -100

# Actual upgrade
helm upgrade llmkube llmkube/llmkube \
  -n llmkube-system \
  --version <target-version> \
  --values /tmp/llmkube-values-pre.yaml
```

The chart bundles the CRDs in `templates/crds/`, so CRD updates ride the chart upgrade. If the target release adds CRD fields, they appear automatically. If a release removes CRD fields (a breaking change called out in the release notes), the values consuming those fields must be updated separately before the upgrade.

### CRD-only upgrade (rare)

If the release notes call out an out-of-band CRD update (e.g., a hotfix that changes only the CRD), apply just the CRDs first, verify, then run the standard chart upgrade:

`raw.githubusercontent.com` serves files, not directories, so pointing `-f` at
`config/crd/bases/` returns a 404. Fetch the chart's CRD templates from the
release you are moving to and apply the ones you need:

```bash
helm pull llmkube/llmkube --version <target-version> --untar --untardir /tmp/llmkube-crds
kubectl apply --server-side -f /tmp/llmkube-crds/llmkube/templates/crds/
```

To pin a single CRD, apply it by name from the tag instead:

```bash
kubectl apply --server-side \
  -f https://raw.githubusercontent.com/defilantech/LLMKube/v<target>/config/crd/bases/inference.llmkube.dev_models.yaml
```

### Metal: chart-then-agent upgrade order

Clusters running Metal InferenceServices have a second moving part outside
Helm: the metal-agent on each Mac. Order matters between the two: upgrade
the chart (controller-manager) first, then the metal-agents.

The controller's per-InferenceService relay Deployments run the same
router-proxy image as `controllerManager.routerProxy.*`, and that image
version must match the controller version (the chart defaults already keep
them in step). A pinned router-proxy tag older than the controller's
release does not know the relay behavior the newer controller expects, and
the relay pod would crashloop.

If you roll the chart back (see Rollback below), roll the metal-agents
back first, then roll back the chart, same order reversed.

After either direction, check the Metal-related Events on the affected
InferenceServices:

```bash
kubectl describe inferenceservice <name> -n <namespace>
```

Look for `RelayCreated`, `ServiceAdopted`, and `RelayRemoved` (Normal), and
for `RelayReconcileFailed` or `InvalidAgentIngressPin` (Warning) if
something went wrong.

### Metal agent 0.10.1: security hardening

0.10.1 tightens the metal-agent's SSRF guard on model downloads, adds
`spec.sha256` verification, moves the model store and its ownership checks,
and locks down the client proxy and vllm-swift's `extraArgs`.

#### Upgrade blockers

Each of these stops an agent, or an InferenceService, after the upgrade until
it is fixed. Check them on every Mac before upgrading its metal-agent:

- **Re-render the launchd plist (required when upgrading from 0.10.0).** The
  0.10.0 plist passed `--model-store /tmp/llmkube-models` explicitly and
  logged to `/tmp/llmkube-metal-agent.log`. A binary swap followed by
  `launchctl kickstart -k` keeps that plist, and 0.10.1 refuses a model store
  under `/tmp`, so the agent will not start. Boot the job out and reinstall:

  ```bash
  launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
  while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
  make install-metal-agent
  launchctl print gui/$(id -u)/com.llmkube.metal-agent | grep 'state = running'
  ```

  `make install-metal-agent` alone is not enough while the job is loaded:
  launchd keeps the old definition until it is booted out. Wait for the
  unload before reinstalling: a `bootstrap` that runs too soon after
  `bootout` can fail with `Bootstrap failed: 5: Input/output error` and leave
  the agent unloaded. The last command prints `state = running` once the
  agent is back. The new plist uses
  the default store `~/Library/Application Support/llmkube/models` and logs to
  `~/Library/Logs/llmkube/metal-agent.log`. For a hand-maintained plist,
  remove the `--model-store /tmp/...` pair (or point it at a directory the
  agent owns outside `/tmp`), move `StandardOutPath` and `StandardErrorPath`
  out of `/tmp`, and set `WorkingDirectory` to the agent user's home. Nothing
  moves by itself.
- **Check the model store and every directory above it.** The agent refuses
  to start unless the store is owned by the agent's user and not group- or
  other-writable, and every ancestor up to `/` is owned by root or the
  agent's user and not group- or other-writable unless it has the sticky bit.
  A store on an exFAT or FAT volume (for example under `/Volumes`) always
  fails, because those volumes report every directory as mode 0777; use an
  APFS or HFS+ volume. Print each directory's owner and mode (works in `sh`,
  `bash` and `zsh`; set `STORE` to your `--model-store` if you set one):

  ```bash
  STORE="$HOME/Library/Application Support/llmkube/models"; p=$(cd "$STORE" 2>/dev/null && pwd -P || echo "$STORE"); while :; do ls -ld "$p" 2>/dev/null || echo "not created yet: $p"; [ "$p" = / ] && break; p=$(dirname "$p"); done
  ```

  Each line must show the agent's user or `root` as owner, and no group or
  other `w` unless the mode ends in `t`. The refusal names the path, owner
  and fix.
- **Replace symlinked cache slots.** Serving a hand-placed GGUF by putting it,
  or a symlink to it, at `<model-store>/<model>/<file>` with an `https`, `hf`
  or `s3` URL in `spec.source` now fails with `ModelSourceNotAllowed`. Set the
  Model's `spec.source` to the file's absolute path (or a `file://` URI) and
  add its directory to `--allowed-model-roots`.
- **Allowlist any LAN model mirror.** Every download (`http`, `https`, `hf`,
  `s3`, and the memory-check HEAD probe) now refuses a host that resolves to
  a private, loopback or link-local address unless it is listed in
  `--allowed-download-hosts`. If a Model source points at an internal MinIO or
  registry, add its host or CIDR to that flag before upgrading, for example
  `--allowed-download-hosts=minio.lan,10.20.0.0/16`. A new agent binary also
  loses its macOS Local Network permission, so LAN downloads can fail with
  `no route to host` until access is granted again in System Settings,
  Privacy & Security, Local Network.
- **Audit vllm-swift `extraArgs` on every affected InferenceService.**
  `--trust-request-chat-template`, `--enable-prompt-embeds` and
  `--enable-mm-embeds` are now refused, as is pointing `--tokenizer`,
  `--hf-config-path`, `--generation-config` or a `--lora-modules` path at a
  Hugging Face repo id instead of a path already on disk:

  ```bash
  kubectl get inferenceservice -A -o json | jq -r '
    .items[] | select(.spec.extraArgs != null) |
    select(.spec.extraArgs | any(test(
      "trust-request-chat-template|enable-prompt-embeds|enable-mm-embeds"
    ))) | "\(.metadata.namespace)/\(.metadata.name)"'
  ```

  An InferenceService that trips one of these starts failing
  `ExtraArgsRejected` after the upgrade; fix its `extraArgs` or set
  `--allow-unsafe-extra-args` on that agent. See the "extraArgs typed
  allowlist" section in `deployment/macos/README.md` for the full policy.

After the plist is re-rendered, every model re-downloads once on first start:
the new default store is a directory the agent has never populated.

#### Behavior changes in 0.10.1

- The controller's GGUF metadata reads honor `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY` (and their lowercase forms), as the agent's downloads do. The
  target host is still checked against the allowlist when a proxy is used.
- A NAT64 address in `64:ff9b::/96` is judged by its embedded IPv4 address.
- Downloads follow at most 5 redirects (the agent allowed 10 before).
- The download dial timeout is 10 seconds (it was 30).
- Downloads use HTTP/1.1 only.
- `spec.sha256` is enforced for sources the agent downloads for llama-server
  (`http`, `https`, `hf`, `s3`). Local sources are not hashed, and the other
  Metal runtimes ignore it in 0.10.1.

#### Behavior changes in 0.10.4

- An `InferenceService` whose generated Deployment name is held by a
  Deployment the controller did not create is no longer overwritten or
  recreated by the controller: the reconcile refuses with phase `Failed` and
  `status.schedulingStatus: DeploymentNameTaken` (#2011). This only bites an
  install where a foreign object already holds the name: the Deployment
  create path has stamped the service's controller reference on every
  Deployment it created
  (`setControllerReferenceUnblocked(isvc, deployment, ...)` immediately
  before `r.Create` in `internal/controller/inferenceservice_controller.go`),
  verified in every tagged release from v0.2.0 through v0.10.3, so upgrade in
  place is unaffected. A service whose name is taken recovers by deleting or
  renaming the foreign object; see
  [`inferenceservice-name-taken-deployment.md`](./inferenceservice-name-taken-deployment.md).
  The same guard has covered the router-proxy objects and the managed HPA
  since 0.10.3.

## Verify the upgrade

1. **Controller pod replaced and Ready.**

   ```bash
   kubectl -n llmkube-system rollout status deploy/llmkube-controller-manager
   kubectl -n llmkube-system get pods -l control-plane=controller-manager
   ```

2. **Existing InferenceServices still in their previous phase** (no unintended Failed state caused by the upgrade).

   ```bash
   kubectl get inferenceservices -A -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase'
   ```

   Diff against `/tmp/llmkube-resources-pre.yaml` if you want to be exact.

3. **Reconcile loop is running and clean.**

   ```bash
   kubectl -n llmkube-system logs deploy/llmkube-controller-manager --since=2m \
     | grep -E "Reconciling|ERROR" | head -20
   ```

   Look for `Reconciling Model` lines without `ERROR` companions. The InferenceService controller does not log a matching `Reconciling` line, so judge it by the absence of errors and by the phases in step 2 rather than by a positive log signal.

4. **Functional smoke: deploy the smallest catalog model and hit the endpoint.**

   `llmkube deploy` takes a catalog id. `tinyllama-1.1b` is not one; run
   `llmkube catalog list` to see the 17 that ship. `llama-3.2-3b` is the
   smallest and is the cheapest smoke test:

   ```bash
   llmkube deploy llama-3.2-3b
   # wait for Ready, then (dots become dashes in the Service name):
   kubectl port-forward svc/llama-3-2-3b 8080:8080 &
   curl -s http://localhost:8080/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -d '{"model":"llama-3.2-3b","messages":[{"role":"user","content":"hi"}],"max_tokens":16}' \
     | jq '.choices[0].message.content'
   llmkube delete llama-3.2-3b
   ```

5. **Helm history reflects the new revision.**

   ```bash
   helm history llmkube -n llmkube-system | tail -3
   ```

   The newest revision is `STATUS=deployed` and points at the target version.

## Rollback

When the verification above fails, or when an issue appears within hours of the upgrade.

### Rollback path (chart-level, fast)

```bash
helm history llmkube -n llmkube-system

# Identify the previous successful revision number
helm rollback llmkube <previous-revision> -n llmkube-system
```

`helm rollback` reverts the chart's manifests but does NOT touch existing CRD instances or running InferenceService Deployments. Inference traffic is unaffected.

### When rollback is not enough

Some failure modes need extra cleanup:

- **CRD storage version regression** (the new minor changed the storage version and the rollback target predates that change): apply the older CRDs back from the git tag of the rollback target and run a `kubectl get inferenceservice -A` to confirm `spec` parses correctly.

  Same directory-URL caveat as above: pull the rollback target's chart and
  apply its CRD templates.

  ```bash
  helm pull llmkube/llmkube --version <rollback-target> --untar --untardir /tmp/llmkube-rollback-crds
  kubectl apply --server-side --force-conflicts \
    -f /tmp/llmkube-rollback-crds/llmkube/templates/crds/
  ```

- **Webhook config left from the new version**: if the new version installed a validating or mutating admission webhook the older version did not have, `helm rollback` may not remove the webhook config. Manually delete:

  ```bash
  kubectl get validatingwebhookconfigurations | grep llmkube
  kubectl get mutatingwebhookconfigurations | grep llmkube
  kubectl delete <webhookkind> <name>
  ```

  After removal, confirm InferenceService apply still succeeds without the webhook in path.

### Rollback verification

Same checks as the post-upgrade verification list. The Helm history should now show the rollback as the newest revision.

## Common upgrade pitfalls

1. **Skipped a minor.** Helm allows it, but breaking changes accumulate. Read the release notes for every minor between source and target, not just the target.

2. **Custom values diverged from chart defaults.** `helm get values` returns only your overrides; the chart may have changed defaults you implicitly depended on. Compare `helm show values llmkube/llmkube --version <target>` against your saved values file.

3. **Image pull credentials.** A new minor that bumps the controller image may need a fresh image pull secret if you mirror images to a private registry. Ensure the pull secret references the new image tag.

4. **Reconcile bursts during rollout.** When the new controller starts, it re-reconciles every existing InferenceService. On clusters with hundreds of services this can spike CPU on the controller pod for a minute or two. Expected; do not treat as regression.

## Related

- [`controller-hot-spin-on-file-source.md`](./controller-hot-spin-on-file-source.md): one specific failure mode that an upgrade could surface if a new release reintroduces the rate-limited tight-retry behavior fixed in [PR #412](https://github.com/defilantech/LLMKube/pull/412)
- [`metal-agent-memory-pressure.md`](./metal-agent-memory-pressure.md): metal-agent runs as a separate launchd process on Apple Silicon hosts and is upgraded independently of the chart
- Helm chart README: `charts/llmkube/README.md` for the Tested platforms section (when it lands)
- Release notes: `gh release list --repo defilantech/LLMKube --limit 10`
