# Issue #850: strict-taint node-local model caches

**Date:** 2026-07-14
**Status:** Design approved; spec under user review.
**Issue:** https://github.com/defilantech/LLMKube/issues/850

## Problem

A GPU node protected by a hard `NoSchedule` taint can run LLMKube inference
pods because the controller renders the matching GPU toleration. A dynamically
provisioned MicroK8s hostpath cache can still remain Pending: the external
provisioner creates an owner-less helper pod on the selected node without the
GPU toleration. LLMKube cannot add pod scheduling fields to a PVC or mutate a
third-party helper pod through that PVC.

There is a second, independent failure on heterogeneous multi-node clusters.
The default shared RWO cache binds to one node, so an inference pod selected for
a different GPU node gets a volume node-affinity conflict. The existing global
`modelCache.mode: perService` avoids that conflict when the storage provisioner
can create a volume on the serving node.

LLMKube already supports the strict-taint path needed by users who pre-provision
storage. `InferenceService.spec.modelCache.claimName` selects a pre-existing,
user-managed PVC for one service. The normal downloader init container writes a
remote Model into that claim, and the serving container reads the same claim.
Because both containers are in the inference pod, they use its existing node
selection and GPU toleration. For a model already staged in a PVC, a `pvc://`
Model source is the read-only alternative and `claimName` is intentionally
ignored.

The current documentation does not connect these facts into a complete
strict-`NoSchedule` workflow. It also contains stale statements that
`llmkube cache list` cannot inspect operator-managed per-service cache PVCs.

## Design

Make the existing Model cache guide on the public documentation site real and
use it as the user-facing source of truth. The guide will distinguish storage
placement from pod scheduling and document three complete topologies:

1. `shared` plus RWX storage for multi-node clusters where the cache must be
   reachable from every serving node.
2. Operator-managed `perService` RWO caches with a `WaitForFirstConsumer`
   provisioner whose volume-creation path can run on the selected node.
3. A pre-provisioned, node-aligned PVC selected with
   `spec.modelCache.claimName`, which is the LLMKube path for a strict-tainted
   GPU node when dynamic provisioning is unsuitable.

The strict-taint section will show a remote-URL `Model` and an
`InferenceService` that selects an existing cache claim. It will explain that
the downloader remains part of the inference pod; no separate staging Job is
required. A companion section will show when to use `pvc://` for weights that
are already staged.

Troubleshooting will separate the two observable failures:

- A Pending `hostpath-provisioner-<node>-*` helper with an untolerated taint is a
  provisioner limitation. Preserving strict `NoSchedule` requires a
  pre-provisioned/static claim, a provisioner whose helper path supports the
  taint, or a narrowly scoped cluster-level mutation policy. Merely tolerating
  the inference pod or using global `perService` mode cannot fix that helper.
- `volume node affinity conflict` with the shared RWO cache means the cache is
  bound to another node. Use shared RWX storage, an operator-managed
  `perService` cache on a compatible provisioner, or a node-aligned
  `claimName`.

`PreferNoSchedule` may be mentioned as an explicit relaxation, not as the
recommended solution or a requirement. The guide will not provide an
unverified third-party mutation-policy manifest.

## Documentation changes

| File | Change |
|------|--------|
| `docs/site/guides/model-cache.md` | Replace the stub with the public Model-cache guide, including strict-taint workflow and troubleshooting |
| `docs/site/_meta/nav.yaml` | Mark the Model cache guide as live by removing `status: stub` |
| `docs/MODEL-CACHE.md` | Align the repository guide with cache modes, `claimName`, strict-taint troubleshooting, and current cache inspection behavior |
| `docs/model-storage.md` | Correct stale cache-list text and add the external-helper caveat to the mode-selection guidance |
| `charts/llmkube/values.yaml` | Correct stale `cache list` comments and clarify the provisioner requirement for `perService` on hard-tainted nodes |

The public and repository guides may organize the material differently, but
they must describe the same behavior and recommendations.

## Validation

- Cross-check every behavioral statement against the current controller and
  CLI implementation:
  - `ModelCacheSpec.ClaimName` and user-owned claim handling;
  - `ensureModelCachePVC` shared/per-service behavior;
  - inference-pod node selection and GPU toleration rendering;
  - cache PVC discovery in `llmkube cache list`.
- Search the changed documentation for stale claims that per-service caches are
  not inspectable.
- Run formatting/lint/test gates required by the repository and record any
  documentation-only limitations explicitly in the PR checklist.
- Review the final diff for provider-specific claims that are not supported by
  the issue's observed evidence or an upstream source.

## Pull request

Use `.github/PULL_REQUEST_TEMPLATE.md` without removing its `What`, `Why`,
`How`, or checklist sections. The PR will include `Fixes #850` and disclose AI
assistance in the body as required by `CONTRIBUTING.md`. Commits will use a
conventional `docs:` subject and DCO sign-off.

## Non-goals

- Adding another InferenceService cache-selection API; `claimName` already
  provides the required per-service user-managed cache.
- Making LLMKube mutate or manage MicroK8s hostpath helper pods.
- Claiming that global `perService` mode alone fixes hard-taint provisioning.
- Changing the default cache mode.
- Implementing Model prefetch or a standalone staging Job; that belongs to
  issue #904.
- Adding generic Pending-PVC status conditions without a reliable way to
  identify the external helper-pod failure.
- Recommending that users weaken strict GPU isolation as the primary fix.
