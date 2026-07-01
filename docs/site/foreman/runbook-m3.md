# Foreman M3 runbook: native agent loop on the M5 Max

This runbook walks through the M3 end-to-end demo: from a fresh checkout
to a model-authored, DCO-signed branch landing on the LLMKube fork.

M3 introduces the Agent CRD, the native agent loop (OpenAI function
calling against the local llama.cpp server), the six-tool registry
(`read_file`, `write_file`, `str_replace`, `grep`, `bash`,
`submit_result`), and the `NativeAgentLoopExecutor` that wires it all
into the M2 watcher. By the end of this runbook you will have a
branch on `Defilan/LLMKube` whose commit was authored by the local
Carnice model.

## Prerequisites

- LLMKube core operator + metal-agent running on the M5 Max (the
  standard local dev setup; the metal-agent log at
  `~/Library/Logs/llmkube-metal-agent.log` should be live).
- The `qwen36-35b-carnice-mtp` InferenceService is `Ready`:
  ```sh
  kubectl get inferenceservice qwen36-35b-carnice-mtp -n default
  ```
- The Foreman CRDs (Agent, AgenticTask, FleetNode, Workload) installed
  in the target cluster. When checking out a newer branch on top of a
  cluster that already had older foreman CRDs, **re-apply all four**:
  ```sh
  kubectl apply -f config/crd/bases/foreman.llmkube.dev_agentictasks.yaml
  kubectl apply -f config/crd/bases/foreman.llmkube.dev_agents.yaml
  kubectl apply -f config/crd/bases/foreman.llmkube.dev_fleetnodes.yaml
  kubectl apply -f config/crd/bases/foreman.llmkube.dev_workloads.yaml
  ```
  Re-applying every file (not just newly created ones) is required
  because the M3 branch added `spec.agentRef` to the AgenticTask
  schema; an older AgenticTask CRD will silently reject the new field
  under strict decode with `unknown field "spec.agentRef"`. Sanity:
  ```sh
  kubectl get crds | grep foreman.llmkube.dev   # expect 4
  kubectl get crd agentictasks.foreman.llmkube.dev \
    -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.agentRef.type}'
  # expect: object
  ```
- A GitHub Personal Access Token with `public_repo` scope (the
  Foreman bot will push branches with this). The token is read from
  one of:
  - `$GITHUB_TOKEN` in the foreman-agent's env (preferred for
    production / launchd), or
  - `~/.config/foreman/github-token` for local dev. The `gh` CLI
    keyring works as a source: `gh auth token > ~/.config/foreman/github-token && chmod 600 ~/.config/foreman/github-token`.

## 1. Apply the Agent CR

```sh
kubectl apply -f config/foreman/agents/qwen36-35b-carnice-mtp-coder.yaml
```

Verify:

```sh
kubectl get agents -n default
# NAME                            ROLE    MODEL                                                 INFERENCESERVICE         AGE
# qwen36-35b-carnice-mtp-coder    coder   carnice-qwen3.6-moe-35b-a3b-apex-mtp-i-balanced       qwen36-35b-carnice-mtp   ...
```

A note on `requiredCapability.minRAMGB`: the FleetNode advertises
`availableRAMGB` which is **net of the loaded model**, not total RAM.
Carnice 35B A3B at q8_0 KV cache + 256K context occupies ~36 GiB
resident; on a 128 GiB M5 Max that leaves ~62 GiB advertised as
available. Set `minRAMGB` to the workspace + build + test headroom
you need on top of the model (16 is plenty for an LLMKube repo clone
+ `go build` + `make test`), not to the model's own footprint. The
M3 Carnice coder Agent ships with `minRAMGB: 16` for exactly this
reason.

## 2. Start (or restart) foreman-agent in native mode

The default `--agent-mode` flipped to `native` at the same commit that
shipped this runbook, but the binary still needs a few flags pointed
at the right places. Stop any running stub-mode instance, then start
in native mode:

```sh
# Stop existing instance (if running under launchd or as a foreground
# process). The launchd unit name varies by install; check
# `launchctl list | grep foreman` if unsure.
pkill -f 'llmkube-foreman-agent --agent-mode=stub' || true

./bin/llmkube-foreman-agent \
  --agent-mode=native \
  --kubeconfig=$HOME/.kube/config \
  --task-namespace=default \
  --workspace-dir=$HOME/foreman-workspaces \
  --git-remote-url=https://github.com/Defilan/LLMKube.git \
  --inference-base-url-host-override=127.0.0.1 \
  --commit-author-name="Foreman Bot" \
  --commit-author-email="foreman@$(hostname -s).local" \
  --installed-models=qwen36-35b-carnice-mtp \
  --max-context-tokens=262144 \
  --tokens-per-second=80
```

Why each native-mode flag:

- `--git-remote-url`: clones from and pushes to the same URL (the fork).
  Optional as of #915: leave it unset and each coder task clones and
  pushes its own `payload.repo` instead, so one agent serves many repos.
  Set it only to pin every task to a single shared remote.
- `--inference-base-url-host-override`: required when foreman-agent
  runs on the host (where `*.svc.cluster.local` does not resolve).
  The executor still reads `InferenceService.status.endpoint` for the
  scheme and path; it substitutes this host and re-reads the live
  port from the v1 Endpoints object the metal-agent rewrites on every
  llama-server respawn. Set to `127.0.0.1` for the launchd-on-M5-Max
  case. (`--inference-base-url-override` is still available for tests
  and stub OAI servers as a full-URL replacement, but it locks the
  port at install time and breaks on every metal-agent respawn,
  which is exactly the bug #540 fixes.)
- `--commit-author-email`: required; the executor refuses to start
  without it because DCO sign-off needs a real email.
- `--installed-models`, `--max-context-tokens`, `--tokens-per-second`:
  what the FleetNode advertises on its heartbeat so the scheduler
  matches the Agent's `requiredCapability`.

Confirm FleetNode is Ready:

```sh
kubectl get fleetnodes
# NAME      PHASE   ACCELERATOR   RAM   CURRENT TASK   HEARTBEAT   AGE
# m5-max    Ready   metal         128                  10s         ...
```

## 3. Pick a small open issue

The demo expects a real open LLMKube issue scoped to a single file or
two. Curate one with:

```sh
gh issue list -R defilantech/LLMKube --state open --label "good first issue,bug" --limit 20
```

Or filter by size/scope manually. For the first demo, prefer:

- Typo fixes in code comments or markdown.
- Single-file refactors with a clear acceptance criterion.
- Missing-test additions where the fix is obvious.

Avoid (until the model has been validated end-to-end):

- Anything tagged `epic`, `feature`, or `discussion`.
- Multi-file refactors.
- Anything that requires running a real cluster to validate.

## 4. Author the AgenticTask

Copy `examples/foreman/m3-coder-demo.yaml`, replace
`REPLACE_WITH_ISSUE_NUMBER` with the chosen issue number, and paste
the issue's title + body into `spec.payload.prompt`. Then apply:

```sh
cp examples/foreman/m3-coder-demo.yaml /tmp/m3-demo.yaml
# Edit /tmp/m3-demo.yaml: payload.issue + payload.prompt
kubectl apply -f /tmp/m3-demo.yaml
```

## 5. Watch the run

```sh
# Phase transitions, real-time:
kubectl get agentictask m3-coder-demo -w

# Once it reaches Running, tail the foreman-agent log:
tail -f ~/Library/Logs/llmkube-foreman-agent.log

# When it finishes, full status:
kubectl get agentictask m3-coder-demo -o yaml | yq '.status'

# Transcript:
kubectl get cm -l foreman.llmkube.dev/transcript-of=m3-coder-demo
kubectl get cm foreman-transcript-m3-coder-demo -o jsonpath='{.data.transcript\.json}' \
  | jq '.messages | length' # how many turns
kubectl get cm foreman-transcript-m3-coder-demo -o jsonpath='{.data.meta\.json}' | jq
```

Expected phase progression:

- `Pending` (scheduler sees the task, matches it against fleet nodes)
- `Scheduled` (assignedNode=m5-max)
- `Running` (foreman-agent claims it, native loop starts)
- `Succeeded` (loop terminated; transcript persisted; on GO, branch
  pushed)

Time to terminal: usually 3-10 minutes on a small issue.

## 6. Verify the branch

On a GO outcome:

```sh
# The status.result.extra carries the branch and commit SHA:
kubectl get agentictask m3-coder-demo -o jsonpath='{.status.result}' | jq

# Confirm the branch is on the fork:
gh api repos/Defilan/LLMKube/branches/foreman/issue-<N> | jq .commit.sha
gh api repos/Defilan/LLMKube/commits/foreman/issue-<N> | jq '.commit.message'

# The commit message MUST end with `Fixes #<N>` and MUST carry the
# DCO sign-off the executor added via `git commit -s`.
```

## 7. Open the PR (manual in M3; automated in v0.2)

```sh
gh pr create \
  --repo defilantech/LLMKube \
  --base main \
  --head Defilan:foreman/issue-<N> \
  --title "$(gh api repos/Defilan/LLMKube/commits/foreman/issue-<N> | jq -r .commit.message | head -1)" \
  --body-file - <<'BODY'
## What

(Quote the relevant excerpt of `status.result.summary` here.)

## Why

(Quote the issue's motivation.)

## How

(Quote the model's plan-line + the verification it ran.)

## Checklist

- [x] Built locally via the foreman coder agent.
- [x] `make fmt vet lint test` passed inside the agent's workspace
      (see transcript ConfigMap `foreman-transcript-m3-coder-demo`).
- [x] DCO sign-off present.

Fixes #<N>
BODY
```

## What "good" looks like

A clean M3 demo run produces:

- One AgenticTask in `Succeeded` / `verdict=GO`.
- One ConfigMap `foreman-transcript-m3-coder-demo` carrying the full
  turn-by-turn transcript (truncated marker only if the run exceeded
  ~1 MiB).
- One branch `foreman/issue-<N>` on `Defilan/LLMKube` whose head
  commit is authored by `Foreman Bot`, carries `Fixes #<N>` in the
  trailer, and `Signed-off-by: Foreman Bot ...`.

## What can go wrong

- **`status.result.extra.outcome == "EXECUTOR-PRECONDITION-FAILED"`**
  with `reason=AgentNotFound`: the Agent CR was deleted between
  scheduling and execution. Reapply
  `config/foreman/agents/qwen36-35b-carnice-mtp-coder.yaml`.

- **`outcome=PUSH-FAILED`**: the GitHub token lacks `public_repo`
  scope, or the fork's `main` moved under us. Rotate the token or
  rebase your fork against upstream and re-run.

- **`outcome=COMMIT-REJECTED`**: usually a pre-commit hook in the
  workspace that the model could not satisfy. Check the transcript;
  the bash tool's stderr will show the rejection. Fixable by either
  improving the system prompt or removing the hook for foreman runs.

- **`outcome=NO-CHANGES`**: the model emitted `verdict=GO` but never
  edited a file. Honest report: the loop's reasoning is in the
  transcript. Often signals the model decided the issue was already
  fixed mid-run but did not flip to `NO-GO`.

- **`outcome=LOOP-INCOMPLETE`** with `reason=MaxTurnsExhausted`:
  the model ran `maxTurns` (80 on the Carnice coder) without calling
  `submit_result`. Either the issue was too complex (raise to a
  human) or the prompt needs tightening.

- **`outcome=LOOP-INCOMPLETE`** with `reason=AssistantHallucinatedFinish`:
  the model emitted plain text instead of a tool call. The system
  prompt explicitly forbids this; if it recurs, the prompt's
  "no `=== VERDICT ===` text markers" line needs to be more
  emphatic, or the model needs a higher-quality function-calling
  fine-tune.

## What is *not* in M3

- Multi-step pipelines (coder → gate → reviewer): the gate Agent
  on ShadowStack lands in M4; the reviewer Agent on the Mac Studio
  in M5.
- Automated PR creation: still a manual step here. M4 wires it
  alongside the gate.
- A Workload CR that fans an intent out across many tasks: M6.
- Anything cross-cluster: foreman-agent runs on the M5 Max only in
  v0.1.
