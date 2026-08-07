# AMD GPU metric contract

The pinned metric names for the AMD tier (issue #700, slices #1185/#1187).
Dashboards and alerts key off these names; changing them is a breaking change
to this contract and must update `charts/llmkube/dashboards/amd-gpu-observability.json`
in the same PR.

## Source: amdgpu-exporter (DaemonSet, `config/monitoring/amdgpu-exporter.yaml`)

`ghcr.io/defilantech/llmkube-amdgpu-exporter` (this org's `llmkube-runtimes`
repo) reads amdgpu **sysfs/hwmon** directly — no `rocm-smi`/ROCm userspace
required, which is what makes it work on Vulkan-first boxes (gfx1151) where
the datacenter exporters (`device-metrics-exporter`, `amd_smi_exporter`) do
not enumerate the iGPU.

Every per-GPU series carries `card` (e.g. `card0`) and `pci_slot`
(e.g. `0000:c5:00.0`). **`pci_slot` is the stable join/alert key** — card
indices can move across reboots.

| Metric | Unit | Notes |
| --- | --- | --- |
| `amdgpu_gpus_discovered` | count | AMD DRM devices found (vendor `0x1002`) |
| `amdgpu_gpu_info` | 1 | static labels: device/subsystem/revision ids, product name |
| `amdgpu_gpu_busy_percent` | % | GPU engine busy |
| `amdgpu_memory_busy_percent` | % | memory controller busy (absent on some APUs) |
| `amdgpu_vram_used_bytes` / `amdgpu_vram_total_bytes` | bytes | dedicated VRAM |
| `amdgpu_visible_vram_used_bytes` / `_total_bytes` | bytes | CPU-visible VRAM |
| `amdgpu_gtt_used_bytes` / `amdgpu_gtt_total_bytes` | bytes | **GTT — on APUs the model weights live here**, this is the key memory signal |
| `amdgpu_temperature_celsius{sensor}` | °C | hwmon temp sensors (`edge`, …) |
| `amdgpu_power_watts{sensor,type}` | W | hwmon power (`type`: `average`/`cap`/…) |
| `amdgpu_clock_hertz{sensor}` | Hz | hwmon freq sensors; **`sensor="sclk"` is the core clock**. `mclk` is not exposed via hwmon on gfx1151 (only `pp_dpm_mclk`); memory clock is a known gap |
| `amdgpu_voltage_volts{sensor}` | V | hwmon voltage |
| `amdgpu_fan_rpm{sensor}` / `amdgpu_fan_pwm{sensor}` | rpm / raw | absent on fanless boxes |
| `amdgpu_pcie_replay_total` | counter | PCIe replay events |
| `amdgpu_scrape_success` / `amdgpu_scrape_failures_total` / `amdgpu_last_scrape_duration_seconds` | — | exporter health |

Per-metric failures are isolated: a missing sysfs attribute skips that one
metric, never the scrape. A node with zero AMD GPUs serves an empty-but-healthy
`/metrics` (`amdgpu_gpus_discovered 0`).

## Cross-check: node-exporter hwmon

node-exporter's hwmon collector independently exposes the same sensors as
`node_hwmon_temp_celsius` / `node_hwmon_power_watt`, joinable on
`node_hwmon_chip_names{chip_name="amdgpu"}`. The dashboard's health row uses
these (verified on gfx1151); the `amdgpu_*` equivalents are the per-GPU-labeled
versions of the same readings.

## Inference signals (slice #1186)

<!-- metrics-contract:begin -->
SLO metrics come from llama.cpp `/metrics` (`--metrics`), not this exporter:
`llamacpp:predicted_tokens_seconds`, `llamacpp:prompt_tokens_seconds`,
`llamacpp:requests_processing`. Backend-agnostic and already emitted.
<!-- metrics-contract:end -->

## Known gaps (deliberate)

- Memory clock (`mclk`): only available via `pp_dpm_mclk` text parsing; not
  yet exported.
- Per-process attribution: not available from sysfs; would need ROCm userspace.
- Instinct/MI datacenter tiers: use `ROCm/device-metrics-exporter` when that
  hardware exists; this exporter targets the iGPU/APU class it was built on.
