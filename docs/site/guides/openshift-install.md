---
title: OpenShift / OKD / MicroShift install
description: Install LLMKube on OpenShift, OKD, or MicroShift. Covers the bundled values-openshift.yaml Helm preset, restricted-v2 SCC behavior, per-InferenceService fsGroup overrides, and the MicroShift CI parity LLMKube ships with.
---

# OpenShift / OKD / MicroShift install

LLMKube ships with first-class OpenShift support as of `v0.7.7`.
Every PR exercises a real MicroShift cluster in CI against the
`restricted-v2` SecurityContextConstraint, so the
SCC-compatibility path is regression-tested before it reaches you.

This guide covers the install for OpenShift Container Platform,
OKD, MicroShift, and any distribution that runs the standard SCC
admission controller with the `MustRunAs` fsGroup strategy.

## TL;DR

Use the bundled Helm preset:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm install llmkube llmkube/llmkube \
  -f charts/llmkube/values-openshift.yaml \
  -n llmkube-system --create-namespace
```

That single command produces an LLMKube install whose
`InferenceService` pods are admitted cleanly under `restricted-v2`.
The same preset works on OpenShift Container Platform, OKD,
MicroShift, and standalone OpenShift Local.

The rest of this page explains why the preset is needed and how to
adapt it for less common SCC configurations.

## Why the preset exists

The default LLMKube install (the one you'd get with plain
`helm install llmkube llmkube/llmkube`) sets a sensible `fsGroup`
default of `102` on every `InferenceService` pod. That value lines
up with the GID of the `curl_group` user in the default
init-container image (`docker.io/curlimages/curl:8.18.0`), which is
what lets the init container write the downloaded model into a
freshly-provisioned PVC.

On standard Kubernetes that's correct. On OpenShift's
`restricted-v2` SCC, it's not:

- `restricted-v2` enforces `MustRunAs` for `fsGroup`.
- The namespace carries an
  `openshift.io/sa.scc.supplemental-groups` annotation describing
  the GID range pods in that namespace may use.
- The SCC admission controller wants to inject an `fsGroup` from
  that range itself.
- If the pod spec already declares a different `fsGroup`, admission
  fails: `unable to validate against any security context constraint:
  fsGroup not within allowed range`.

LLMKube's default `fsGroup=102` is outside any namespace's allocated
range, so plain `helm install` produces pods that never get
scheduled.

## What the preset does

`charts/llmkube/values-openshift.yaml` is a handful of lines of
YAML, almost all of which are explanatory comments. The
load-bearing setting is:

```yaml
controllerManager:
  initContainer:
    defaultFSGroup: 0
```

`0` is a sentinel meaning "don't set fsGroup at all" — the operator
emits InferenceService pods without an `fsGroup` in their
`podSecurityContext`, and the SCC admission controller injects the
right value from the namespace's allocated range.

CI coverage: the `test-e2e-openshift` job in
`.github/workflows/test-e2e.yml` runs the full operator suite
against a MicroShift cluster via MINC on every PR. The job is
currently best-effort (`continue-on-error: true`) while the
MicroShift-in-CI setup is hardened, but the kind merge gate plus
the OpenShift parity job catch the same admission regressions.

## Step-by-step install

### 1. Create the namespace

OpenShift will compute the supplemental-groups range when the
namespace is created. Both `oc` and `kubectl` work:

```bash
oc create namespace llmkube-system
oc get namespace llmkube-system -o jsonpath='{.metadata.annotations.openshift\.io/sa\.scc\.supplemental-groups}'
# example: 1000680000/10000
```

The range is the operator's signal that `restricted-v2` is in
effect. If the annotation is empty you're on plain Kubernetes; use
the standard install instead.

### 2. Install LLMKube with the preset

If you cloned the operator repo:

```bash
helm install llmkube ./charts/llmkube \
  -f charts/llmkube/values-openshift.yaml \
  -n llmkube-system
```

If you're installing from the Helm repo:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm pull llmkube/llmkube --untar
helm install llmkube ./llmkube \
  -f ./llmkube/values-openshift.yaml \
  -n llmkube-system
```

### 3. Verify the controller is up

```bash
oc -n llmkube-system get pods
# expect: llmkube-controller-manager-...   1/1   Running

# Confirm the preset took: the rendered Deployment passes
# --default-fsgroup=0 in the manager container's args.
oc -n llmkube-system get deployment llmkube-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].args}' \
  | tr ',' '\n' | grep default-fsgroup
# expect: "--default-fsgroup=0"
```

### 4. Deploy a model

Apply any standard `Model` + `InferenceService`. The pod will be
admitted by `restricted-v2` with the SCC's chosen `fsGroup`:

```bash
oc apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: { name: phi-4-mini }
spec:
  source: https://huggingface.co/bartowski/microsoft_Phi-4-mini-instruct-GGUF/resolve/main/microsoft_Phi-4-mini-instruct-Q4_K_M.gguf
  format: gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: phi-4-mini }
spec:
  modelRef: phi-4-mini
EOF
```

Verify the pod's effective security context shows an fsGroup that
falls inside your namespace's allocated range:

