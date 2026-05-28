# Foreman

<p align="center">
  <img src="../images/foreman-logo-icon.svg" alt="Foreman" width="128" height="128" />
</p>

Foreman is the Kubernetes-native control plane for agentic workloads
that ships as an opt-in add-on to LLMKube. It dispatches **coder**,
**verifier**, and **reviewer** agents across a heterogeneous fleet of
locally-hosted LLM nodes (Apple Silicon Metal, NVIDIA CUDA, Intel
oneAPI / SYCL), runs each agent through a native Go function-calling
loop, and produces PR-shaped contributions against your GitHub
repositories.

If you only want LLMKube for serving local models, the operator and
CRDs you already use are unchanged. Foreman lives in its own API
group (`foreman.llmkube.dev`) and its own Helm chart. You install it
on top of LLMKube when you're ready for the pipeline shape; you
ignore it otherwise.

This page is the entry point. Deep references are linked at the
bottom.

## Why Foreman

The argument for Foreman is the combination of three constraints
that ordinary agentic frameworks don't co-solve:

1. **Kubernetes-native.** Declarative CRDs, controller-runtime
   reconcilers, RBAC, Helm, OpenTelemetry. Drops into your existing
   ops surface rather than fighting it.
2. **Heterogeneous-fleet by design.** Capability-aware dispatch
   matches each task to a node whose advertised hardware actually
   fits the job. An Apple Silicon Mac runs the coder, a NVIDIA box
   runs the gate, a third node runs the reviewer.
3. **Local by default.** Inference happens against your own
   InferenceServices (vLLM, llama.cpp, mlx-server, vllm-swift). No
   cloud API egress unless you opt in to the cloud-reviewer escape
   hatch.

In-process frameworks (CrewAI, LangGraph, AutoGen) are excellent
inside one Python or TypeScript process; they don't solve the fleet
dispatch problem. Datacenter inference platforms (NVIDIA Dynamo,
vLLM serving) solve the inference side but not the agent-pipeline
side. Foreman lives between them: above the inference engine,
below the application-layer agent framework, with Kubernetes as
the substrate.

## Key concepts

Four CRDs make up the Foreman surface:

| CRD | Scope | What it represents |
|---|---|---|
| `Workload` | namespaced | The user-facing intent ("fix these eight issues in this repo"). The reconciler decomposes it into a pipeline of AgenticTasks. |
| `AgenticTask` | namespaced | One dispatchable unit of work. References an Agent + a payload. The scheduler claims it for a FleetNode whose capability matches. |
| `Agent` | namespaced | A reusable role definition: system prompt, tool whitelist, model endpoint, required capability. The same Agent can drive many AgenticTasks. |
| `FleetNode` | cluster-scoped | A node in the fleet. The foreman-agent on each host self-registers and advertises its capability (accelerator family, RAM, context window, roles). |

The minimal lifecycle:

```
Workload (intent)
  └── reconciler decomposes →
      ├── AgenticTask: code (agentRef: coder, kind: issue-fix)
      ├── AgenticTask: verify (agentRef: gate, kind: verify, dependsOn: code)
      └── AgenticTask: review (agentRef: reviewer, kind: review, dependsOn: verify)
```

Each task is claimed by the FleetNode whose capability satisfies the
referenced Agent's `requiredCapability`. The foreman-agent on that
host runs the native Go loop against the local inference endpoint.
Verdicts cascade to the parent Workload. Fork branches land on
GitHub for human review.

## Pipeline shape (v0.1)

v0.1 ships the linear pipeline:

1. **Coder.** Reads the issue body via `fetch_issue`, edits the
   workspace, commits with a DCO sign-off, pushes the branch to a
   fork.
2. **Verifier (gate).** Pulls the branch in a Kubernetes Job, runs
   your gate command (in our reference setup
   `make fmt vet lint test manifests chart-crds`). Emits
   `GATE-PASS` or `GATE-FAIL`.
3. **Reviewer(s).** Read the diff against the issue body, score it
   against an A-through-H checklist, emit
   `APPROVE` / `REQUEST-CHANGES` / `REJECT` with structured findings.

Reviewer ensembles are first-class: a `Workload.spec.reviewerAgentRefs`
slice expands to one review-N-i task per (issue, reviewer) pair, and
any `REQUEST-CHANGES` from any reviewer flips the Workload to
`Phase=Failed` via the cascade rule.

