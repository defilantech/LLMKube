# llama.cpp metric contract

The pinned metric names for the inference tier (issue #700, slice #1186).
Dashboards and alerts key off these names; changing them is a breaking change
to this contract and must update `charts/llmkube/dashboards/llamacpp-dashboard.json`
in the same PR, which is where every `llamacpp:` query lives.

Backend-agnostic: these come from llama.cpp itself, so they are identical on
CUDA, ROCm and Vulkan servers. The AMD-specific device metrics are a separate
contract — see `amd-gpu-metrics.md`.

## Source: llama.cpp `/metrics`

Requires the server to run with `--metrics`. Scraped by the chart's
`inference-podmonitor.yaml` (`prometheus.inferencePodMonitor.enabled`, on by
default), which selects on the operator's `inference.llmkube.dev/service` label
and relabels every series with `service`, `model`, `runtime` and `namespace`, so
a panel can break down per model without naming a target.

<!-- metrics-contract:begin -->

### Throughput

| Metric | Type | Unit | Notes |
| --- | --- | --- | --- |
| `llamacpp:predicted_tokens_seconds` | gauge | tokens/s | **decode** throughput — generation |
| `llamacpp:prompt_tokens_seconds` | gauge | tokens/s | **prefill** throughput — prompt processing |

Keep these separate rather than presenting one "tokens per second". Prefill is
compute-bound and decode is memory-bandwidth-bound, so they degrade
independently, and a combined figure hides the case where one collapses while
the other holds. Observed in practice: a server whose decode fell from 56 to
5 tokens/s while prefill stayed at its usual rate — a single averaged number
would have read as a mild dip rather than a 10× regression.

### Saturation

| Metric | Type | Unit | Notes |
| --- | --- | --- | --- |
| `llamacpp:requests_processing` | gauge | count | requests in flight |
| `llamacpp:requests_deferred` | gauge | count | requests queued behind a full slot set — **the queueing signal** |
| `llamacpp:n_busy_slots_per_decode` | gauge | count | average busy slots per `llama_decode()` call |

`llamacpp:requests_deferred` is what distinguishes "the server is busy" from "the server
is oversubscribed": it goes above zero only once every parallel slot is
occupied. Alert on it rather than inferring queueing from latency.

### Totals

| Metric | Type | Notes |
| --- | --- | --- |
| `llamacpp:n_decode_total` | counter | decode calls |
| `llamacpp:tokens_predicted_total` / `llamacpp:tokens_predicted_seconds_total` | counter | generated tokens and time |
| `llamacpp:prompt_tokens_total` / `llamacpp:prompt_seconds_total` | counter | prompt tokens and time |
| `llamacpp:prompt_tokens_cached_total` | counter | prompt tokens served from cache — prefix-cache effectiveness |
| `llamacpp:n_tokens_max` | counter | largest observed sequence length (prompt + generation) |
| `llamacpp:spec_decode_num_drafts_total`, `llamacpp:spec_decode_num_draft_tokens_total`, `llamacpp:spec_decode_num_accepted_tokens_total` | counter | speculative decoding; emitted unconditionally, at zero, on a server with no draft model |
| `llamacpp:spec_decode_num_accepted_tokens_per_pos_total` | counter | speculative decoding, labelled by position; the one spec_decode counter that appears only when a draft model is configured |

<!-- metrics-contract:end -->

## Minimum llama.cpp build

Five of the names above are newer than many shipping servers. `prompt_tokens_cached_total`
arrived in `decaf508b` (2026-08-13) and the four `spec_decode_*` counters in
`a035a8887` (2026-08-05), so a server older than mid-August 2026 emits neither
group and a panel keyed on them stays blank rather than erroring. Everything
else in this contract predates that.

## Known gaps (deliberate)

- **KV-cache occupancy is not exposed.** There is no `kv_cache_usage_ratio` or
  equivalent in llama.cpp's `/metrics`. Slice #1186 originally pinned that name
  along with `llamacpp:tokens_per_second`; neither exists. `n_busy_slots_per_decode`
  is a reasonable saturation proxy but measures slot occupancy, not cache
  residency, and should be labelled as such rather than substituted.
- **Per-request attribution**: the metrics are per-server aggregates. Attributing
  throughput to a caller needs the proxy in front of the server, not this endpoint.
- **Context-window pressure**: `n_tokens_max` is a high-water mark, not a
  distribution, so it cannot show how often requests approach the limit.

## Verification

Every metric above was read from a live server rather than from documentation.
Confirmed on a Strix Halo (gfx1151) node and four other InferenceServices, all
scraped through the chart's PodMonitor; types and descriptions are the ones the
server reports in its own metric metadata. The `spec_decode_*` split was checked
against a build `b10795` server started without `--spec-type`, which emits the
first three counters at zero and omits
`llamacpp:spec_decode_num_accepted_tokens_per_pos_total` entirely.
