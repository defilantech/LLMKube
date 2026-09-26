# ripwire vs repomap: localization spike

**Issue:** none yet (spike branch `feat/ripwire_spike`); assign a tracking issue before this lands.
**Date:** 2026-09-26
**Status:** Spike complete. Recommendation: ripwire wins the value question; a separate proposal is needed for any integration, and it must not touch the enforcement path.

## Problem

Foreman's coder localization uses `pkg/foreman/agent/repomap`, a lexical bag-of-words ranker (Aider-style, cited at `repomap.go:24-30`, scored by `score.go:73-118`). ripwire (redhat-et/ripwire) advertises a deterministic call-graph ranker and reports 58.3% strict file@10 against Aider's repo-map at 20.0% on its own bench.

The open question this spike answers: **does a call-graph ranker beat the lexical ranker at putting the files a real llmkube fix touched into the top-K, measured on llmkube's own merged issues?** This is a value question only. It deliberately does not answer the integration or deployability questions (see [Not answered](#not-answered-by-this-spike)).

## Method

- **Ground truth.** For each commit in the last 600 whose subject references `(#N)` and which changed at least one non-test `.go` file, gold = that commit's changed non-test `.go` files (`git show --name-only`). This is the files-the-fix-touched set, the standard localization gold. 180 usable candidates existed; 25 were sampled.
- **Query text.** The real GitHub issue/PR body for `N` (`gh pr view` falling back to `gh issue view`), whitespace-collapsed and capped at 1500 chars. Using the issue body rather than the commit subject matters: commit subjects name files, and repomap's `pathMentionWeight = 50` (`score.go:125`) would light up on that, inflating the lexical baseline. Both rankers receive byte-identical query text.
- **ripwire.** `ripwire . --for=<query> --format=candidates` (v0.6.4, macos/arm64), deduping the `p=` path attributes in rank order to get a ranked file list.
- **repomap.** `Walk` + `ScoreFiles` via the throwaway `scripts/spike/repomaprank/main.go`.
- **Metric.** Strict file@K: all gold files inside the top K of a case, K = 5 and K = 10. Reported overall and split by whether the query literally names a gold file ("easy" vs "hard"), so easy wins cannot stack.
- **Harness.** `scripts/spike/ripwire_eval.sh <ncases> <workspace>`. Query and gold overrides exist for the falsification checks below.

## Results

25 cases, 82 gold files total, ripwire v0.6.4 on the llmkube checkout (cold parse ~0.9 s).

| Metric | ripwire | repomap |
|---|---|---|
| Strict file@5 | **11/25 (44%)** | 5/25 (20%) |
| Strict file@10 | **15/25 (60%)** | 8/25 (32%) |
| Strict file@10, hard cases (n=15) | **9/15 (60%)** | 4/15 (27%) |
| Strict file@10, easy cases (n=10) | 6/10 (60%) | 4/10 (40%) |

Aggregate gold-file recall@10 was 68/82 (83%) for ripwire against 47/82 (57%) for repomap.

The margin is close to ripwire's own published claim (58.3% vs 20.0%) and it holds on the hard stratum, where the query does not name any gold file: 60% vs 27%. That is the result that matters, because it is the case where a ranker has to use structure rather than string overlap.

## Falsification

Three checks were run before trusting the numbers. All three behave as required.

