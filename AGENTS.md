# AGENTS.md

LLMKube is a Kubernetes operator that deploys and manages local LLM inference
workloads. Go 1.27, built with controller-runtime / Kubebuilder.

This file is the source of truth for the engineering conventions that agents and
contributors share. The human onboarding narrative lives in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Contents

- [Setup](#setup)
- [Commands](#commands)
- [Definition of done](#definition-of-done)
- [Code style](#code-style)
- [Testing](#testing)
- [Commits](#commits)
- [Branches and pull requests](#branches-and-pull-requests)
- [Project layout](#project-layout)
- [Runtime-arg parity](#runtime-arg-parity)
- [Security-sensitive invariants](#security-sensitive-invariants)
- [Do not](#do-not)

## Setup

- Go 1.27.4+ (pinned in `go.mod`).
- `make test` downloads envtest binaries on first run; no cluster needed for
  unit tests.
- `make test-chart` needs `helm` and the `helm-unittest` plugin.
- `make test-chart-rules` needs `helm`, `yq` and `promtool`.
- `make test-e2e` requires Docker and Kind.
- `make test-e2e-rbac` requires Docker, Kind and `jq`; it creates (or reuses)
  its own Kind cluster and kubeconfig file, and never touches your current
  kubectl context.
- `make test-e2e-apisix` requires Docker, Kind and `helm`, plus network access
  to the APISIX chart repository.
- `make test-e2e-multinode` requires Docker and Kind; it creates a two-worker
  Kind cluster whose workers share one host directory
  (`E2E_SHARED_CACHE_DIR`, default `/tmp/llmkube-e2e-shared-cache`), runs
  the e2e specs whose description starts with `Multi-node`, and, like
  `make test-e2e`, reuses an existing cluster of the same name, so run
  `make cleanup-test-e2e` first if a single-node one is up.

## Commands

| Task                                | Command                 |
|-------------------------------------|-------------------------|
| Build the controller                | `make build`            |
| Build the `llmkube` CLI             | `make build-cli`        |
| Build the router-proxy              | `make build-router-proxy` |
| Build the metal-agent               | `make build-metal-agent`  |
| Unit tests (envtest, all packages)  | `make test`             |
| envtest suites only (fixed seed)    | `make test-envtest`     |
| Affected packages while editing     | `make test-affected`    |
| Agent gate (once per round)         | `make gate`             |
| Agent tooling script tests          | `make test-scripts`     |
| Helm chart lint + unit tests        | `make test-chart`       |
| Alert rule behavior (promtool)      | `make test-chart-rules` |
| E2E tests (needs Kind)              | `make test-e2e`         |
| Generated-RBAC e2e (needs Kind)     | `make test-e2e-rbac`   |
| APISIX e2e (needs Kind and Helm)    | `make test-e2e-apisix` |
| Multi-node e2e, two workers (needs Kind) | `make test-e2e-multinode` |
| Format                              | `make fmt`              |
| Vet                                 | `make vet`              |
| Lint (golangci-lint)                | `make lint`             |
| Lint under every `GOOS`             | `make lint-all`         |
| Dead-production-code guard          | `make lint-deadcode`    |
| Validate samples against CRD schema | `make validate-samples` |
| Regenerate CRDs / RBAC / webhooks   | `make manifests`        |
| Regenerate DeepCopy methods         | `make generate`         |
| Regenerate CRDs into the Helm chart | `make chart-crds`       |
| Check AGENTS.md references resolve  | `make check-agents-md`  |
| New Go file headers carry the template's year | `make check-copyright` |
| Go builder images match `go.mod`    | `make check-go-version` |
| Chart metal-agent RBAC and write policy vs manifests | `make check-metal-agent-rbac` |

## Definition of done

A change is not done until all of these hold.

1. The pre-commit gate passes: `make fmt`, `make vet`, `make lint`, `make test`.
2. The pre-push gate passes: `make lint-all`. If `charts/` changed, run
   `make test-chart`, plus `make test-chart-rules` if the PrometheusRule
   changed, plus `make check-metal-agent-rbac` if the metal-agent identity or
   write-policy template or its values changed. If CRD types in `api/v1alpha1/` changed, run
   `make generate` and `make chart-crds`, then `git status` must be clean.
   Uncommitted generated files mean a step was skipped, and the CI CRD sync
   check will fail.
3. Every behavior change has a test that fails without it. Before adding an
   assertion, name the production edit that would make it fail. If you cannot,
   the assertion does not bite. Paste the failure you saw, not a paraphrase.
4. The tests assert on side effects, not return values. A controller test must
   check the world changed the way the spec requires (an object exists, a file
   is on disk, a condition carries the cause), not merely that the code
   returned what it returns. Issue #374 is the canonical case.
5. You can name every consumer of anything you touched (a resource, an
   endpoint, a port, a wire payload, a command-line contract) and what must stay
   true for it. A change that fixes one path while silently breaking a consumer
   is not done.
6. The PR description matches the code. No claim in it that a test or the diff
   does not back.

## Code style

- Match the surrounding code: naming, error handling, comment density, layout.
  Do not introduce a different style for new code.
- Idiomatic Go: `gofmt`, wrapped errors (`fmt.Errorf("...: %w", err)`), table-driven tests.
- A new Go file takes its header from `hack/boilerplate.go.txt`, not from a
  neighboring file. That template's year is what `make check-copyright`
  requires, so the check accepts exactly what AGENTS.md tells you to copy, and
  bumping the template in January is the one edit the rollover needs. A branch
  that adds a Go file with a different year, a year range, or no copyright
  header at all turns the gate red, and so does an untracked scratch `.go` file
  in the worktree, since the gate reads the working tree before you commit.
- Comments explain *why*, not *what*. Do not add docstrings or decorative
  comments to hit a quota.
- Controller code follows controller-runtime conventions: reconcilers are
  idempotent, garbage collection uses owner references, state is surfaced
  through status conditions.

## Testing

- Every behavior change needs a test. A bug fix gets a regression test that
  fails before the fix and passes after.
- Unit tests use `envtest` (controller-runtime); they run with `make test` and
  need no live cluster. For the blessed envtest patterns and the common traps,
  see `docs/contributing/envtest-guide.md`.
- In `internal/controller`, a test must not start its own envtest control
  plane: it is started once per CRD path set in
  `internal/controller/envtest_shared_test.go` (`sharedTestEnv`), and each test
  works in its own namespace (`newTestNamespace`); do not call `t.Parallel`
  there. `make test-scripts` fails on a test that constructs an
  `envtest.Environment` of its own (#2141). The other envtest packages
  (`pkg/cli`, `internal/watchcanary`, `internal/foreman/controller`) keep their
  own control planes; they run in seconds.
- Darwin-only tests (`//go:build darwin`, `_darwin_test.go`) run in CI on a
  hosted macOS runner, with `-race`, for the packages listed in
  `.github/workflows/test-macos.yml`; add a new package with darwin tests there.
- Assert observable behavior, not internal implementation detail.
- envtest runs as cluster-admin, so it cannot catch a missing
  `+kubebuilder:rbac` marker. After a controller change that adds an API
  call (a new resource, verb, or API group), run `make test-e2e-rbac`
  (`hack/kind-e2e-rbac.sh`). It runs the manager out of cluster under only the
  generated ClusterRole and leader-election Role, applies the smoke fixtures
  in `test/e2e-rbac/fixtures`, and fails on any `forbidden` error or reconcile
  error loop, naming the controller, verb, resource and API group. It does not
  cover admission webhooks, the metrics endpoint, or reconcile paths the
  fixtures do not reach (GPU scheduling, gateway and SLO integrations whose
  CRDs are absent); add a fixture when you add such a path.
- Do not weaken or delete a test to make a change pass. If a test is genuinely
  wrong, fix it and say why.
- Do not test a contract against a double built from the consumer's own type. A
  stub that constructs its payload with the same type the code decodes pins
  nothing: a rename of a wire field leaves it green. Feed the real producer's
  bytes, or assert both sides of the contract.

### Agent gate

- While editing: targeted package tests
  (`go test ./internal/foo/ -run TestX -count=1`), plus `make test-affected`
  for the blast radius. It tests every package changed since the merge base
  with `upstream/main` (override with `BASE_REF=<ref>`; includes committed,
  uncommitted and untracked changes) plus every package that depends on them,
  with a fixed `GINKGO_SEED` (default 4242) so go's test cache works between
  runs. `go.mod`, `go.sum`, `go.work` or `go.work.sum` changes select
  everything; no Go change selects nothing and exits 0. Excludes `test/e2e`.
- Once per round, before review: `make gate`. It regenerates manifests and
  DeepCopy and fails on uncommitted regeneration drift, checks gofmt without
  rewriting, checks that every Go file the branch adds carries the copyright
  year `hack/boilerplate.go.txt` does, runs `go vet`, then runs `make lint-all`
  and the full test suite in parallel. Logs land in `${TMPDIR:-/tmp}/llmkube-gate` (override
  `GATE_DIR`); it prints one summary line per step and exits nonzero if any
  step failed. One command covers the fmt/vet/lint/test pre-commit gate and
  the lint-all pre-push check. A rerun with no edits reports `(cached)` for
  the suites; the gate writes no coverage profile, CI's `make test` does.
  The gate does not regenerate the Helm chart CRDs; run `make chart-crds`
  separately when `api/v1alpha1` types change. If
  staticcheck panics under your Go toolchain, set
  `LINT_GOTOOLCHAIN=<version>` (for example go1.27.4).
  The copyright step diffs against `BASE_REF` (default `upstream/main`,
  override with `BASE_REF=origin/main` in a fork clone, the same knob
  `make test-affected` uses), so an unresolvable base fails the gate.
- Full suite (what CI and the Foreman gate run): `make test`, random seed and
  coverage profile, unchanged.
- Editing `scripts/gate.sh`, `scripts/test-affected.sh`,
  `scripts/check-ginkgo-seed_test.sh`, `scripts/check-envtest-sharing_test.sh`
  or `cmd/affected-packages` requires `make test-scripts`: go test never runs
  the shell regression suites. CI runs the same target.
- The gate short-circuits: a failed regen, drift, fmt or copyright step skips
  vet, lint and tests (marked SKIPPED in the summary), so that round gets no
  lint or test signal until the failure is fixed and the gate reruns.
  After an `api/v1alpha1` type change, the first `make gate` fails the drift
  step by design: regeneration rewrote committed files. Commit the
  regenerated output (and run `make chart-crds`), then rerun the gate.

## Commits

This repo uses conventional commit prefixes so `release-please` can generate
changelogs and version bumps. Every commit needs one:

| Prefix      | Use for                                   | Version bump |
|-------------|-------------------------------------------|--------------|
| `feat:`     | New feature, CRD field, CLI command       | minor        |
| `fix:`      | Bug fix, correctness improvement          | patch        |
| `perf:`     | Performance improvement                   | patch        |
| `docs:`     | Documentation only                        | patch        |
| `chore:`    | Deps, CI, tooling (hidden from changelog) | none         |
| `test:`     | Test-only change (hidden)                 | none         |
| `refactor:` | Refactor, no behavior change (hidden)     | none         |

Use `feat!:` / `fix!:` for breaking changes.

- Human contributors sign off every commit: `git commit -s`. A human is
  accountable for every commit, however it was produced (DCO is enforced by
  CI; bot-only sign-offs are not accepted).
- Work authored by `defilan-agent[bot]`, the project's GitHub App, carries no
  `Signed-off-by` line; the agent never signs off its own commits. Before
  approving an agent PR, a maintainer adds the sign-off to the PR branch with
  the `signoff-pr` helper: a `git rebase --signoff` followed by a
  `--force-with-lease` push.
- Subject says *what* changed; body says *why*. No implementation play-by-play.
- Keep commit messages conventional and free of attribution trailers
  (`Co-Authored-By`, `AI-Agent`, `Assisted-by`, etc.). Disclose AI assistance
  with the `Assisted-by:` line in the PR description instead, per
  `CONTRIBUTING.md` ("AI-Assisted and Agent Contributions").
- One logical change per commit.

## Branches and pull requests

- Human contributions go through fork-based PRs. Branch from an up-to-date
  `main`.
- Branch names: `feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `refactor/<slug>`,
  `chore/<slug>`.
- Work an agent does for the maintainers is the exception: it is authored by
  `defilan-agent[bot]`, the project's GitHub App, on `agent/<topic>` branches
  in `defilantech/LLMKube` itself, and the bot pushes there, never to a fork.
- PRs follow `.github/PULL_REQUEST_TEMPLATE.md` (What / Why / How / Checklist)
  and reference the issue with `Fixes #N`.
- Do not push to `main`. Do not force-push shared branches; the one exception
  is a maintainer adding the DCO sign-off to an agent PR branch with
  `signoff-pr` (see Commits).

## Project layout

| Path                    | Contents                                          |
|-------------------------|---------------------------------------------------|
| `cmd/`                  | Entry points (controller, router-proxy, metal-agent, foreman, CLI) |
| `internal/controller/`  | Model, InferenceService, ModelPool, ModelRouter reconcilers |
| `internal/metrics/`     | Prometheus metrics                                |
| `internal/foreman/`     | Foreman controllers                               |
| `pkg/agent/`            | Metal-agent process executor and arg builders     |
| `pkg/foreman/`          | Foreman agent core (gates, tools, review)         |
| `pkg/cli/`              | Cobra CLI commands                                |
| `api/v1alpha1/`         | Core CRD types (regenerate after editing)         |
| `api/foreman/`          | Foreman CRD types                                 |
| `api/federation/`       | Federation CRD types                              |
| `charts/llmkube/`       | Helm chart (CRDs synced via `make chart-crds`)    |
| `charts/foreman/`       | Foreman Helm chart                                |
| `config/`               | Kustomize bases                                   |
| `catalog/`              | Model catalog definitions                         |
| `deployment/`           | Deployment manifests and Ansible                 |
| `terraform/`            | Terraform modules                                 |
| `examples/`             | Sample manifests                                  |
| `hack/`                | Boilerplate and helper scripts                    |
| `scripts/`              | CI helper and validation scripts                  |

The core API group (`api/v1alpha1/`) holds the inference CRDs (Model,
InferenceService, ModelPool, ModelRouter, and the rest). Foreman and federation
are separate API groups under their own directories.

## Runtime-arg parity

Any new `InferenceServiceSpec` field that changes the llama-server command
line must be wired into **all three** of these places, with a test for each:

1. `LlamaCppBackend.BuildArgs`:
   `internal/controller/runtime_llamacpp.go` (and the helper in
   `runtime_llamacpp_args.go`). This is the controller-side arg builder that
   renders the args for the in-cluster container.
2. The metal-agent path:
   `MetalAgent.ensureProcess` -> `buildExecutorConfig` (in
   `pkg/agent/agent.go`) -> `MetalExecutor.StartProcess` /
   `buildLlamaServerArgs` (in `pkg/agent/executor.go`). The metal-agent is
   the out-of-cluster sibling that runs llama-server natively on Apple
   Silicon hosts; its flags must match the controller's.
3. `computeSpecHash` (in `pkg/agent/agent.go`): any field that changes the
   command line must also be folded into the spec hash so a change triggers
   a respawn instead of a no-op.

When adding a new field, mirror the existing `resolveCacheTypes` comment in
`pkg/agent/agent.go`: add a one-line note in the new arg-builder site
explaining that it mirrors the other side, and add a single table-driven
Go test that feeds one representative `InferenceService` spec (with the
runtime-affecting fields set) through **both** `LlamaCppBackend.BuildArgs`
and the metal-agent's `buildLlamaServerArgs`, asserting the same flags
appear on both sides for each field. This is the parity guard; it catches
drift the moment someone wires a field into one side and forgets the other.

Do **not** refactor the two arg builders into one shared function; they
serve different runtimes and have different defaults. The parity test is
the contract, not shared code.

## Security-sensitive invariants

- **Enforcement code is adversarial.** Anything between a client request and a
  decision (a charge, an allow, a route) is driven by input the client
  controls. For every guard, fallback, default, or early return, name the input
  that reaches it and whether a client controls it. A guard whose fallback a
  client can trigger needs a test at the request path, not only a unit case for
  the helper.
- **Contracts that cross a process boundary are duplicated.** The router
  proxy's budget wire tags and the operator's decoder are one pair; the three
  runtime-arg sites above are another. Test them with the real producer's
  bytes or type, never with a double built from the consumer side.
- **A ClusterIP is not a network boundary.** Any new Service port, endpoint, or
  admin surface is reachable by every pod in the cluster once it exists. State
  the default posture in the change, and scope it (NetworkPolicy, an internal
  Service, auth) or say why not.
- **A privileged syscall in a container needs runtime reasoning, not just a
  rendered-spec test.** Do not run an init/main container that performs a
  privileged syscall (`chown`, `chmod` of an unowned file, `mount`, etc.)
  non-root with `capabilities.add` when the syscall runs in an exec'd child
  (`command: ["sh", "-c", "chown ..."]`). A non-root process loses its
  capabilities across `execve` unless the runtime sets ambient capabilities,
  which is runtime-dependent (it works on kind but not on every cluster, so
  envtest and CI will pass while real users hit `EPERM`). Either run the
  container as root (uid 0, still `Drop: [ALL]` + the minimal caps + no
  privilege escalation), or do the syscall directly in the container's
  entrypoint (a small helper binary) so the caps are not cleared. This caused
  the 0.8.20 `model-cache-prep` regression (#887).

## Do not

- Do not hand-edit generated files (`zz_generated.*`, CRD YAML under `config/`
  and `charts/`). Change the source and regenerate.
- Do not edit `.claude/worktrees/` or `bin/`; they are local checkout and build
  artifacts, not source.
- Do not commit secrets, kubeconfigs, or `.env` files.
- Do not skip the pre-commit checks or the CRD sync step.
