# Reproducing release scan gates locally

Foreman can make a coder branch prove itself against the release CVE gate,
not just the source gate. When a task declares a `scanGate`, after every push
Foreman re-runs the `hack/scan-images.sh` gate in a clean-room Kubernetes Job:
it builds the release images the way GoReleaser does and Trivy-scans them.
Findings are fed back to the coder as a retry prompt, up to the Agent's
`maxScanIterations` bound; a branch that exhausts the bound settles
`INCOMPLETE / SCAN-GATE-FAILED` instead of slipping through.

The verify stage keeps its word honest: on a scan-declared branch, a
deterministic `GATE-PASS` triggers one more scan re-run, and only a clean
scan lets the `GATE-PASS` stand. `scripts/foreman-finalize.sh` reads that
verdict, so it refuses to finalize a scan-declared branch that never scanned
clean.

## Declaring a scan gate

`scanGate` lives on `AgenticTask.spec`, and — like `gateProfile` — on
`Workload.spec` as the default for every task a Workload decomposes into,
with `pipeline[].scanGate` overriding per step. Resolution is step →
workload → no scan gate; an unset gate on both is byte-identical to today's
behavior. Presence is the declaration signal: `scanGate: {}` — a present
gate with every field empty — scans **all** built-in targets at the CI
defaults, exactly like the `images` row of the table below promises; only
omitting the key entirely means "no gate".

Worked example — an issue-batch Workload whose tasks must keep the
foreman-agent release image free of fixable CRITICAL/HIGH CVEs:

```yaml
apiVersion: foreman.llmkube.dev/v1alpha1
kind: Workload
metadata:
  name: harden-foreman-agent
spec:
  repo: defilantech/LLMKube
  issues: [1812]
  coderAgentRef: { name: coder }
  verifierAgentRef: { name: gate }
  scanGate:
    images: ["foreman-agent"]   # built-in target id; empty means all four
    severity: ["CRITICAL", "HIGH"]
```

The retry budget is an Agent knob, `Agent.spec.maxScanIterations`: `nil`
means one fix round (a single human "the scan failed, fix it" iteration),
and an explicit `0` turns the loop off — the first scan failure is terminal.

Every `scanGate` field is optional. `ScanGate.Resolve` merges what you set
over the CI-aligned defaults:

| Field           | Default                                        |
|-----------------|------------------------------------------------|
| `images`        | empty = all built-in targets: `controller`, `foreman-operator`, `foreman-agent`, `router-proxy` |
| `severity`      | `CRITICAL,HIGH`                                |
| `ignoreUnfixed` | `true` (only findings with an available fix block the gate) |
| `builderImage`  | `golang:1.26`                                  |
| `runnerImage`   | `alpine:3.24`                                  |

The defaults are pinned byte-aligned with `hack/scan-images.sh` and guarded
by a parity test, so the local gate means the same thing as the CI gate it
stands in for.

## How the scan runs

The scan Job is a clean room: it clones the pushed branch (the fork, where
the branch actually lives), an initContainer builds each target binary with
the GoReleaser flags (`CGO_ENABLED=0`, static, `linux/amd64`, `-trimpath`),
and the scan container assembles each image with daemonless
`buildah --storage-driver=vfs --isolation=chroot` and runs Trivy over it. No
Docker daemon, no DinD socket, no privileged mode — a scan gate never needs
more power than the workload it scans.

Trivy is pinned (`v0.74.0`), fetched as the release asset of that exact tag
rather than through a mutable install script — the same supply-chain posture
as the gate's pinned helm. It is not in the Alpine repos, so the runner
downloads it into the cache on first use. The scan mounts the same cache
claim as the source gate (`--gate-cache-pvc`): the Go builds hit the shared
`GOMODCACHE`/`GOCACHE`, so a warm gate cache warms the scan too.

Verdicts are three-valued, the same infra-vs-branch split as the source
gate: `SCAN-PASS` (clean), `SCAN-FAIL` (findings remain — the log tail is
fed back to the coder), and `SCAN-ERROR` (the scan could not be run at all,
including a deadline kill).

## Node requirements (honest limits)

The Job assembles `linux/amd64` images because that is what GoReleaser
publishes. A scan-runner node must therefore be `linux/amd64`, or have
`binfmt`/QEMU registered so amd64 builds run under emulation.

If your cluster cannot run the Job at all, the gate degrades honestly:
`SCAN-ERROR` means *could not verify*, never *clean*. On the first push the
GO stands (there is no evidence of a finding yet); on a retry it does not —
a fix nobody confirmed cannot land as a false GO, so the task downgrades.
And a verify `GATE-PASS` on a scan-declared branch that cannot be re-scanned
becomes `GATE-ERROR`, which finalize refuses. A cluster without scan
capacity never turns into a silent pass; wire up amd64 runners (or QEMU)
when you want the gate to bite.

## See also

- [Non-Go projects (language gates)](./language-gates): the source-gate analogue of this page.
- [M4 verifier runbook](./runbook-m4): standing up the verify gate on a node.
- `hack/scan-images.sh`: the CI release scan this gate reproduces.