1. **Unrelated query must score near zero.** `SPIKE_FORCE_QUERY="the quick brown fox..."` over 5 cases: 1/15 gold hits for both rankers. The scoring path is not a constant and is not trivially permissive.
2. **Gold must be read per case, not pinned.** `SPIKE_GOLD_FROM_RELEASE=1` (gold = HEAD's changed files for every case): every case collapses to 1 gold file and 0 hits. The harness is actually reading each commit's changed set.
3. **The two rankers must diverge on identical input.** Same query (#1898) through both: ripwire top-5 = `pkg/agent/agent.go, internal/controller/runtime_llamacpp.go, internal/controller/runtime_llamacpp_router.go, pkg/agent/executor.go, internal/controller/runtime_sglang.go`; repomap top-5 = `pkg/agent/agent.go, pkg/agent/executor.go, internal/controller/runtime_llamacpp.go, pkg/foreman/agent/executor.go, internal/router/config.go`. Different structure, and ripwire surfaces the gold `runtime_llamacpp_router.go` where repomap does not.

## Recommendation

**Value: adopt ripwire for the advisory path, if the deployability question clears.** The margin is large, holds on the hard stratum, and aligns with ripwire's independent bench.

**Do not wire ripwire into the enforcement path.** `scopeRelevantFiles` (`coder_gate.go:705`) feeds `checkScopeOverlap` and `enforceReviewerScopeOverlap`, which can demote a GO verdict to NO-GO (`scope_overlap.go:524`). Swapping that ranker changes a branch-blocking gate and needs its own falsification (a test that drives the drift-demotion branch through the new ranker), not this spike.

**The next step is a separate proposal**, whose scope is an advisory-only integration (the coder prompt prefix built by `applyRepoMapPrefix`, `executor_native.go:801`), never `scopeRelevantFiles`.

## Not answered by this spike

- **Deployability.** The spike ran on a macOS arm64 host. The integration target is inside the foreman images, whose whole design point is a static `CGO_ENABLED=0` Go binary on alpine (`Dockerfile.foreman-agent:19`), plus the macOS versioned-binary install (`make install-foreman-agent`). The installed ripwire binary is **50 MB, Mach-O arm64**. Shipping it means a per-arch build step and release assets for every fleet node. That is a real cost this spike does not price.
- **The frozen-Foreman quarter.** ROADMAP freezes new Foreman surface this quarter and caps Foreman at 25% of commits. An advisory repomap augmentation is not a new CRD surface, but it is Foreman work competing with the homelab wedge. The integration proposal has to face that sequencing gate.
- **The enforcement coupling risk.** Answered above as a constraint, not resolved as a design.

## Follow-up (Spike B): porting the signal into repomap

Hypothesis: repomap's lexical gap is closable with a pure-Go structural signal, so llmkube gets the win with no third-party binary. This was built, measured, and **reverted**: it does not work.

**What was built (on the branch, then reverted).** `extract.go` captured each file's Go import paths; a new `graph.go` resolved a workspace import to the directory's files by longest path-suffix match; `ScoreFiles` gained a second pass that lifted (added a bonus to) every file one undirected import hop from the top lexical seeds. Tests: `graph_test.go` (the resolver links a seeded import edge) and `score_test.go` (an import-adjacent file outranks an equally shallow lexical leader).

**Result: net harmful at both magnitudes.**

| repomap variant | strict file@10 |
|---|---|
| baseline (HEAD) | 8/25 (32%) |
| import-adjacency lift, bonus 30 | 4/25 (16%) |
| import-adjacency lift, bonus 4 | 5/25 (20%) |
| reverted (re-measured) | 8/25 (32%) |

Aggregate gold-file hits@10 moved the same way: 47 baseline, 32 at bonus 30, 42 at bonus 4.

**Diagnosis.** The lift is undirected and applied to every import neighbour of up to eight lexical seeds, so the top-K floods with adjacency winners. The genuinely-named gold files are usually the lexical seeds themselves, receive no lift, and get displaced by their own neighbourhood. A dominant bonus (30) and a tiebreaker bonus (4) both lose to baseline, so this is the signal's shape, not its magnitude. Undirected directory-import adjacency is not the call-graph signal ripwire uses, and it does not substitute for one.

**Falsification.** Each edit produced the predicted failure:
- `importReachBonus = 0` -> `TestScoreFiles_LiftsImportAdjacentFile` FAILED: `adj=2 far=0 order=[far, seed, adj]`.
- `buildImportGraph` returning an empty map -> `TestBuildImportGraph_ResolvesWorkspaceImport` and the score test FAILED.
- Revert the reachability pass -> the harness repomap arm returned to 8/25, isolating the delta and confirming the harness is run-to-run deterministic (15/25 ripwire both runs).

**Decision: killed.** The repomap change is reverted; the product tree is byte-identical to HEAD. The durable artifacts are the harness and this finding, not the code. If we want the localization win in pure Go, it needs a real call graph (cross-package symbol reachability), which is a project, not a spike; if we want it this quarter, Story A (ship the ripwire binary, advisory-only) is now the route with evidence behind it. The [Recommendation](#recommendation) above stands, with this follow-up narrowing it.

## Reproducing

```bash
# install (macOS arm64, no sudo, does not activate agent skills)
RIPWIRE_REPO=redhat-et/ripwire RIPWIRE_INSTALL_YES=1 RIPWIRE_NO_ACTIVATE=1 \
  bash -c "$(curl -fsSL https://raw.githubusercontent.com/redhat-et/ripwire/main/scripts/install.sh)"
export PATH="$HOME/.local/bin:$PATH"

# the eval (25 cases), and the two falsification runs
bash scripts/spike/ripwire_eval.sh 25 .
SPIKE_FORCE_QUERY="the quick brown fox" bash scripts/spike/ripwire_eval.sh 5 .
SPIKE_GOLD_FROM_RELEASE=1 bash scripts/spike/ripwire_eval.sh 5 .
```

The two files under `scripts/spike/` are throwaway. If this proposal is rejected, delete them. If accepted, they are superseded by the integration proposal's harness.
