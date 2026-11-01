# Multi-tenancy with GPUQuota

GPUQuota is the LLMKube mechanism for enforcing GPU budgets across multiple
tenants (namespaces, teams, or label-scoped groups). It works as a validating
admission webhook on `InferenceService` creation and update, rejecting any
request that would exceed the declared quota.

## Prerequisites

GPUQuota enforcement is **off by default**. The `vinferenceservicequota`
validating webhook is rendered only when `multitenancy.enabled=true`
(`charts/llmkube/values.yaml`); on a default install (`enabled=false`) the
webhook is absent and **nothing is rejected**, even if you create GPUQuota
resources.

The GPUQuota reconciler, by contrast, is always registered. It still writes
`status.usedGPUCount`, `status.usedVRAMBytes`, and the
`llmkube_gpuquota_used_gpu_count`, `llmkube_gpuquota_used_vram_bytes`,
`llmkube_gpuquota_gpu_count_limit`, and `llmkube_gpuquota_vram_bytes_limit`
Prometheus gauges regardless of the flag. So on a default install you will see
usage populate and dashboards move, but no admission is being gated. Set
`multitenancy.enabled=true` before relying on enforcement.

Quota is **opt-in per namespace**, not default-deny: a namespace covered by no
GPUQuota has no cap at all. The webhook only denies against quotas whose
`namespaceRef` or `selector` matches the incoming InferenceService's namespace;
a namespace with no matching quota is admitted unconditionally.

## When to use

Use GPUQuota when you need to guarantee that one tenant cannot starve another
of GPU resources. Common scenarios:

- **Shared GPU cluster with multiple teams**: each team gets a hard cap on
  how many GPUs their InferenceServices can consume.
- **Priority-based scheduling**: high-priority workloads (e.g., production
  inference) are guaranteed admission even when the cluster is saturated,
  while low-priority workloads (e.g., batch fine-tuning) are denied first.
- **Cost control**: tie a quota to a `CostBudgetRef` so that dollar spend
  is capped in addition to GPU count.

## Scoping a quota

A GPUQuota must declare exactly one of `selector` or `namespaceRef`; they are
mutually exclusive, enforced by a CEL validation rule on the CRD.

An InferenceService carrying the `kueue.x-k8s.io/queue-name` label skips
GPUQuota accounting entirely: the webhook defers to Kueue's ClusterQueue for
that object. The intent is to avoid double-gating a service that Kueue
admits and then unsuspends. Note that the shipped `tenant` ClusterRole grants
`create` on InferenceService, so a tenant can set this label themselves to
opt out of GPUQuota gating.

Autoscaled services are not re-gated on HPA-driven scale-up: the HPA targets
the Deployment directly, so a scale event never re-enters the admission
webhook. The reconciler charges `maxReplicas` against the quota at admission
time to bound the worst-case headroom, but a service admitted at a lower
replica count can still scale up within that ceiling without a further quota
check. See #1311 for the separate tracking issue.

### Namespace-scoped quota

Pins the quota to a single namespace. Every InferenceService created in that
namespace is checked against this quota.

> **Quota placement:** the webhook lists quotas cluster-wide, so a quota can
> live in any namespace and still govern a tenant namespace. Prefer placing
> the GPUQuota in an admin-owned namespace rather than co-locating it in the
> tenant namespace. The shipped `tenant-admin` ClusterRole grants `delete` on
> `gpuquotas`; if it is RoleBound in the tenant namespace, a tenant admin can
> delete the quota governing them. An admin-owned namespace keeps the quota
> out of tenant-reachable RBAC.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: GPUQuota
metadata:
  name: team-alpha-quota
  namespace: team-alpha
spec:
  namespaceRef: team-alpha
  gpuCount: 4
  minPriority: normal
```

### Label-selector quota

Matches namespaces by label, allowing a single quota to cover multiple
namespaces. Useful for cross-namespace teams or environments.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: GPUQuota
metadata:
  name: staging-quota
  namespace: kube-system
spec:
  selector:
    matchLabels:
      environment: staging
  gpuCount: 8
  vramBytes: 17179869184  # illustrative value
```

## RBAC binding examples