DAGs, best-of-N selection, and an autonomous LLM-driven planner are
v0.2+ work. See **What v0.1 deliberately doesn't ship** below.

## Install

Foreman is a separate Helm chart that depends on LLMKube core:

```bash
# Make sure LLMKube core is installed first (0.8.0+)
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
helm upgrade --install llmkube llmkube/llmkube \
  --namespace llmkube-system --create-namespace

# Then add Foreman
helm install foreman llmkube/foreman \
  --namespace foreman-system --create-namespace
```

That installs the foreman-operator (controllers for the four CRDs),
plus a foreman-agent Deployment that registers a FleetNode for the
gate-runner role on the Linux/K8s host. Apple Silicon coder /
reviewer nodes run the foreman-agent binary directly via launchd;
see [the M3 runbook](/docs/foreman/runbook-m3) and
[the M4 runbook](/docs/foreman/runbook-m4) for hosts-side install.

## A minimal example

A two-step coder + gate pipeline against a single issue (the V3
shape from M4):

```yaml
apiVersion: foreman.llmkube.dev/v1alpha1
kind: Workload
metadata:
  name: fix-one-bug
  namespace: default
spec:
  intent: "Fix the lint-all docs gap"
  repo: defilantech/LLMKube
  issues: [510]
  coderAgentRef:
    name: qwen36-35b-carnice-mtp-coder
  verifierAgentRef:
    name: shadowstack-gate
```

`kubectl apply` and watch:

```bash
kubectl get workload,agentictask -n default -w
```

The Workload synthesizes a `code-510` AgenticTask (coder) and a
`verify-510` AgenticTask (gate, depends on code). When both succeed,
a DCO-signed branch lands on the fork
(`Defilan/LLMKube:foreman/fix-one-bug/issue-510`). Verdict
`GATE-PASS` means it cleared the gate; open the branch as an
upstream PR or queue more issues into the Workload.

For the full reviewer-ensemble shape:
`examples/foreman/workload-v04-default.yaml` in this repo.

## What v0.1 deliberately doesn't ship

Foreman v0.1 is the foundation, not the finished platform. A few
capabilities we know people will ask for and deliberately punted:

- **Linear pipelines only.** Full DAGs (parallel branches, joins,
  fan-out across competing candidates) land in v0.2.
- **No best-of-N or jury selection.** Reviewers score the coder's
  diff but don't pick between competing coder candidates. Lands as
  a separate role in v0.2.
- **No autonomous planner.** The current planner is a stub: you
  hand it an issue list, or an explicit pipeline. The LLM-driven
  decomposition lands in v0.2; the v0.1 CRD shape doesn't change.
- **No self-improving routing.** The capability matcher uses fixed
  rules. The AgentScore corpus that biases future dispatch based on
  past outcomes is on the roadmap; v0.1 records the data.
- **Model-tool-protocol compatibility is implicit, not declared.**
  Foreman currently assumes every inference endpoint speaks OpenAI
  `tool_calls`. See [model-compatibility](/docs/foreman/model-compatibility)
  for the calibrated table.

The v0.1 CRD shape was designed so each of those additions is a
non-breaking extension. Pinning the foundation is the work of
0.8.0; everything above is what we build on it.

## Deep references

- **[M3 coder runbook](/docs/foreman/runbook-m3)**: install the foreman-agent
  on the coder host (M5 Max / Apple Silicon).
- **[M4 verifier runbook](/docs/foreman/runbook-m4)**: install Foreman as a chart
  on the K8s cluster and stand up the gate Agent on a verifier
  node.
- **[Verifier node install notes](/docs/foreman/install-verifier-node)**:
  deeper notes on the ShadowStack reference verifier deployment.
- **[Model compatibility table](/docs/foreman/model-compatibility)**: which
  models the v0.4 reviewer and coder roles have been empirically
  validated against.

## Where to file issues

- **Repository:** [github.com/defilantech/LLMKube](https://github.com/defilantech/LLMKube)
- **Discord:** [discord.gg/Ktz85RFHDv](https://discord.gg/Ktz85RFHDv)
- **Issue templates:** `[BUG]` or `[FEATURE]` prefix; the
  templates under `.github/ISSUE_TEMPLATE/` are mandatory for
  triage.

If your shop fits the target profile (on-prem GPU, sovereignty
constraint, K8s in production), we'd love to hear about your fleet.
