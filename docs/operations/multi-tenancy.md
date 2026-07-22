# Multi-tenancy with GPUQuota

GPUQuota is the LLMKube mechanism for enforcing GPU budgets across multiple
tenants (namespaces, teams, or label-scoped groups). It works as a validating
admission webhook on `InferenceService` creation and update, rejecting any
request that would exceed the declared quota.

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

### Namespace-scoped quota

Pins the quota to a single namespace. Every InferenceService created in that
namespace is checked against this quota.

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

The `llmkube-quota.json` Grafana dashboard (in `docs/grafana/`) reads the
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
from `docs/grafana/llmkube-quota.json` into your Grafana instance; it reads
from the same Prometheus datasource used by the other LLMKube dashboards.

For GPU sharing modes (exclusive, partitioned, shared) and how they interact
with VRAM-based quota accounting, see [gpu-sharing.md](gpu-sharing.md).
