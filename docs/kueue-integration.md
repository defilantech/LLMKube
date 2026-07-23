# Suspend and the Kueue integration

## spec.suspend

Setting `spec.suspend: true` on an InferenceService stops its workload
without losing the desired scale: the serving Deployment (or metal-agent
process) is scaled to zero, `spec.replicas` is preserved, the service
reports `phase: Suspended`, and a `Suspended` condition is set. If
autoscaling is configured, the HPA is removed while suspended and recreated
on resume. Setting `spec.suspend: false` restores the service to
`spec.replicas`.

Suspend is the admission lever used by external queueing controllers. It is
also useful on its own for temporarily parking a service without recording
its replica count somewhere else.

## GPUQuota and Kueue

LLMKube's built-in `GPUQuota` admission stays the default for clusters
without Kueue. When an InferenceService carries the
`kueue.x-k8s.io/queue-name` label, the GPUQuota webhook defers: quota
admission for that service is owned by Kueue's ClusterQueue (via the
[llmkube-kueue](https://github.com/defilantech/llmkube-kueue) integration),
and LLMKube does not double-gate it. Spec validation (for example
`gpuSharing` rules) still applies to every service. `GPUQuota` status
continues to report usage from all services, labeled or not, for
observability.

Removing the label opts the service back into GPUQuota gating.

Tracking: [#1253](https://github.com/defilantech/LLMKube/issues/1253)
(epic), [#1251](https://github.com/defilantech/LLMKube/issues/1251)
(this slice), [#1252](https://github.com/defilantech/LLMKube/issues/1252)
(the external component).

### Notes

The queue-name label is effectively a quota bypass for GPUQuota: any
service that carries it skips GPUQuota admission entirely. Controlling who
may set labels on InferenceServices, through RBAC, is the actual guard
against misuse; the label itself carries no authorization check.

In a namespace that mixes labeled and unlabeled services, GPUQuota status
usage aggregates both, by design, for observability. This means
`status.usedGPUCount` can exceed the quota limit without any violation
being recorded, since the labeled services were never subject to the
limit. Unlabeled services are still admitted against that same aggregate
count, so a labeled service's usage can push an unlabeled service's
admission request over the limit and cause it to be denied.
