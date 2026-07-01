# Gate-check suite: tiered semantic checks

Foreman's fast in-workspace gate runs in two layers. The first is the
language-configurable command tier (gofmt, vet, build, lint, unit tests,
codegen, scope-overlap, test-presence, mutation-survival, reference-grounding):
see [Running Foreman on non-Go projects](./language-gates) and
[Grounding checks](./grounding-checks). The second is the **tiered registry**
of semantic checks described on this page.

Registry checks run after the command tier. Each check has a tier that
determines how a failure is handled:

- **block**: the gate fails, and the failure text is fed back to the coder loop
  as a directive to fix and resubmit. Same treatment as a failing `go vet`.
- **advisory**: the gate still passes, but the finding is attached to the
  reviewer's input and the audit record under `gateAdvisories`. The reviewer
  can use it to upgrade a verdict or request changes.

## Checks

### Block tier

These checks fail the gate and block a GO.

| Check | Kill switch | Description |
|---|---|---|
| `rbac-use` | `FOREMAN_RBAC_USE_GATE=0` | Flags a changed controller file that calls a controller-runtime client verb (Create/Get/List/Update/Patch/Delete/Watch) on a known typed object without a matching `+kubebuilder:rbac` marker in the same package. Unknown types are silently skipped (fail-open); only files under `internal/controller/` or `internal/foreman/controller/` are inspected. |
| `import-graph` | `FOREMAN_IMPORT_GRAPH_GATE=0` | Flags a new import edge in a changed Go file that violates the layering rule: a `pkg/` package must not import an `internal/` package. Pre-existing edges are not flagged; external and stdlib imports are never judged. This is a layering check, not a cycle detector. |
| `embedded-artifact` | `FOREMAN_EMBEDDED_ARTIFACT_GATE=0` | Extracts fenced `yaml`/`yml` code blocks from changed `*.md` files and validates each block as YAML. For blocks that have both `apiVersion` and `kind`, runs `kubectl --dry-run=client` if kubectl is on PATH. Fails on invalid YAML or a failed manifest dry-run; non-YAML blocks are not examined. |

### Advisory tier

These checks surface findings to the reviewer without blocking the coder.

| Check | Kill switch | Description |
|---|---|---|
| `grounding-breadth` | `FOREMAN_GROUNDING_BREADTH_GATE=0` | Flags doc tokens shaped like an external metric name or chart resource name (e.g. a `DCGM_FI_*` prefix or a `kube_*` counter) that do not resolve to a known `llmkube_*` symbol, chart resource, or recognised exporter-metric prefix. Surfaces "minor" severity findings only; the "blocker" findings from the same grounding library are handled by the block-tier reference-grounding check in the command tier. |
| `caller-impact` | `FOREMAN_CALLER_IMPACT_GATE=0` | For each function added or body-modified in a changed Go file, lists the external call sites (callers in other files) so the reviewer can check the blast radius. Functions with no cross-file callers produce no advisory. |
| `issue-example` | `FOREMAN_ISSUE_EXAMPLE_GATE=0` | Harvests a concrete example, repro, or expected-output block from the issue body and surfaces it as an advisory so the reviewer can verify the diff satisfies it. Operates purely on the issue text; does not access the workspace. |

## Disabling a check

Each check has an environment variable kill switch of the form
`FOREMAN_<UPPER_SNAKE_NAME>_GATE=0`. The gate process reads these from its
OS environment, so the variable must be present in the environment of the
process that runs the gate checks (e.g. injected into the gate Job's pod
environment or set on the node). Setting it to `"0"` disables the check;
any other value, or absence of the variable, leaves the check enabled.

Example: to suppress the `caller-impact` advisory on a refactoring task where
widespread caller churn is expected and reviewed by hand, set
`FOREMAN_CALLER_IMPACT_GATE=0` in the gate Job's pod environment.

Checks default to **enabled** when the variable is unset or set to any value
other than `"0"`.

## See also

- [Language gates](./language-gates): the command-tier checks (format, lint, build, test).
- [Grounding checks](./grounding-checks): the reference-grounding check that
  catches confabulated API groups, fields, and metrics in docs.
- [Foreman overview](./README): the CRDs and the coder / verifier / reviewer pipeline.
