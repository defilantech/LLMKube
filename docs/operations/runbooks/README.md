# LLMKube operational runbooks

This directory holds incident-response runbooks for operators running LLMKube in production. Each runbook follows a consistent shape so operators can find the right one fast under pressure and follow it step-by-step.

## Runbook conventions

- **Title** names the symptom an operator would search for, not the underlying cause (e.g., "controller pod CPU pegged at 100%" not "file:// fetch backoff regression"). Operators triage by what they see.
- **Trigger** section names the alert, log line, or `kubectl describe` output that brought the operator to this runbook.
- **Diagnose** section is a numbered checklist. Each step has the exact command to run.
- **Mitigate** section is the immediate action that stops the bleeding.
- **Resolve** section is the structural fix.
- **Verify** section is the exact command that proves the issue is gone.
- **Related** section cross-links to issues, PRs, and other runbooks.

Every runbook should be testable: an on-call engineer who has never seen the failure mode should be able to follow the runbook to resolution without paging the maintainer.

## Runbook index

### Reliability

- [`controller-hot-spin-on-file-source.md`](./controller-hot-spin-on-file-source.md): controller-manager pod pegging CPU because a `Model` references a path the controller pod cannot read.

### Memory and resource pressure (Apple Silicon / metal-agent)

- [`metal-agent-memory-pressure.md`](./metal-agent-memory-pressure.md): the metal-agent watchdog reports Warning or Critical memory pressure and may evict managed inference processes.

### Lifecycle

- [`upgrade-rollback.md`](./upgrade-rollback.md): the supported Helm-based upgrade path for moving an LLMKube install between minor or patch versions, plus the rollback procedure when an upgrade goes wrong.

### Pending (planned)

These are scheduled to land before the day-one production install. Each maps to a known failure mode worth documenting.

- `model-fetch-failure.md`: HTTP 5xx, DNS, or auth failures fetching a `https://` model.
- `pvc-source-not-bound.md`: `pvc://` source where the referenced PVC is missing or unbound.
- `controller-crashloop.md`: controller-manager pod itself is restarting; how to triage.
- `inference-pod-stuck-in-creating.md`: `InferenceService` phase stays `Creating`; init container, scheduling, image pull triage.
- `inference-pod-oom-on-gpu.md`: GPU memory exhaustion; fits with `Memory.Budget` enforcement on metal-agent and with multi-GPU sharding gone wrong.
- `nvlink-degrade-to-pcie.md`: Fabric Manager / driver mismatch causing NVLink5 to silently fall back to PCIe (B200 specific; severe).
- `cold-start-timeout-at-large-context.md`: `--max-model-len` set high enough that the runtime startup probe times out before the model finishes loading.
- `parser-race-on-tool-calls.md`: streaming parser race between tool-call frames and ordinary text frames.
- `metal-agent-respawn-blocked.md`: agent refuses to respawn an evicted process; usually correct but documented for clarity.

## Tabletop cadence

Quarterly: pick one runbook and follow it as written against the `shadowstack` cluster, simulating the failure. If the runbook is wrong, fix it on the spot. If the failure is no longer reproducible, mark the runbook as historical and link to the change that retired it.

This is the only way runbooks stay accurate. A runbook nobody has run in a year is a piece of comforting fiction.

## Authoring a new runbook

1. Copy `controller-hot-spin-on-file-source.md` as the template (it has all six sections filled in).
2. Drop the new file in this directory with a descriptive kebab-case name.
3. Add it to the runbook index above (in the right category).
4. Cross-link from any relevant code paths via comment (e.g., the watchdog hook in `pkg/agent/pressure.go` should reference the memory-pressure runbook).
5. Open a PR. CI will not gate on this; reviewers should sanity-check that the commands actually work as documented.