```bash
oc -n default get pod -l app=phi-4-mini -o jsonpath='{.items[0].spec.securityContext.fsGroup}'
# example: 1000680000  (first value from the supplemental-groups annotation)
```

## Single-tenant escape hatch

If you'd rather pin `fsGroup` per workload than disable the
operator-wide default, omit the preset and set the value directly
on each `InferenceService`:

```bash
oc get namespace default -o jsonpath='{.metadata.annotations.openshift\.io/sa\.scc\.supplemental-groups}'
# example: 1000680000/10000
```

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: phi-4-mini }
spec:
  modelRef: phi-4-mini
  podSecurityContext:
    fsGroup: 1000680000   # first value from the command above
```

That works on a single namespace where you control every
deployment. The operator-wide preset is preferred for multi-tenant
or platform-team-managed clusters.

## ModelRouter on OpenShift

The same preset covers the router-proxy pods that the
`ModelRouter` reconciler creates. The router-proxy already runs as
a non-root user (UID 65532), so `restricted-v2` admits it cleanly
once the preset is in place.

The proxy has no model-weight init container; it doesn't need
`fsGroup` at all. The preset's `defaultFSGroup: 0` flows through to
the router-proxy pod spec the same way as InferenceService pods —
SCC injects from the namespace range or skips fsGroup entirely if
the pod has no writable volumes that need group ownership.

If your OpenShift cluster requires `imagePullSecrets` on every
namespace (a common pattern with private mirror registries), add
them to the `ModelRouter` spec.proxy block as well:

```yaml
spec:
  proxy:
    imagePullSecrets:
      - name: internal-registry-creds
```

## Air-gapped OpenShift

For OpenShift installs without public-internet egress, combine this
guide with the [`Air-gapped install`](./air-gapped) walkthrough.
The Helm install line becomes:

```bash
helm install llmkube ./llmkube \
  -f ./llmkube/values-openshift.yaml \
  -n llmkube-system --create-namespace \
  --set controllerManager.image.repository=registry.internal.corp/defilantech/llmkube-controller \
  --set controllerManager.image.tag=v0.7.7 \
  --set controllerManager.initContainer.repository=registry.internal.corp/curlimages/curl \
  --set controllerManager.initContainer.tag=8.18.0
```

Both presets are additive: the OpenShift values handle SCC, the
extra `--set` flags handle the private registry. Note that the
init container is configured via `repository` + `tag` separately,
not a single `image` field.

## Verification

End-to-end smoke test that exercises the SCC admission path:

```bash
oc create namespace e2e-scc
oc apply -n e2e-scc -f config/samples/inference_v1alpha1_model.yaml
oc apply -n e2e-scc -f config/samples/inference_v1alpha1_inferenceservice.yaml
oc -n e2e-scc wait inferenceservice/phi-3-inference \
  --for=jsonpath='{.status.phase}'=Ready --timeout=5m

# Confirm the SCC injected an fsGroup from the namespace range
oc -n e2e-scc get pod -l inference.llmkube.dev/service=phi-3-inference \
  -o jsonpath='{.items[0].spec.securityContext.fsGroup}'
```

If the InferenceService reaches `Ready` and the pod's `fsGroup`
falls inside the namespace's supplemental-groups annotation, the
preset is working as designed.

## Troubleshooting

**`Pod ... is forbidden: unable to validate against any security context constraint`**
Your install didn't pick up the preset. Confirm with
`helm get values llmkube -n llmkube-system` that
`controllerManager.initContainer.defaultFSGroup` is `0`. Rerun
`helm upgrade` with `-f values-openshift.yaml --reuse-values`.

**`fsGroup not within allowed range`**
A per-InferenceService `podSecurityContext.fsGroup` is set but
falls outside the namespace's supplemental-groups range. Either
remove it (the SCC will inject) or set it to the first value in
the namespace's range.

**Controller pod itself won't start with `restricted-v2`**
LLMKube's controller manager runs as UID 65532 with no
`fsGroup` declaration, which is SCC-clean. If it's still failing,
check whether your cluster uses a more restrictive SCC than
`restricted-v2` (some enterprises ship custom SCCs that further
constrain seccomp profiles). Override
`controllerManager.podSecurityContext` in your Helm values.

**MicroShift on a single-node edge box**
Same preset. The MicroShift CI lane in this repo uses MINC
(MicroShift in Container) with the standard `restricted-v2` SCC,
and that's the same SCC shape you get on real MicroShift hosts.

## Next steps

- [Air-gapped install](./air-gapped) if you're combining OpenShift
  with restricted-egress requirements
- [Model Router](../concepts/model-router) for the policy-aware
  routing layer; the same SCC preset covers its proxy pods
- [Multi-GPU sharding](./multi-gpu) for 70B+ models on
  OpenShift-managed NVIDIA nodes

## Reference

- [`charts/llmkube/values-openshift.yaml`](https://github.com/defilantech/LLMKube/blob/main/charts/llmkube/values-openshift.yaml) — the preset itself
- [CRD reference](../concepts/crds) — `Model`, `InferenceService`, `ModelRouter`
- OpenShift docs: [restricted-v2 SCC](https://docs.openshift.com/container-platform/latest/authentication/managing-security-context-constraints.html)
