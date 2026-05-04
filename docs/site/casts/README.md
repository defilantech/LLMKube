# Terminal recordings (asciinema casts)

The `.cast` files in this directory are [asciinema](https://asciinema.org) v3 recordings embedded in the docs site at https://llmkube.com/docs. Each cast pairs with one or more doc pages to show a workflow that's hard to convey in text.

When the docs site builds, llmkube-web copies these `.cast` files into its `static/casts/` directory and embeds them via the `AsciinemaCast` Svelte component, which loads `asciinema-player` on the client.

## Conventions used by every cast in this repo

- **Format:** asciinema v3 (`.cast` JSON)
- **Terminal:** `cols=110, rows=32` (matches our doc-site layout)
- **Idle compression:** `idle_time_limit=2.0` (any wait longer than 2 s is shrunk to 2 s during playback)
- **Theme:** `dracula` (configured client-side in `AsciinemaCast.svelte`, not the cast itself)
- **Shell:** `zsh` with a clean prompt (no PS1 timestamps, no version managers in the title)
- **No autoplay:** every embed waits for the user to click play, and respects `prefers-reduced-motion`

These conventions exist so casts feel consistent on the site. Don't deviate without a reason.

## Recording a new cast

Casts are recorded **manually** via `asciinema rec`. There is no Makefile target — the value is in shaping the demo, not automating the capture.

The recipe:

1. **Write a driver script first.** A shell script that runs the exact sequence of commands you want to show. The script lives outside this repo in `llmkube-internal/marketing/demo-script-*.sh` (or wherever you keep marketing scripts). The script is what `asciinema rec` will execute, not your live shell. Driver-script-first beats live recording: you can re-record cleanly, you can iterate on the narrative, you can pre-stage the cluster state.
2. **Pre-stage everything that takes minutes.** Pull the model. Apply manifests. Wait for the GPU. The cast should show *meaningful* state changes, not three minutes of `Pending`.
3. **Use prompt markers and dim notes** so the viewer can follow along. Shell colors render fine in asciinema. See `marketing/demo-script-memory-pressure.sh` (in llmkube-internal) for the established prompt/note pattern.
4. **Resize your terminal to 110×32 before recording.** `asciinema` captures whatever cols/rows the terminal advertises.
5. **Run:**
   ```bash
   asciinema rec --idle-time-limit 2 --command /path/to/your/driver.sh new-feature.cast
   ```
6. **Trim the cast** if needed. The `asciinema rec` output sometimes has stray sequences at the very start (clear-screen escapes) — that's fine and intentional. Don't post-process the file unless the recording genuinely failed.
7. **Commit the `.cast` to this directory** alongside (or in the same PR as) the doc page that embeds it. Cast and doc should ship together.

## Embedding in a doc

In `docs/site/<some-page>.md`:

```mdx
<AsciinemaCast
  src="/casts/your-recording.cast"
  title="Real-time recording on a kind cluster, idle waits compressed."
/>
```

The `AsciinemaCast` Svelte component is registered globally in the renderer and is available inside any markdown file rendered to the site. `src` is relative to the site root (`/casts/<file>.cast`), not to the markdown file.

## Re-recording an existing cast

`.cast` files are JSON text and only ~5 KB raw, so over-writing is cheap and re-recording is the right reflex when the workflow it shows changes (new flags, new output format, etc.). One re-record overwrite costs ~1.7 KB in git history — negligible.

If a re-record produces a substantively different demo (different topic, different audience), give it a new filename and link it from a different doc page rather than overwriting.

## Why we don't use animated GIFs

- GIFs are 50–500× larger.
- GIFs aren't selectable (you can't copy a command out of the demo).
- GIFs don't pause, scrub, or restart.
- GIFs don't respect `prefers-reduced-motion`.

asciinema beats GIFs on every dimension that matters for documentation.
