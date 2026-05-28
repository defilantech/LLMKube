# Foreman M4 install runbook: gate-role Agent on a verifier node

This runbook walks through standing up the **verifier** role of the
v0.1 Foreman pipeline. By the end, your cluster has a
running `foreman-operator` + `foreman-agent`, advertises itself as a
FleetNode with `roles=[worker,verifier]`, and is ready for the V3
two-step demo (M4 Phase F) where a coder Agent on the M5 Max produces
a branch and the gate Agent on the verifier node runs `make fmt vet lint
test` against it in a Kubernetes Job.

If you only want the coder role on the M5 Max, read
[`M3 coder runbook`](./runbook-m3) instead. The two runbooks are
independent: install whichever roles you have hardware for, in any
order.

## What M4 ships

- **CRD changes**: `Agent.spec.inferenceServiceRef` is now optional;
  empty means "deterministic Agent, dispatch the first tool
  directly". `AgenticTask.spec.requiredCapability.roles` filters
  FleetNodes by advertised role.
- **New tool**: `run_gate_job` submits a Kubernetes Job that clones a
  branch of the fork, runs the foreman gate check suite (fmt + vet +
  lint + test + manifests + chart-crds + foreman-chart-crds), and
  surfaces `GATE-PASS`, `GATE-FAIL`, or `GATE-ERROR` in the task's
  verdict.
- **New chart pieces** in `charts/foreman/`: operator + agent
  Deployments, RBAC for both, the `foreman-gate-cache` PVC, and a
  `values.yaml` that exposes the M4 install knobs.
- **New images**: `ghcr.io/defilantech/llmkube-foreman-operator`
  and `ghcr.io/defilantech/llmkube-foreman-agent`, published at the
  chart's `.Chart.AppVersion` tag (track the foreman chart release
  for the current tag).

## Prerequisites

- A Kubernetes cluster you have admin on. The runbook assumes
  a CUDA-equipped Linux Kubernetes node but any linux/amd64
  cluster works; the gate Agent is deterministic and does not need a
  GPU.
- `kubectl` configured for the target cluster:
  ```sh
  kubectl get nodes
  ```
- `helm` v3.13 or newer.
- LLMKube core installed at `>=0.7.10`. Foreman's chart depends on
  it for the `inference.llmkube.dev` CRDs the operator's RBAC
  references:
  ```sh
  helm repo add defilantech https://defilantech.github.io/llmkube
  helm repo update
  helm upgrade --install llmkube \
    defilantech/llmkube \
    --namespace llmkube-system --create-namespace
  ```

