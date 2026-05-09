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

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/defilantech/LLMKube/v<target>/config/crd/bases/
```

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

   Look for `Reconciling Model` / `Reconciling InferenceService` lines without `ERROR` companions.

4. **Functional smoke: deploy a TinyLlama and hit the endpoint.**

   ```bash
   llmkube deploy tinyllama-1.1b
   # wait for Ready, then:
   kubectl port-forward svc/tinyllama-1.1b 8080:8080 &
   curl -s http://localhost:8080/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -d '{"model":"tinyllama-1.1b","messages":[{"role":"user","content":"hi"}],"max_tokens":16}' \
     | jq '.choices[0].message.content'
   llmkube delete tinyllama-1.1b
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

  ```bash
  kubectl apply --server-side --force-conflicts \
    -f https://raw.githubusercontent.com/defilantech/LLMKube/v<rollback-target>/config/crd/bases/
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