The operator's `manager-role` ClusterRole includes `get;list;watch` on
`gpuquotas` and `get;patch;update` on `gpuquotas/status`. To let a team
manage their own quota, bind a Role to their namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gpuquota-manager
  namespace: team-alpha
rules:
- apiGroups: ["inference.llmkube.dev"]
  resources: ["gpuquotas"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: ["inference.llmkube.dev"]
  resources: ["gpuquotas/status"]
  verbs: ["get"]
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: team-alpha-gpuquota
  namespace: team-alpha
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: gpuquota-manager
subjects:
- kind: Group
  name: team-alpha
  apiGroup: rbac.authorization.k8s.io
```

For cluster-wide quota management (e.g., a platform team that sets quotas
for all namespaces), use a ClusterRole:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gpuquota-admin
rules:
- apiGroups: ["inference.llmkube.dev"]
  resources: ["gpuquotas"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["inference.llmkube.dev"]
  resources: ["gpuquotas/status"]
  verbs: ["get", "patch", "update"]
```

## InferenceService rights and Pod Security

GPUQuota limits how many GPUs a tenant's InferenceServices consume. It does
not limit what those InferenceServices run. Plan namespace isolation around
the following.

### What creating an InferenceService grants

The operator builds the inference pod from the InferenceService spec and
creates it with its own permissions. These spec fields reach the pod directly,
merged with the operator's own entries where both exist:

- `image`, `command` and `args`
- `env`, including `valueFrom.secretKeyRef`
- `nodeSelector`, `tolerations`, `affinity` and `runtimeClassName`
- `extraVolumes` and `extraVolumeMounts`
- `securityContext` (the inference container) and `podSecurityContext`

So `create` or `update` on InferenceServices in a namespace is equivalent to
creating a pod with an arbitrary spec there: any image and command, any
Secret in the namespace, `hostPath` volumes, `privileged: true`, and any node
the tolerations and selectors reach. The author's own pod RBAC is not
consulted. The spec has no ServiceAccount field, so inference pods run as the
namespace's `default` ServiceAccount; do not bind roles to that account in a
tenant namespace.

Model fields reach the same pods. A local `spec.source` (an absolute path or
`file://` URL) becomes a hostPath volume, within the limits of
`modelSource.allowedHostPathRoots` (below), and `spec.sourceSecretRef` names a
Secret in the namespace whose keys are all loaded into the model downloader
init container. Grant Model write access on the same terms as InferenceService
write access.

The chart's `tenant` and `tenant-admin` ClusterRoles (rendered when
`multitenancy.enabled=true`) grant `create`, `update` and `patch` on Models and
InferenceServices. Bind them only to principals you would trust to create pods
in that namespace, and only in namespaces that enforce a Pod Security level.

### Pod Security Admission levels

LLMKube does not filter the fields above. Kubernetes Pod Security Admission
applies to the pods the operator creates, so enforce a level on every
namespace that holds InferenceServices:

```bash
kubectl label namespace team-alpha \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/warn=restricted
```

`baseline` refuses `hostPath` volumes, host namespaces, `privileged: true`
and capabilities outside its default set. `restricted` also requires non-root
containers and allows no added capability except `NET_BIND_SERVICE`. Some
LLMKube features need a more permissive level:

| Feature | What the pod carries | Lowest level that admits it |
|---|---|---|
| InferenceService with the operator's default security contexts | seccomp `RuntimeDefault`, all capabilities dropped, no privilege escalation, but no `runAsNonRoot` | `baseline` |
| GPU request (`nvidia.com/gpu`, `amd.com/gpu`, `devic.es/dri-render`) | an extended resource request; the container is not privileged | no change: the level the rest of the pod needs |
| Model cache PVC with `modelCache.prep: always` (the default) | the `model-cache-prep` init container runs as uid 0 and adds `CHOWN` and `FOWNER` | `baseline` |
| Local model source (`file://` or an absolute path) | a hostPath volume the operator generates | `privileged` |
| `spec.multiNode` | `hostNetwork` and `hostIPC` on every member, plus `IPC_LOCK` when `rdmaResource` is set | `privileged` |

GPU device plugins and the NVIDIA GPU Operator run privileged pods in their
own namespaces under their own policy. That does not carry over to inference
namespaces.

