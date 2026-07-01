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
| `rbac-use` | `FOREMAN_RBAC_USE_GATE=0` | Flags new calls to Kubernetes RBAC-bearing APIs (e.g. `SubjectAccessReview`, `TokenReview`) without a corresponding marker or documented rationale. Prevents privilege escalation by an unreviewed coder. |
| `import-graph` | `FOREMAN_IMPORT_GRAPH_GATE=0` | Detects imports that introduce a forbidden dependency cycle or pull a heavy external package into a lightweight core package. Keeps the import topology clean without relying on the linter's limited cycle detection. |
| `embedded-artifact` | `FOREMAN_EMBEDDED_ARTIFACT_GATE=0` | Catches binary blobs or pre-built artifacts accidentally committed alongside source (e.g. a compiled binary checked into `cmd/`, a `.zip` dropped into `charts/`). These pass every text-based check and can hide supply-chain issues. |

### Advisory tier

These checks surface findings to the reviewer without blocking the coder.

| Check | Kill switch | Description |
|---|---|---|
| `grounding-breadth` | `FOREMAN_GROUNDING_BREADTH_GATE=0` | Reports when the set of files the coder read during its loop is narrow relative to the scope of changes made. A coder that edits three packages but only ever read files from one may have missed context. |
| `caller-impact` | `FOREMAN_CALLER_IMPACT_GATE=0` | Detects exported function or type signature changes that have callers elsewhere in the repo, so the reviewer knows to check that downstream call sites are consistent with the new contract. |
| `issue-example` | `FOREMAN_ISSUE_EXAMPLE_GATE=0` | Verifies that any example YAML or shell snippet in the issue description that is also reproduced in the coder's output is structurally coherent (not just copied verbatim with no adjustment). |

## Disabling a check

Each check has an environment variable kill switch of the form
`FOREMAN_<UPPER_SNAKE_NAME>_GATE=0`. Set it on the gate Job's pod (via
`AgenticTask.spec.gateEnv` or the node's environment) to skip that check for
a specific task or globally on that node.

Example: to suppress the `caller-impact` advisory on a refactoring task where
widespread caller churn is expected and reviewed by hand:

```yaml
spec:
  gateEnv:
    - name: FOREMAN_CALLER_IMPACT_GATE
      value: "0"
```

Checks default to **enabled** when the variable is unset or set to any value
other than `"0"`.

## See also

- [Language gates](./language-gates): the command-tier checks (format, lint, build, test).
- [Grounding checks](./grounding-checks): the reference-grounding check that
  catches confabulated API groups, fields, and metrics in docs.
- [Foreman overview](./README): the CRDs and the coder / verifier / reviewer pipeline.
