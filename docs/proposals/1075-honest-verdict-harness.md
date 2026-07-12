# Proposal: Honest-verdict harness (declare-then-verify contract for repo-agnostic coder gates)

**Status:** Proposed (design)
**Umbrella issue:** [#1075](https://github.com/defilantech/LLMKube/issues/1075)
**Related:** [#1072](https://github.com/defilantech/LLMKube/issues/1072) (gate blind to non-Go changes), [#1073](https://github.com/defilantech/LLMKube/issues/1073) (fabricated benchmark numbers survive the grounding rail), [#1061](https://github.com/defilantech/LLMKube/issues/1061)/[#1062](https://github.com/defilantech/LLMKube/issues/1062) (anti-confabulation rule and `NEEDS-VERIFICATION` outcome)
**Evidence:** Foreman workload `run-20260711-182721` (issues 850/233/822/440/699/631); three GO verdicts, zero mergeable.

This document is the design reference for making a Foreman coder's `GO` verdict
auditable instead of self-certified. It defines one repeatable primitive (the
coder declares, the gate verifies the declaration against the diff), applies it
to three failure classes observed in production batches, and specifies how the
pattern generalizes to repositories that are not LLMKube and not Go.

---

## 1. Problem and motivation

A coder's `GO` today is a self-certification. The gate then verifies the *Go
code* in the workspace (`gofmt`, `go vet`, `go build`, `golangci-lint`, a fast
unit-test tier, plus the tiered registry checks), and if those pass the verdict
stands. This works precisely when the diff is Go code. A six-issue M2/M3 batch
showed what happens when it is not:

- **#233 (supply-chain CI).** The diff was entirely GitHub Actions YAML. It
  contained a reusable workflow invoked as a `steps:` entry (invalid; reusable
  workflows are job-level only) and keyless `cosign sign` without granting the
  job `id-token: write`. The Go gate had nothing to check, so nothing failed.
  The verdict read `GO`; the change could never have run.
- **#699 (validated AMD example + benchmark).** The diff was docs plus
  manifests. The repository holds exactly one measured gfx1151 number
  (proposal 697: Llama-3.2-3B Q4_K_M, ~87 tok/s decode). The coder attributed
  that number to a different model, altered it for the model it was measured
  on, invented the remaining rows, and labeled the table "documented" and the
  example "validated". The grounding rail did not fire: it checks *names and
  identifiers* (metrics, CLI commands, chart resources, CRD fields) against
  repo ground truth, and has no concept of a numeric, empirically-measured
  claim.
- **Triage is advisory.** The coder prompt already instructs it to decline
  policy work and to report `NEEDS-VERIFICATION` for facts it cannot ground.
  #233 was signing policy (a human decision by the prompt's own Step 1 rules)
  and #699's numbers were the prompt's literal textbook example of an
  ungroundable fact. The model walked past both exits. Prompt rules reduce
  these failures; they cannot bound them.

The common shape: **the coder asserts things the gate never checks.** The fix
is not a better prompt and not more Go checks. It is a contract in which every
load-bearing assertion in a `GO` is either machine-verified or explicitly
surfaced as unverified.

The secondary goal is repeatability. Today "verified" is defined by LLMKube's
own Makefile. For Foreman to gate work on arbitrary repositories, the mapping
from "what changed" to "what proves it correct" must be discoverable and
overridable per repository, not hardcoded per language.

## 2. The contract: declare, then verify

One primitive, applied three times. With every `GO`, the coder submits a claim
package; the gate independently derives the same facts from the diff and
downgrades on mismatch:

```text
submit_result(
  verdict = "GO",
  summary = "...",
  commit_message = "...",
  extra = {
    "workClass": "code-fix",          # section 3
    "evidence": [                      # section 4
      {"claim": "~87 tok/s decode on gfx1151 (Llama-3.2-3B Q4_K_M)",
       "source": "docs/proposals/697-amd-vulkan-runtime-image.md:7"},
    ],
  },
)
```

The gate audits three questions, each deterministically:

1. **Is the declared work class what the diff actually is?** Footprint
   analysis; mismatch or a class that policy says cannot self-GO downgrades
   the verdict (section 3).
2. **Is every empirical claim in the diff backed by evidence that exists?**
   A gate-side claim detector; unmatched load-bearing claims downgrade to
   `NEEDS-VERIFICATION` (section 4).
3. **Was the diff actually verified by anything?** Path-scoped checks with
   coverage reporting; a pass over a diff the gate could not inspect is
   reported as unverified, not clean (section 5).

Verdict vocabulary does not change. `GO` narrows to mean "the gate could and
did audit this"; `NEEDS-VERIFICATION` (existing, #1062) absorbs everything
correct-looking that machines cannot check. A downgraded verdict carries the
specific mismatches in the gate feedback, so a revise cycle (or a human) sees
exactly what to fix. Consistent with #1062, `NEEDS-VERIFICATION` is not
escalated to a larger model: a larger model cannot reach the missing ground
truth either.

Why gate-side derivation matters: a fabricating coder simply omits the claim
from its evidence block. Detection must run against the diff, not against the
declaration. The declaration exists so the model commits to an account of its
own work that the machine can falsify; the asymmetry (cheap to verify, hard to
fake) is the entire design.

## 3. Work-class cross-check

### 3.1 Classes and footprint derivation

Each changed file is classified by a glob table (first match wins):

| Class | Default globs |
|---|---|
| `ci-policy` | `.github/workflows/**`, `.github/actions/**` |
| `release-policy` | `.goreleaser*`, `**/release-please*`, signing/provenance config |
| `packaging` | `Formula/**`, `Dockerfile*`, `charts/**`, `**/*.spec` |
| `docs` | `**/*.md`, `docs/**`, `examples/**/README*` |
| `config` | `**/*.yaml`, `**/*.yml`, `**/*.toml`, `**/*.json` (not matched above) |
| `code-fix` | everything else (source and test files) |

The diff's **actual class** is the dominant class by changed-line count;
a diff with no dominant class (none reaches 70%) is `mixed`. The coder's
declared `workClass` is compared against this. Mismatch is a gate failure
with feedback naming both classes, exactly like a failing lint check; the
coder can revise (correct the declaration, or correct the diff).

### 3.2 Self-GO policy: configurable, default never for policy classes

Which classes may self-certify `GO` is policy, not model judgment:

```yaml
# Workload.spec.verdictPolicy (all fields optional; defaults shown)
verdictPolicy:
  selfGO: [code-fix, docs, packaging, config]   # docs additionally requires a
                                                # clean claim scan (section 4)
  # ci-policy and release-policy are absent by default: a GO on those classes
  # is downgraded to NEEDS-VERIFICATION with the diff footprint as evidence.
```

The default encodes "an agent does not set its own CI, signing, or release
policy". Operators who *want* an agent managing workflows opt in per Workload
by adding the class to `selfGO`. This mirrors the layered philosophy used
everywhere else in this proposal: safe defaults, explicit overrides, nothing
hardcoded.

With this table, #233 is mechanical: declared class irrelevant, footprint is
100% `ci-policy`, `ci-policy` is not in `selfGO`, verdict becomes
`NEEDS-VERIFICATION` ("policy change requires human sign-off; footprint:
.github/workflows/release-please.yml, 100% of changed lines").

## 4. Evidence contract for empirical claims

### 4.1 Gate-side claim detection

A new detector in `pkg/foreman/agent/grounding`, symmetric with the existing
`DetectUngroundedReferences`, scans **added lines** for empirical claims:

- numbers with performance/resource units: `tok/s`, `t/s`, `ms`, `s`, `GB`,
  `MB`, `W`, `%` in a measurement context;
- benchmark-table rows (a Markdown table row containing such numbers);
- certainty language attached to results: "validated", "verified",
  "measured", "benchmarked", "tested on", "documented" adjacent to numbers
  or hardware names.

Scope: blocking for `docs`-class files (Markdown, `examples/**`); advisory
for comments in code and YAML (a wrong number in a comment misleads, but it
does not present itself as a validated deliverable). The detector is
deliberately narrow and lexical: it does not judge truth, it identifies
*claims that require a source*.

### 4.2 Matching and source validation

Every detected load-bearing claim must be matched by an `evidence` entry
whose `source` the gate validates:

- **In-repo `file:line`**: the file exists and the cited line (±2 for drift)
  contains the same number and unit. This is the strong check, and it is the
  one #699 fails three ways: the real line pairs 87 tok/s with Llama-3.2-3B,
  so attributing it to Qwen3 30B does not match; 95 tok/s appears nowhere;
  the Mixtral row has no source at all.
- **URL**: the gate records it as declared-but-unfetched by default
  (air-gapped clusters cannot fetch); a gate flag can enable a HEAD/GET
  probe on connected clusters. Unfetched URL evidence marks the claim
  "declared, unverified" in the coverage report rather than passing it.
- **`source: NONE` or missing**: the claim is unmatched.

Unmatched load-bearing claims downgrade `GO` to `NEEDS-VERIFICATION`, with
the claims themselves listed in `extra.unverified` (the existing #1062
shape: `{fact, whyItMatters, howToVerify}` is generated from the claim text
and its location). The coder's honest alternatives are: cite a real source,
delete the claim, mark the number as illustrative and unmeasured in the text
itself, or submit `NEEDS-VERIFICATION` up front. All four are acceptable
outcomes; a "validated" table with invented rows is not.

### 4.3 False-positive posture

Lexical claim detection will over-trigger occasionally (a version number in
a sentence, a port number near a "%"). Mitigations: unit-context matching
(a number qualifies only with a recognized unit or inside a benchmark
table), the docs-only blocking scope, and the standard revise cycle (the
feedback names the exact line; adding a citation or rewording costs one
turn). The failure mode of over-triggering is a wasted turn; the failure
mode it prevents is publishing fabricated hardware results. That trade is
taken deliberately.

## 5. Path-scoped verification registry and coverage

### 5.1 Built-in detection (zero-config day one)

The tiered `gateCheckRegistry` gains **path-scoped checks**: each registers a
glob and runs only when the diff footprint matches.

| Glob | Check | Tier |
|---|---|---|
| `.github/workflows/**` | `actionlint` | blocking |
| `**/*.sh` | `shellcheck` | blocking |
| `Formula/*.rb` | `brew style --formula` (or standalone RuboCop) | blocking |
| `**/*.{yaml,yml}` | schema/syntax validation (existing artifact gate, extended) | blocking |
| `**/*.md`, `docs/**` | claim scan (section 4) + existing reference grounding | blocking |
| `Dockerfile*` | `hadolint` | advisory |

Missing tooling degrades honestly, reusing the generic gate's existing
semantics: a checker absent from the coder image (exit 127 / ENOENT) is
recorded as `self-gate-deferred` and re-run in the clean-room verify Job,
whose image carries the full toolchain. A check that cannot run anywhere is
reported as uncovered (5.3), never silently skipped.

### 5.2 Repo override: `.foreman/verify.yaml`

A target repository can pin or extend verification without any Foreman-side
change:

```yaml
# .foreman/verify.yaml (optional; layered over built-in detection)
checks:
  - match: "**/*.go"
    run: ["make fmt", "make vet", "make lint", "make test"]
  - match: "Formula/*.rb"
    run: ["brew style --formula {files}"]
  - match: "docs/**"
    verify: claims        # route through the claim scan only
defaults:
  unmatched: report       # report | block: what to do with file types
                          # nothing matched (report = count as uncovered)
```

Resolution order per file: repo `verify.yaml` match, else built-in glob
table, else unmatched. `run` commands execute in the workspace (self-gate)
and the clean-room Job (authoritative), with the same deferred semantics as
5.1. This is the adoption story for other users: day one needs nothing;
pinning exact commands is one small file in their own repo, reviewed through
their own PR process.

### 5.3 Coverage reporting: no more hollow green

The gate computes, per verdict, the fraction of changed lines that at least
one blocking check actually examined, and attaches it to the result:

```text
GATE-PASS  coverage: 84% (go: 61%, actionlint: 23%; uncovered: docs 16%)
GATE-PASS  coverage: 0%  (uncovered: docs 100%)   <- today this reads "clean"
```

Coverage lands in `status.result.extra.gateCoverage` and in the human
feedback line. It does not change the verdict by itself (policy in 3.2 and
claims in section 4 do that); it changes what a `GATE-PASS` *means* to the
person reading a batch report. A 0%-coverage pass stops masquerading as
verification.

## 6. Existing seams this builds on

This proposal adds no parallel machinery. Each piece extends a seam that
already exists:

- **`GateProfile`** (`api/foreman/v1alpha1`) already models per-language gates
  (Go, Python, Rust, Node, Generic) with `usesGenericGate` routing; the
  path-scoped registry generalizes the same idea from "one language per task"
  to "checks per changed path".
- **`gateCheckRegistry`** (`pkg/foreman/agent/coder_gate.go`) already runs
  tiered blocking/advisory checks (artifact YAML validation, import graph,
  RBAC use, grounded findings); path-scoped checks and the work-class
  cross-check register there.
- **`grounding`** (`pkg/foreman/agent/grounding`) already scans repo ground
  truth and flags ungrounded identifier references in added lines; the claim
  detector is a sibling of `DetectUngroundedReferences` sharing `AddedLines`.
- **Generic gate deferred semantics** (`generic_gate.go`) already distinguish
  "check failed" from "runtime missing, defer to clean-room"; path-scoped
  checks reuse it unchanged.
- **`NEEDS-VERIFICATION`** (#1062) already exists as an outcome with
  structured `unverified` entries and a no-escalation rule; sections 3 and 4
  generate it mechanically instead of relying on the model to volunteer it.

## 7. What another user does to adopt this

1. Point a Workload at their repo. Built-in detection covers workflows,
   shell, Markdown claims, YAML syntax with zero configuration; Go repos
   additionally get the full Go gate.
2. Optionally commit `.foreman/verify.yaml` to pin their real build/test
   commands per path.
3. Optionally set `verdictPolicy.selfGO` on the Workload to widen or narrow
   what may self-certify (default: policy classes never).
4. Read batch results where every `GO` carries a work class, an evidence
   ledger, and a coverage figure.

Nothing in steps 1-4 mentions Go, LLMKube's Makefile, or LLMKube's docs
layout. That is the repeatability claim, and it is testable against any
public repository.

## 8. Decomposition (for the implementation plan)

- **Slice 1 (kills the observed false-GO classes):**
  - claim detector in `grounding` + evidence matching + verdict downgrade;
  - work-class footprint derivation + declared-class cross-check +
    `verdictPolicy` on the Workload CRD (manifests, chart CRDs, RBAC sync);
  - coder prompt: declare `workClass`, populate `evidence` (small,
    additive prompt change; the enforcement is gate-side).
- **Slice 2 (closes #1072 broadly):**
  - path-scoped check registration + built-in glob table + tool presence /
    deferred handling; toolchain additions to the coder and gate images;
  - `.foreman/verify.yaml` loader and resolution;
  - coverage computation + `extra.gateCoverage` + feedback line.
- **Acceptance A/B:** re-run the same six-issue batch (850/233/822/440/699/
  631) on the same coder model. Green means zero hollow GOs: #233 and #699
  become `NEEDS-VERIFICATION` with the exact defects named; #822's GO gains
  a RuboCop check that catches the duplicate method; #440/#631/#850 outcomes
  unchanged.

## 9. Alternatives considered

- **Reviewer-model routing for unverifiable content.** Reuses the reviewer
  fleet, but it is a model checking a model: reviewer-diversity experiments
  on this fleet showed rubber-stamping under exactly the conditions that
  matter. Kept as a complement (a reviewer can still read what the gate
  cannot), rejected as the enforcement layer.
- **Forced human queue for any unverifiable diff.** Maximally honest, but
  caps autonomy: all docs and CI work would require a human touch, which
  defeats overnight batches. The claim/evidence contract preserves autonomy
  for the (common) case where sources exist.
- **Pre-dispatch issue classification.** Classifying from issue text alone
  is unreliable; #233 read as a chore until the diff existed. Cheap
  pre-filtering can be added later as an optimization; the footprint check
  is the one that cannot be fooled by issue wording.
- **Requiring `.foreman/verify.yaml` (no built-in detection).** Fully
  deterministic, but every adopter must write config before their first run,
  and unconfigured file types would silently get zero verification, which is
  today's hole formalized. Layered detection-plus-override matches how
  golangci-lint and similar tools won adoption.
