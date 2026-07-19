# LLMKube documentation

This directory contains all the markdown for LLMKube. It is split by audience:

| Path | Audience | Rendered to |
|---|---|---|
| `docs/site/` | End users | https://llmkube.com/docs |
| `docs/contributors/` | People hacking on the operator itself | Repo only |
| `docs/images/` | Diagrams and screenshots used by both | Both |
| `docs/releases/` | Per-release notes | Repo only |

## `site/` — user-facing docs

Anything in `docs/site/**/*.md` is the source of truth for a page on the docs site. The site reads this directory at build time, applies the brand layout, and renders it. The sidebar order lives in `docs/site/_meta/nav.yaml`.

Site pages can embed Svelte components like `<DocCallout>` and `<AsciinemaCast>` directly inside the markdown — they're auto-registered by the renderer.

Terminal recordings live in `docs/site/casts/`. See `docs/site/casts/README.md` for the recording recipe and conventions.

## `contributors/` — internal docs

Reference material for working on the operator itself: how to add a new runtime backend, how to spec the HuggingFace source format, internals of the vLLM image pipeline. These pages are read by people *changing* LLMKube, not people *using* LLMKube — they aren't rendered to the public site.

## `observability/`: SLOs and error budgets

- [SLOs and error budgets](observability/slo.md)

## During the transition

A few user-facing markdown files still live at the root of `docs/` (`MULTI-GPU-DEPLOYMENT.md`, `air-gapped-quickstart.md`, etc.). They will move into `docs/site/` as they're ported — each port is its own commit/PR so the rendered site can light up one section at a time.

When you see a file at the root of `docs/`, treat it as either (a) about to be moved into `site/` or `contributors/`, or (b) something specific to the repo (like this README). If you're not sure where a new doc should go, default to `site/` if a user might reach for it and `contributors/` if only a maintainer would.
