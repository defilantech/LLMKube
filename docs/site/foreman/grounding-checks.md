# Grounding checks: catching confabulated references

Foreman's verify gate has two layers. The first is the language-configurable
command tier (format, lint, build, test): see
[Running Foreman on non-Go projects](./language-gates). The second is a set of
deeper **semantic** checks that catch failure modes a command can never see.
Reference grounding is one of them, and this page documents both what it does
and the model to follow when you want the same protection for your own stack.

The premise is the "harness, not model" one: a capable local model still
confabulates, and the answer is a harness that verifies, not a bigger model.

## The blind spot: documentation and config

Format, lint, build, and test all operate on code. They never read the prose
and example manifests a coder adds to `docs/`. So a model can write a tutorial
that cites an API group, a CRD field, a metric, or a CLI command that does not
exist, and every command-based check stays green because no code changed.

This is a real, observed failure mode. In one run a coder wrote a perfectly
formatted autoscaling tutorial whose YAML used the API group `llmkube.io`
(the real group is `inference.llmkube.dev`) and scraped a Prometheus metric
that was never registered. The docs compiled into nothing, so the gate passed
it. A reader who copy-pasted that manifest would get a schema rejection, and a
dashboard built on that metric would stay empty.

## What the grounding check does

On a coder's candidate GO, the grounding check scans the **added** lines of any
docs and example YAML and flags references to project-owned symbols that do not
resolve in the repository:

- an `apiVersion:` naming an LLMKube-owned API group that no CRD defines, and
- an `llmkube_*` metric token that is registered nowhere in the tree.

A flagged reference fails the gate and feeds the specifics back to the coder,
the same way a failing test does, so the loop gets a chance to fix the citation
before a reviewer ever sees it. A correct example like this one passes
untouched:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: my-service
spec:
  modelRef: my-model
```

## The model to follow

The shape of the check is general; only the symbol sources are
project-specific. If you are adapting this to another project or language,
follow the same four rules. They are what keep it useful without becoming
noisy.

### 1. Build ground truth from the project's own sources of truth

The check assembles the set of real symbols by reading the repository: API
groups and kinds from the generated CRDs under `config/crd/bases`, and
`llmkube_*` metric names from the source. For another stack the sources differ
but the idea is identical: enumerate the symbols your project actually defines
from wherever you define them, for example an OpenAPI document, `.proto` files,
a metrics registry, your package's public API, or your CLI's command list.

### 2. Judge only the symbols you own

The check validates only LLMKube-owned namespaces: groups matching
`inference.llmkube.dev` (and the project's other owned suffixes) and the
`llmkube_*` metric prefix. External references such as `apps/v1`,
`autoscaling/v2`, or a third-party metric are never judged. This is the single
most important rule: models confabulate *your* symbols, not the wider
ecosystem's, and validating things you do not own is where false positives come
from. Scope to your prefixes and namespaces and the check stays quiet on
correct docs.

### 3. Inspect the working tree, not committed history

The gate runs before the coder's work is committed, so the check diffs the
staged working tree against the branch point rather than committed history.
That way it sees exactly the lines the coder added, including brand-new files.

### 4. Hold the line at near-zero false positives

Because a failed check blocks a GO, a false positive blocks legitimate work,
which is worse than a missed catch. Keep each check to a class you can validate
as essentially false-positive-free. The group and metric checks were validated
against the entire committed documentation corpus with zero false positives;
checks that could not clear that bar (matching CRD field names and `kind:`
values, which collide with field values and historical release notes) were
deliberately left out and deferred to a tool-using reviewer that can parse and
grep rather than pattern-match.

## Bringing grounding to your language

Today the grounding check is wired into the Go gate path and grounds against
LLMKube's CRDs and metrics. The command tier already travels to any language
through [`gateProfile`](./language-gates); the semantic checks do not yet, so a
Python or Rust project gets the command gate but not grounding.

Closing that is additive, not a rewrite. `gateProfile` is the seam: it already
carries `language`, `sourceExtensions`, and the command set. A
profile-configurable grounding check would add declared **symbol sources** (an
OpenAPI path, a proto directory, a metrics manifest, a package's exported API)
and the owned-namespace patterns, and the same four rules above would then
protect your docs in your language. If that is something you want, open an issue
describing your project's symbol sources and we can shape the generalization
together.

## See also

- [Running Foreman on non-Go projects](./language-gates): the command tier and
  the `gateProfile` field, with a verified Python example.
- [Foreman overview](/docs/foreman): the CRDs and the coder / verifier /
  reviewer pipeline.
