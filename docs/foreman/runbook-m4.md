# Foreman M4 runbook: V3 two-step pipeline

The M4 acceptance test, end to end: a real issue-fix `AgenticTask`
on the M5 Max produces a branch on the LLMKube fork, and an
ordering-dependent gate `AgenticTask` on a verifier node verifies it via
`make fmt vet lint test` in a Kubernetes Job.

If you have not yet installed Foreman on a verifier node, read
[`install-verifier-node.md`](./install-verifier-node.md) first.

## What this verifies

- Pipeline ordering: `gate-N` blocks on `code-N` via `dependsOn`.
- Capability-aware dispatch: `code-N` lands on metal, `gate-N` lands
  on cuda+verifier.
- The deterministic executor branch (M4 Phase A): `gate-N`'s Agent
  has empty `inferenceServiceRef`, so the foreman-agent skips the
  LLM loop and dispatches `run_gate_job` directly.
- The gate Job (M4 Phase B): submitted into `foreman-system`, clones
  the branch, runs the gate check suite, exits 0 = `GATE-PASS` or
  non-zero = `GATE-FAIL`.
- Verdict mapping (M4 Phase A): the Job's terminal status flows back
  into `AgenticTask.status.verdict` as `GATE-PASS` / `GATE-FAIL`
  with a 32 KiB log tail in `.status.result.extra.logTail`.

## Prerequisites

- M3 coder Agent installed on the M5 Max. Confirm it's `Ready`:
  ```sh
  kubectl get agent qwen36-35b-carnice-mtp-coder
  ```
- M4 gate Agent installed where the AgenticTasks live (the
  scheduler resolves AgentRef in the task's namespace; the gate Job
  itself lands in `foreman-system` regardless):
  ```sh
  kubectl apply -f config/foreman/agents/gate.yaml
  ```
- Both foreman-agents up, registered, advertising the right roles:
  ```sh
  kubectl get fleetnodes -o custom-columns=NAME:.metadata.name,READY:.status.phase,ACC:.status.accelerator,ROLES:.spec.roles
  ```
  Expected:
  ```
  NAME             READY   ACC     ROLES
  <verifier-node>    Ready   cuda    [worker verifier]
  m5max-…          Ready   metal   [worker]
  ```

## Step 1 :: pick an issue + apply the demo manifest

```sh
# Edit examples/foreman/m4-two-step-demo.yaml: bump the `issue:`
# field on both tasks, the task `name:` (code-N + gate-N), and the
# `payload.branch:` to a fresh `foreman/issue-N` slug.

kubectl apply -f examples/foreman/m4-two-step-demo.yaml
```

Both tasks land in `Pending`. The scheduler picks them up on the
next reconcile.

## Step 2 :: watch code-N run

```sh
kubectl get agentictasks -w
```

Expected sequence (timestamps from the M3 baseline):

```
code-503   Pending     <none>           1s
code-503   Scheduled   m5max-…          2s
code-503   Running     m5max-…          3s
gate-503   Pending     <none>          (held by dependsOn)
code-503   Succeeded   m5max-…    GO   ~76s
```

The M5 Max's foreman-agent log streams the OAI turns during this
window:

```sh
kubectl -n foreman-system logs \
  -l app.kubernetes.io/component=agent --tail=200 -f
```

(If running the coder locally rather than as the in-cluster Pod,
tail `~/Library/Logs/foreman-agent.log` instead.)

## Step 3 :: watch gate-N pick up + the Job land

The moment `code-503` reaches `Succeeded`, the controller marks
`gate-503` schedulable. The the verifier-node foreman-agent claims it and
calls `run_gate_job`; that submits a Job in `foreman-system`.

```sh
# Watch the AgenticTask:
kubectl get agentictask gate-503 -o jsonpath='{.status.phase}{"\t"}{.status.assignedNode}{"\n"}'

# Watch the underlying Job:
kubectl -n foreman-system get jobs -w
```

Expected:

```
foreman-gate-gate-503-…   0/1 Running       0s
foreman-gate-gate-503-…   1/1 Completed   ~2-3 min (warm) or ~10 min (cold)
```

## Step 4 :: inspect the verdict + log tail

```sh
kubectl get agentictask gate-503 -o yaml | yq '.status'
```

Expected `.status` shape:

```yaml
phase: Succeeded
verdict: GATE-PASS
assignedNode: <verifier-node>
result:
  kind: native-agent-loop
  verdict: GATE-PASS
  summary: all gate checks passed
  extra:
    deterministic: true
    dispatchedTool: run_gate_job
    toolOutput:
      jobName: foreman-gate-gate-503-…
      namespace: foreman-system
      repo: defilantech/LLMKube
      branch: foreman/issue-503
    modelExtra:
      jobName: foreman-gate-gate-503-…
      logTail: |
        === make fmt ===
        ...
        === make test ===
        ok  github.com/defilantech/llmkube/...  6.5s
        GATE PASS
```

The `logTail` carries the last 32 KiB of the gate Pod's log so a
human reviewer can scan the failure mode without `kubectl exec`-ing.

## Step 5 :: optional, the GATE-FAIL case

Reproduce by re-running on a known-broken branch:

```sh
# Edit examples/foreman/m4-two-step-demo.yaml to point at a branch
# that fails one of the checks (e.g. a deliberately unformatted
# file). Re-apply both tasks under fresh names.

kubectl apply -f examples/foreman/m4-two-step-demo.yaml
```

Expected:

```yaml
phase: Succeeded
verdict: GATE-FAIL
result:
  verdict: GATE-FAIL
  summary: one or more gate checks failed
  extra:
    modelExtra:
      logTail: |
        === make fmt ===
        - some/file.go
        GATE FAIL: tree dirty after checks (unformatted or stale codegen)
```

`AgenticTask.status.phase` is still `Succeeded` -- the task
executed cleanly, the gate just *reported* a failure. `verdict`
is the field downstream consumers should pivot on, not `phase`.

## Troubleshooting

**gate-N stays Pending forever** :: the scheduler matches Roles
strictly. Confirm the verifier FleetNode actually advertises
`verifier`:
```sh
kubectl get fleetnode -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.roles}{"\n"}{end}'
```
If the role list is missing, re-run the Helm install with the
`agent.roles={worker,verifier}` override per
[`install-verifier-node.md`](./install-verifier-node.md).

**Job submits but never picks up** :: the gate-cache PVC is stuck
Pending. Run `kubectl -n foreman-system
describe pvc foreman-gate-cache` for the events. Common causes:
no default StorageClass, or no node can satisfy ReadWriteOnce.
Workarounds: `--set agent.gateCache.storageClass=...` or disable
the cache via `--set agent.gateCache.enabled=false`.

**Verdict GATE-ERROR with "create job: forbidden"** :: the agent
ServiceAccount is missing `create` on `batch/jobs` in
`foreman-system`. Re-apply the chart's RBAC:
```sh
helm upgrade --reuse-values foreman \
  defilantech/foreman -n foreman-system
```

**Verdict GATE-ERROR with "poll timeout"** :: the Job ran past the
PollTimeout (default 2 * activeDeadlineSeconds = 60 min). Either
the gate Job is genuinely slow (cold cache + 8Gi RAM ceiling can
push past 30 min) or the apiserver is dropping watches. Pull the
gate Pod's log directly:
```sh
kubectl -n foreman-system logs \
  -l job-name=foreman-gate-gate-503-…
```

## What this does NOT cover

- The reviewer step (M5). The pipeline is two steps here, three
  in M5.
- A real Workload kicking off the pipeline. M6 ships the planner;
  for M4, AgenticTasks are applied directly.
- Cleanup. Both tasks linger as Succeeded objects; delete them
  manually when done (`kubectl delete agentictask code-503 gate-503`).
  The gate Job has TTL=24h and cleans itself up.
