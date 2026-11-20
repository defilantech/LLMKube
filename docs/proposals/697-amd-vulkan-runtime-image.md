# Proposal: Owned AMD/Vulkan llama.cpp runtime image and build pipeline

**Status:** Proposed (design)
**Umbrella issue:** [#696](https://github.com/defilantech/LLMKube/issues/696) (AMD accelerator tier epic)
**Primary issue:** [#697](https://github.com/defilantech/LLMKube/issues/697) (AMD llama.cpp runtime image, Vulkan/RADV)
**Related:** [#698](https://github.com/defilantech/LLMKube/issues/698) (node runbook), [#699](https://github.com/defilantech/LLMKube/issues/699) (validated example + benchmark), [#725](https://github.com/defilantech/LLMKube/issues/725) (broken upstream floating tag)
**Validated against:** llama.cpp Vulkan backend on `gfx1151` (Strix Halo / Radeon 8060S), Mesa RADV 26.x, verified end-to-end (Llama-3.2-3B Q4_K_M, 29/29 layers offloaded, ~87 tok/s decode).

This document is the design reference for building and maintaining LLMKube's own AMD/Vulkan inference runtime image in a dedicated repository with a hardware-gated CI pipeline. It captures the motivation, the build and promotion architecture, the security model for running candidate images on a self-hosted GPU host, and how the operator consumes the result.

---

## 1. Problem and motivation

Today LLMKube builds **zero inference runtime images.** `goreleaser` builds only the project's Go images (`llmkube-controller`, `llmkube-router-proxy`, and at the time the agentic pipeline's operator and agent, under `ghcr.io/defilantech/*`). The actual serving runtime is inherited wholesale from upstream floating tags:

- `internal/controller/runtime_llamacpp.go` defaults to `ghcr.io/ggml-org/llama.cpp:server`.
- CLI/benchmark paths reference `:server-cuda13`, `:server-intel`, `:server-rocm`.

This means the load-bearing component of the product, the thing that actually runs the model, is an uncontrolled upstream supply chain. [#725](https://github.com/defilantech/LLMKube/issues/725) made the cost concrete: the upstream floating `:server-vulkan` tag (build 9641) shipped a `libggml-vulkan.so` with an undefined precompiled-shader symbol (`matmul_id_subgroup_q6_k_f32_dot2_aligned_f16acc_data`). Its `dlopen` fails fatally, ggml's backend loader silently drops the Vulkan backend, and llama-server falls back to CPU while printing the misleading "no usable GPU found." The host, RADV, libvulkan, and ICD were all correct: only the image was broken, and we could neither fix it nor detect it ahead of a hand-run on real hardware.

Mirroring a known-good upstream tag (the manual `server-vulkan-b5974` workaround) only half-solves this: we would still be picking from upstream builds we cannot repair. To make AMD a first-class tier (#696) we want to **own the build of the Vulkan runtime**: control the shader-gen step, the base image, dependency and CVE patching, and crucially a release gate that actually exercises a GPU.

Scope of this proposal is deliberately narrow: the **AMD/Vulkan** runtime only. The repository and pipeline are structured so the same pattern lifts to other backends later without redesign, but we are not building CUDA/Intel/CPU images here.

## 2. North-star architecture

A dedicated public repository owns the runtime image; a hardware-in-the-loop promotion gate decides what becomes trusted; the LLMKube operator consumes a pinned, signed tag.

```
  defilantech/llmkube-runtimes (new repo)            self-hosted gfx1151 host
  ┌─────────────────────────────────────┐            ┌──────────────────────────────┐
  │ vulkan/Dockerfile (pinned llama.cpp  │  push      │ promoter (systemd timer):     │
  │   SHA, -DGGML_VULKAN=ON, slim Debian │ :candidate │  1. cosign verify provenance  │
  │   + mesa RADV + libvulkan1)          │ ─────────► │  2. sandboxed GPU smoke       │
  │ GitHub Actions:                      │   (ghcr)   │  3. on pass: promote + sign   │
  │  - build (GitHub-hosted, no GPU)     │            └───────────────┬───────────────┘
  │  - cheap gate: backend .so resolves  │                            │ promotes + signs
  │  - SBOM + keyless provenance sign     │                           ▼
  └─────────────────────────────────────┘            ghcr.io/defilantech/llmkube-llama-vulkan
                                                       :stable  ·  :b<upstream>-llmkube<N>
                                                                  │ pinned tag/digest
                                                                  ▼
                                              LLMKube operator: vendor=amd -> DefaultImage()
```

Two principles drive the design:

1. **The build needs no GPU; the gate does.** Vulkan shader-gen is CPU-only amd64 work, so the build runs on free GitHub-hosted runners. Only the smoke test that proves real offload needs the `gfx1151`. We never put the GPU host on the GitHub Actions attack surface.
2. **Trust is earned by passing a hardware gate, not by being built.** A built image is a *candidate*; only an image that a real GPU host has verified and signed is promoted to the tag the operator consumes.

## 3. The repository and image

- **Repo:** `defilantech/llmkube-runtimes` (plural/generic so CUDA, Intel, etc. can be added as sibling directories later without a new repo). Initial contents: `vulkan/`.
- **Image:** `ghcr.io/defilantech/llmkube-llama-vulkan` (the name #697's example already assumes).
- **Dockerfile (`vulkan/Dockerfile`):** multi-stage.
  - *Builder:* pinned `llama.cpp` commit (by SHA, not a moving branch), `cmake -DGGML_VULKAN=ON` with `vulkan-shaders-gen`. We own the shader-gen step that #725 broke upstream.
  - *Runtime:* slim Debian base pinned by digest, `mesa-vulkan-drivers` (RADV) + `libvulkan1`, no ROCm. Non-root by default. A few hundred MB, vs 6-12 GB for a ROCm image.
- The image consumes the GPU by mounting `/dev/dri` device nodes via the device-plugin resource (`devic.es/dri-render`, exposing **both** `renderD128` and `card1`; see #698). It requests no `nvidia.com/gpu`.

## 4. The two-tier gate

The defining insight from #725: the undefined-symbol failure is a **dlopen/link error, not a device error**: it fires before any GPU enumeration. That lets us catch the entire #725 class cheaply, then confirm real offload on hardware.

**Tier 1, cheap, GitHub-hosted (no GPU):** after build,
- run `llama-server --list-devices` under the image's software Vulkan (llvmpipe). A `libggml-vulkan.so` with an unresolved symbol fails here exactly as #725 did, on a free runner, before the image ever reaches the GPU host.
- assert no unresolved dynamic symbols in the backend `.so` (`ldd -r` / symbol-resolution check).
- on pass, push only `:candidate-<gitsha>`, generate an SBOM (syft), and sign build provenance (cosign keyless, GitHub OIDC).

**Tier 2, real, out-of-band on a self-hosted `gfx1151` host:** the promoter
- pulls `:candidate-*`, **verifies provenance first** (see Section 5),
- runs a tiny baked-in model with `-ngl 99` in a sandbox (Section 5) and asserts: `Vulkan0` device present, layers actually offloaded, a completion returns, and decode throughput clears a floor (e.g. >= 50 tok/s, which guards against silent CPU fallback),
- on pass, promotes to `:b<upstream-build>-llmkube<N>` (immutable) and advances `:stable`, then applies a *smoke-passed* signature (Section 5).

## 5. Security model: running candidate images on the GPU host

Choosing an out-of-band promoter over a self-hosted Actions runner already removes the single biggest risk: **fork-PR code never runs on the GPU host at all.** What remains is "the promoter pulls and runs an image," handled by three nested boundaries.

### 5.1 Provenance: only run images proven to be built by our CI
This is the primary control; the rest is defense in depth.

- The build-and-push-candidate workflow triggers **only on push to `main` and tags, never on `pull_request` from forks.** Fork PRs get a build-only validation job with no registry credentials and no push. A candidate image exists only because reviewed code on `main` produced it.
- CI signs each candidate with **cosign keyless** (GitHub OIDC); the signature is bound to the workflow identity (`github.com/defilantech/llmkube-runtimes` at a SHA on a trusted ref) and cannot be forged: there is no key to steal.
- **Before running anything**, the promoter runs `cosign verify` pinning `--certificate-identity-regexp` to the workflow SAN and `--certificate-oidc-issuer https://token.actions.githubusercontent.com`. No valid provenance, no execution.

Net: an attacker who pushes a `:candidate-evil` tag to the ghcr namespace is ignored; the only path to a runnable candidate is a merge into `main` past branch protection, which is the same trust bar as the operator code itself. No new trust boundary is introduced.

### 5.2 Sandbox: run it as if hostile anyway
A compromised build dependency could still produce a bad binary, so the smoke runs under tight confinement (a throwaway Kubernetes Job, or `podman run --rm`):

- non-root, **read-only rootfs**, `--cap-drop=ALL`, `no-new-privileges`, default seccomp;
- **no host mounts** except the GPU device nodes (`/dev/dri/renderD128` + `card1`): no host filesystem, no container socket, no kubeconfig/kubelet credentials;
- **egress denied** via NetworkPolicy; the smoke model is baked into the test image or a read-only pre-staged volume so the run is fully offline;
- **resource-bounded** with an `activeDeadlineSeconds` timeout, and **ephemeral** (`ttlSecondsAfterFinished`, fresh pull each run);
- the Job's service account has near-zero permissions (no secret access), so even a container escape into the API server gets nothing.

### 5.3 Key isolation: the smoke can never sign itself or touch the host
- The *smoke-passed* signing key lives **in the promoter process, never inside the candidate container** (no shared volume, env, or socket). On the host that is a `0600` key owned by a dedicated `llmkube-promoter` user the smoke container cannot read (a hardware token is a stronger option).
- Order matters: pull -> verify provenance -> launch sandboxed smoke -> read pass/fail -> **only then** sign and promote, in the promoter's own context after the container has exited. The executed code is dead before the key is used.

Consumers (and `:stable` promotion) verify the *smoke-passed* signature, so an unsmoked or broken image is never trusted even if it carries valid build provenance. This reuses the keyless cosign identity pattern planned for the fleet-update work (PR6 in the fleet agent auto-update plan), so the signing primitives are set up once.

## 6. Tag scheme and operator consumption

- **Tags:** `:candidate-<gitsha>` (pre-smoke, unsigned-for-use), `:b<upstream-build>-llmkube<N>` (immutable, promoted, smoke-passed), `:stable` (moving, the promoter advances it).
- **The operator pins an explicit immutable tag or digest in code**, never `:stable` (reproducibility). A vendor-aware image selection returns the pinned `llmkube-llama-vulkan` tag when `hardware.gpu.vendor == amd`. The CLI `--image` flag still overrides. This wiring is the concrete #697 code deliverable.
- Bumping the pinned default is a reviewed PR in the LLMKube repo; renovate can later watch our own ghcr to open those PRs automatically.

## 7. Cadence and supply chain

- We do not chase upstream's roughly daily builds. A **weekly scheduled** job builds the latest upstream release tag into a candidate and lets the promoter smoke it, giving early warning of upstream breakage without forcing adoption. Actual default-image bumps ride LLMKube releases.
- Every candidate carries an **SBOM** (syft) and **build provenance** (buildx attestation / cosign keyless). The upstream `llama.cpp` commit is pinned by SHA and the base image by digest, so builds are reproducible and an upstream force-push cannot silently change what we compile.

## 8. Decomposition (for the implementation plan)

1. **Repo bootstrap:** create `defilantech/llmkube-runtimes`, `vulkan/Dockerfile`, pinned llama.cpp SHA + base digest; build locally, push a first `:candidate` by hand.
2. **CI build + Tier-1 gate:** GitHub Actions build on push/tag, software-Vulkan `--list-devices` + symbol-resolution check, SBOM, keyless provenance signing, push `:candidate-<sha>`.
3. **Promoter (Tier-2 gate):** the sandboxed smoke Job manifest (offline, locked-down) + the promoter (systemd timer) that verifies provenance, runs the smoke, promotes, and signs. Documented setup for the self-hosted host.
4. **Operator consumption (#697 code):** vendor-aware `DefaultImage()` returns the pinned image; helm/values surface an override; tests.
5. **Validated example + benchmark (#699):** checked-in `Model` + `InferenceService` using the new image; documented decode/prefill numbers, including a larger MoE showcase for the unified-memory pool.

## 9. Alternatives considered

- **Mirror-and-pin upstream** (re-tag a known-good `:server-vulkan-bNNNN` after a smoke test): cheap, ~3-line Dockerfile, low maintenance, but cannot *fix* a broken build, only pick a different one. If a run of upstream builds is all broken we are stuck. Rejected as the long-term answer; it is effectively what the manual `b5974` workaround already does.
- **Self-hosted GitHub Actions runner for the GPU gate:** tighter status integration, but puts the GPU host on the Actions attack surface, where fork-PR gating must be airtight forever. Rejected in favor of the out-of-band promoter.
- **Build all llama.cpp backends now (CUDA/Intel/CPU):** larger payoff (removes the #725 risk everywhere) but more base images, runners, and CI up front. Deferred; the repo layout leaves room for it.
