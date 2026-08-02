# Multi-GPU inference is unexpectedly slow on NVLink hardware

Multi-GPU serving works, produces correct output, and shows no errors anywhere,
but throughput is far below what the hardware should deliver. Tensor-parallel or
layer-sharded models are the usual victims.

The most common cause on NVLink systems (HGX/DGX B200, GB200, and NVSwitch
platforms generally) is a **Fabric Manager / driver version mismatch**. When the
`nvidia-fabricmanager` package version does not exactly match the installed
NVIDIA driver, the fabric fails to initialize and GPU-to-GPU traffic **silently
falls back to PCIe**. Nothing crashes. Nothing logs an error at the LLMKube
layer. You just lose most of your interconnect bandwidth.

## Trigger

Any of:

- Multi-GPU throughput well below expectation, with no errors in the
  InferenceService pod, the operator, or `dmesg`.
- `nvidia-smi nvlink -s` reports links inactive or missing on a system that has
  NVLink.
- The `nvidia-fabricmanager` service is not running, or is in a crash loop, on a
  node that has NVSwitch.
- A node was recently upgraded (driver, DGX OS, or GPU Operator) and multi-GPU
  performance regressed afterward. Driver upgrades that do not upgrade Fabric
  Manager in lockstep are the usual way this is introduced.

## Diagnose

1. **Compare the two versions. They must match exactly.**

   ```bash
   # Installed driver
   nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1

   # Fabric Manager package version
   dpkg -l | grep nvidia-fabricmanager     # Debian/Ubuntu, incl. DGX OS
   rpm -qa | grep nvidia-fabricmanager     # RHEL family
   ```

   A driver of `580.173.02` requires Fabric Manager `580.173.02`. Not
   `580.126.20`, not "the 580 branch". Exact.

2. **Check the service state.**

   ```bash
   systemctl status nvidia-fabricmanager
   journalctl -u nvidia-fabricmanager --no-pager | tail -40
   ```

   A version mismatch typically shows here as a startup failure mentioning the
   driver version, after which the service stays down.

3. **Check fabric state as the GPUs report it.**

   ```bash
   nvidia-smi -q | grep -i -A4 fabric
   nvidia-smi nvlink -s
   ```

   Healthy NVSwitch systems report a completed fabric state and active links.
   Inactive links on NVLink-capable hardware is the confirmation.

4. **Confirm the traffic is actually taking PCIe.** If `dcgm-exporter` is
   running with Blackwell counters enabled, NVLink per-link throughput fields
   (DCGM 4.6.0+, field IDs 1525-1533) stay flat at zero under a multi-GPU load
   while the job clearly runs.

## Mitigate (immediate)

Stop scheduling multi-GPU work onto the affected node until the fabric is
healthy. Single-GPU serving is unaffected and can stay up.

```bash
kubectl cordon <node>
# or scale the affected multi-GPU InferenceService to zero
kubectl patch inferenceservice <name> --type merge -p '{"spec":{"replicas":0}}'
```

Leaving the workload running is not dangerous, it is just quietly slow, so this
is about not spending GPU hours at a fraction of expected throughput.

## Resolve (structural)

1. **Install the Fabric Manager build that matches the driver exactly**, then
   restart it before the workload:

   ```bash
   sudo apt-get install -y nvidia-fabricmanager-<branch>=<exact-driver-version>
   sudo systemctl enable --now nvidia-fabricmanager
   sudo systemctl status nvidia-fabricmanager
   ```

2. **Keep them locked together across upgrades.** The mismatch is almost always
   introduced by upgrading one and not the other. On DGX OS, upgrade the driver
   and Fabric Manager as a unit. When the GPU Operator manages the driver, let
   it manage Fabric Manager too rather than installing a host package alongside.

3. **Verify the platform floors** rendered in the chart's `NOTES.txt`
   (`platformFloors`) for the driver, CUDA, NCCL, and GPU Operator minimums the
   node should satisfy.

## Verify

```bash
# 1. Versions match
nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1
dpkg -l | grep nvidia-fabricmanager

# 2. Service healthy
systemctl is-active nvidia-fabricmanager

# 3. Links active
nvidia-smi nvlink -s
```

Then uncordon and re-run the workload: multi-GPU throughput should return to
expected levels, and NVLink throughput counters should move under load.

```bash
kubectl uncordon <node>
llmkube benchmark <service> --output table
```

## Related

- Chart values: `platformFloors.fabricManager` in `charts/llmkube/values.yaml`,
  rendered into `NOTES.txt` at install time.
- #1374: platform floors correction that added this runbook.
- #413: Blackwell (B200 / sm_100) enterprise readiness, which identifies a
  Fabric Manager mismatch as the highest-priority silent-failure mode on
  Blackwell.
- NCCL is unsupported with MIG, so a MIG-partitioned node will not use NVLink
  between instances regardless of fabric health. That is expected, not this bug.