- **Upgrading from an M3-era cluster?** M4 widens both Foreman CRDs:
  `Agent.spec.inferenceServiceRef` becomes optional, and
  `AgenticTask.spec.requiredCapability.roles` is new. The chart's
  CRD sync handles this automatically, but if you ever apply CRDs
  outside the chart (e.g. directly from `config/crd/bases/`), the
  M3 runbook's
  ["re-apply all four CRDs" callout](./runbook-m3#prerequisites)
  still applies: an older AgenticTask CRD silently rejects
  `spec.requiredCapability.roles` under strict decode.

## Step 1: install Foreman

From the published chart repo (recommended once you have llmkube
installed via the prerequisite above):

```sh
helm upgrade --install foreman \
  defilantech/foreman \
  --namespace foreman-system --create-namespace \
  --set agent.mode=native \
  --set 'agent.roles={worker,verifier}'
```

For dev installs from a local checkout:

```sh
cd /path/to/LLMKube
helm upgrade --install foreman \
  charts/foreman \
  --namespace foreman-system --create-namespace \
  --set agent.mode=native \
  --set 'agent.roles={worker,verifier}'
```

Foreman is a sibling chart to llmkube, not a subchart: installing
foreman never installs (or expects to install) llmkube. If you have
not already installed llmkube core in the same cluster, do that
first per the prerequisite section above.

## Foreman milestone status

Foreman is shipped in stages alongside the LLMKube release train.
Track your install against this milestone matrix so you know what to
expect from a given version:

| LLMKube version | Foreman state                                          |
|-----------------|--------------------------------------------------------|
| `<0.7.10`       | Foreman scaffold only (CRDs, no operator / agent)      |
| `0.7.10+`       | M3 coder + M4 verifier installable; 2-step pipeline    |
| **0.8.0 (planned)** | M5 reviewer + M6 Workload planner — full v0.1 debut |

The 0.8.0 "Foreman's debut" tag will cut once the M6 Workload
planner lands; at that point a single `kubectl apply -f workload.yaml`
will drive the full coder + gate + reviewer pipeline end to end. The
installs documented here work today against any release `0.7.10+`;
the workflows the v0.1 vision promises arrive at 0.8.0.

For a gate-only verifier node (the M4 install), nothing else is
required: the deterministic gate Agent never clones or pushes from
the foreman-agent Pod (the gate Job clones inside its own Pod). For
nodes that will also run coder-role Agents (M3 + later), add the
`agent.gitRemoteURL` + `agent.commitAuthorEmail` knobs:

```sh
helm upgrade --install foreman \
  charts/foreman \
  --namespace foreman-system --create-namespace \
  --set agent.mode=native \
  --set 'agent.roles={worker,verifier}' \
  --set agent.gitRemoteURL=https://github.com/Defilan/LLMKube.git \
  --set agent.commitAuthorEmail=foreman-bot@example.com
```

What that produces:

- A `foreman-operator` Deployment (replicas=1) hosting the Agent,
  AgenticTask, FleetNode, and Workload reconcilers.
- A `foreman-agent` Deployment (replicas=1) that registers itself as
  a FleetNode advertising `accelerator=cuda`, `roles=[worker,
  verifier]`, and the in-cluster InferenceService set.
- ClusterRoles + ClusterRoleBindings for both, scoped to the
  `foreman.llmkube.dev` API group + Jobs + ConfigMaps + pod logs.
- A `foreman-gate-cache` PVC (20 GiB ReadWriteOnce) that the
  `run_gate_job` tool mounts into every gate Job so GOMODCACHE,
  GOCACHE, and XDG_DATA_HOME persist across runs. The first cold
  run takes ~10 min; subsequent warm runs land in ~2-3 min.

Watch the operator come up:

```sh
kubectl -n foreman-system \
  logs -l app.kubernetes.io/component=operator -f
```

The "Starting workers" line for each of the four reconcilers
(Agent, AgenticTask, FleetNode, Workload) means the operator is
healthy.

## Step 2: confirm the agent registered itself

```sh
kubectl get fleetnodes
```

Expected:

```
NAME             READY   ACCELERATOR   RAM    AVAILABLE   AGE
<verifier-node>    True    cuda          64Gi   60Gi        1m
```

Then dig into the advertised roles:

```sh
kubectl get fleetnode -o yaml \
  | grep -A4 'spec:' | grep -E 'roles:|- worker|- verifier'
```

Expected:

```
  roles:
    - worker
    - verifier
```

If you see only `- worker`, the Helm install missed the `roles`
override. Re-run the `helm upgrade` with the `--set 'agent.roles=…'`
line above.

## Step 3: optional smoke task (stub executor)

Foreman ships with a stub executor that does nothing but sleep + write
a synthetic Succeeded result. It's the cheapest way to convince
yourself the scheduler routes correctly to the new FleetNode.

```yaml
# /tmp/foreman-smoke.yaml
apiVersion: foreman.llmkube.dev/v1alpha1
kind: AgenticTask
metadata:
  name: verifier-smoke
  namespace: default
spec:
  kind: freeform
  payload:
    prompt: "smoke test"
  requiredCapability:
    roles: [verifier]
  timeoutSeconds: 60
```

```sh
# Temporarily flip the agent to stub mode for this smoke task --
# the stub executor ignores the Agent CR entirely so we don't need
# one yet.
helm upgrade --reuse-values foreman \
  defilantech/foreman -n foreman-system --set agent.mode=stub

kubectl apply -f /tmp/foreman-smoke.yaml

kubectl get agentictasks -w
# Pending -> Scheduled (assignedNode=<verifier-node>) -> Running
# -> Succeeded within ~15s.

# Flip back to native mode before Phase F.
helm upgrade --reuse-values foreman \
  defilantech/foreman -n foreman-system --set agent.mode=native
```

## Step 4: applying the gate Agent CR

The actual gate Agent + the V3 two-step demo manifests land with
Phase F. Once those merge, the workflow is:

```sh
kubectl apply -f config/foreman/agents/gate.yaml
kubectl apply -f examples/foreman/m4-two-step-demo.yaml
```

See [`M4 verifier runbook`](./runbook-m4) (lands with Phase F) for the
full V3 demo.

## Troubleshooting

**`fleetnodes` list is empty**: the `foreman-agent` Pod has not
registered yet. Check its log: `kubectl -n
foreman-system logs -l app.kubernetes.io/component=agent`. The most
common cause is the ServiceAccount missing `create` on
`fleetnodes`; re-run `helm upgrade` to refresh the RBAC.

**ClusterRole permission denied** on FleetNode write: the chart's
agent ClusterRole grants `create`/`update`/`patch` on
`foreman.llmkube.dev/fleetnodes`. If you customized the chart's
RBAC, diff against `charts/foreman/templates/agent-rbac.yaml` and
confirm the rule is intact.

**`foreman-gate-cache` PVC stuck Pending**: the cluster has no
default StorageClass, or the chosen StorageClass cannot satisfy
ReadWriteOnce. Set `agent.gateCache.storageClass` explicitly or
disable the PVC: `--set agent.gateCache.enabled=false` (gate runs
will still work, just without the inter-run cache).

**Agent Pod startup info-log "--git-remote-url is unset"**: not
an error. Foreman v0.7.10+ logs this as INFO and proceeds; coder
tasks that actually need the URL fail per-task with reason
`GitRemoteURLNotConfigured`. Deterministic Agents (e.g. the M4
gate) run fine without it. To suppress the log line on a node that
will also run coder tasks, set `--set agent.gitRemoteURL=https://github.com/Defilan/LLMKube.git`
+ `--set agent.commitAuthorEmail=...` at install time.

If you are running a pre-v0.7.10 foreman-agent image where this is
a hard `os.Exit(1)`, upgrade. The startup check was relaxed in the
M4 work that landed in 0.7.10.

## What this does NOT cover

- The actual gate Job's resource sizing for a particular cluster.
  The Phase B `run_gate_job` tool ships defaults (2/4 CPU, 4Gi/8Gi
  memory, 30 min activeDeadlineSeconds) that match the autofix
  pipeline's tolerance. Tune via the run_gate_job tool's Config in
  a future Phase G if your cluster has different headroom.
- The Mac Studio reviewer Agent. That ships in M5.
- Cross-cluster setups where the coder and gate Agents run in
  different Kubernetes clusters. v0.1 assumes a single cluster.
- Production-grade observability for the gate Job. The pod log tail
  surfaces in `Result.Extra.logTail`; a full ServiceMonitor for the
  gate's resource usage is a v0.2 follow-up.