Running a namespace at `restricted` takes all of the following:

- Each InferenceService sets `spec.podSecurityContext` with
  `runAsNonRoot: true`, a numeric `runAsUser` and
  `seccompProfile: RuntimeDefault`. A supplied context replaces the operator's
  default entirely, so restate `fsGroup` as well.
- The runtime image works when run as that uid.
- A service that sets `spec.securityContext` keeps
  `allowPrivilegeEscalation: false` and drops all capabilities, since the
  supplied context replaces the operator's default too.
- If any service in the namespace uses the model cache, the operator runs
  with `modelCache.prep: never` (controller flag `--model-cache-prep=never`).
  The setting is operator-wide and only safe on storage that applies the pod
  `fsGroup` itself.

[MODEL-CACHE.md, "PSA `restricted`"](../MODEL-CACHE.md#psa-restricted) has
the exact settings for the cache.

A namespace that serves local model sources or multi-node groups has to run at
`privileged`, where Pod Security Admission does not limit the fields above.
Keep InferenceService and Model rights in such a namespace to principals you
trust with privileged pods on its nodes. For node-local weights, a `pvc://`
source over a `local` PersistentVolume works under `baseline` (see the
[air-gapped guide](../site/guides/air-gapped.md)).

### What `allowedHostPathRoots` covers

`modelSource.allowedHostPathRoots` (controller flag `--allowed-host-path-roots`)
decides which absolute paths a Model's local `spec.source` may name, and so
which hostPath volume the operator generates for it. It does not inspect
`spec.extraVolumes`, which reach the pod unchanged. It is not a boundary
against an InferenceService author; Pod Security Admission is.

## ModelRouter budgets and caller identity

A ModelRouter's `spec.policy.budgets` caps tokens or dollars over a rolling
window, scoped to the whole router, to one rule, or to a team. They bound
different callers:

- A `router` budget applies to every request through the router.
- A `rule` budget applies to requests that match the rule. Matching reads the
  request's model name and headers the client sends, so a caller can influence
  which rule, and which rule budget, applies.
- A `team` budget is keyed on the value of a request header (`headerKey`,
  default `x-llmkube-team`). The router-proxy charges the request to that
  value's bucket when the header is present, and applies no team budget when it
  is absent.

The router-proxy does not authenticate callers on its inference listener, and
`spec.endpoint.apiKeySecretRef` is not supported on a ModelRouter. A caller
that omits the team header is held to no team budget; one that sends another
team's value is charged to that team; one that sends a new value gets a fresh
budget. On their own, team budgets are an accounting label for cooperative
callers, not an enforced limit.

To make team budgets hold on the default `dataPlane: Proxy`:

- Put an authenticating gateway in front of the router-proxy that sets the
  team header from the authenticated identity and overwrites any value the
  client sent. The [APISIX guide](../site/guides/apisix.md) covers per-client
  keys with `key-auth`; add the header for each consumer in the gateway.
- Use NetworkPolicy so that only the gateway can reach the router-proxy, and
  only the router-proxy (and the gateway, if it routes there) can reach the
  backend InferenceService Services. A ClusterIP Service is reachable from
  every pod by default, and a request sent straight to a backend Service is not
  charged to any router budget.

On `dataPlane: Gateway`, `spec.policy.auth.jwt` validates a JWT at the gateway
and copies a verified claim into a header (`policy.auth.jwt.headerKey`,
default `x-llmkube-team`) that team budgets can key on. The router-proxy does
not read `spec.policy.auth`.

## Cost-budget integration

The `costBudgetRef` field on `GPUQuotaSpec` references a `CostBudget` resource
(future CRD) that caps the dollar cost of GPU usage under the quota. When the
cost budget is exhausted, the webhook denies **all** admission requests
regardless of GPU headroom or priority. The cost budget is the highest-
priority gate.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: GPUQuota
metadata:
  name: team-alpha-quota
spec:
  namespaceRef: team-alpha
  gpuCount: 4
  costBudgetRef: team-alpha-budget
```

Cost budget enforcement is a documented follow-up; the field is present on the
CRD but the webhook currently passes `costBudgetBreached=false` to the
admission decision. When the `CostBudget` CRD ships, the webhook will query
it and enforce the cap.

## What happens at quota breach

When an InferenceService is denied by a GPUQuota, the validating webhook
returns an HTTP 403 with a message like:

```
GPUQuota "team-alpha-quota" denied: would exceed gpuCount 4 (current 4 + requested 1)
```

The denial reason encodes which rule was violated:

| Reason | Meaning |
|---|---|
| `cost budget "X" exhausted` | The referenced CostBudget has been exceeded. |
| `would exceed gpuCount N` | GPU count cap reached. |
| `would exceed vramBytes N` | VRAM cap reached (only when `vramBytes` is set on the quota). |
| `priority "X" below minimum "Y"` | The InferenceService's priority is lower than the quota's `minPriority`. |

Because the validating webhook is `sideEffects=None`, it cannot mutate the
GPUQuota status from the admission path. Denials are therefore recorded as a
Prometheus counter, `llmkube_gpuquota_admission_denials_total{gpuquota,namespace}`,
incremented on every rejection. The `admissionDenials` and `lastDenial` fields
on `GPUQuotaStatus` are reserved for a future reconciler-observed counter and
are not yet populated, so they read as zero. Denials are observable today via
the counter metric and the Grafana dashboard below.

## Webhook failure-policy behavior

The GPUQuota validating webhook is registered with `failurePolicy: Fail`:

```
+kubebuilder:webhook:path=/validate-inference-llmkube-dev-v1alpha1-inferenceservice-quota,
  mutating=false,failurePolicy=fail,sideEffects=None,
  groups=inference.llmkube.dev,resources=inferenceservices,
  verbs=create;update,versions=v1alpha1,
  name=vinferenceservicequota.inference.llmkube.dev,
  admissionReviewVersions=v1
```

This means:

- **If the webhook is reachable**, it evaluates the quota and returns allow or
deny. Deny blocks the InferenceService from being created or updated.
- **If the webhook is unreachable** (network partition, pod restart, etc.), the
API server rejects the request with a 503. The InferenceService is **not**
created; the cluster fails closed.
- **If the webhook returns an internal error** (e.g., it cannot read current
usage from the API server), it denies the request with a reason like
`failed to read current usage: ...`. This is a safe default: when in doubt,
block admission.

This is intentional. A quota that cannot be enforced is worse than no quota;
it gives a false sense of protection. If you need the cluster to remain
operational when the webhook is down, you would need to change the
`failurePolicy` to `Ignore` in the webhook configuration, but this is not
recommended for production multi-tenant clusters.

## Priority ordering

The `minPriority` field on a quota enforces a minimum scheduling priority.
Priorities are ordered from highest to lowest:

1. `critical`: highest priority, always admitted if GPU headroom exists
2. `high`
3. `normal`: default when no priority is set
4. `low`
5. `batch`: lowest priority, denied first when the quota is tight

An InferenceService with priority `low` will be denied by a quota with
`minPriority: normal`, even if GPU headroom exists. This allows operators to
reserve capacity for important workloads.

## Observability

The `llmkube-quota.json` Grafana dashboard (in `charts/llmkube/dashboards/`)
reads the
`llmkube_gpuquota_*` metrics emitted by the operator and visualizes:

- **GPU utilization per quota**: `llmkube_gpuquota_used_gpu_count /
  llmkube_gpuquota_gpu_count_limit`, the fraction of each quota's cap in use.
- **Used GPUs per quota**: `llmkube_gpuquota_used_gpu_count`, the absolute
  GPU count the reconciler aggregates from in-scope InferenceServices.
- **Admission denial rate**:
  `rate(llmkube_gpuquota_admission_denials_total[5m])`, denials per second
  over a 5-minute window.
- **Cumulative admission denials**:
  `llmkube_gpuquota_admission_denials_total`, the running denial total.

All series are labeled by `gpuquota` and `namespace`. Import the dashboard
from `charts/llmkube/dashboards/llmkube-quota.json` into your Grafana instance,
or ship it with the chart via `grafana.dashboards.enabled=true`; it reads
from the same Prometheus datasource used by the other LLMKube dashboards.

For GPU sharing modes (exclusive, partitioned, shared) and how they interact
with VRAM-based quota accounting, see [gpu-sharing.md](gpu-sharing.md).
