---
title: Linux Host Agent (experimental)
description: Run the metal-agent on a Linux arm64 or amd64 host, joined to the cluster by hand, serving models through a daemon engine such as Ollama.
---

# Linux Host Agent (experimental)

The [Metal agent](/docs/guides/macos-metal) also starts on Linux, on arm64 and
amd64. It joins the cluster the same way a Mac does (a `MetalNode`, a Lease
heartbeat, the dial-back check and the in-cluster relay), and serves models
through an engine on the host.

Models served by a Linux host use `hardware.accelerator: metal`, like a Mac's:
`metal` is what sends a service to a host agent instead of in-cluster pods. On
Linux the agent logs `Metal not available; Linux host support is
experimental` at startup, then whether it found a Hailo device node
(`/dev/hailo*`). Any other host without Metal still exits.

Tracking issue: [#2481](https://github.com/defilantech/LLMKube/issues/2481).

## What has been tested

| | Status |
|---|---|
| linux/arm64 agent, `runtime: ollama` with stock Ollama on CPU | Tested in kind: an agent container next to stock Ollama, joined by hand, reached `Reachable=True` and served chat completions through the relay |
| Linux arm64 host with a 16K-page kernel (Raspberry Pi 5) | The agent runs on Raspberry Pi OS's `6.18.50+rpt-rpi-2712` kernel and served a kind cluster on the same LAN |
| linux/amd64 | Builds. Untested on hardware |
| llama.cpp (`llama-server`) on Linux | Untested. The agent's llama.cpp defaults (flash attention, mlock) are tuned for Apple Silicon |
| `llmkube metal join` | Refuses to run off macOS. [Join the host by hand](#join-the-host-by-hand) |
| `MetalNode` hardware | Not reported off macOS: `kubectl get metalnode` shows no memory or chip for a Linux host |
| `llmkube deploy` for a Linux host | Untested. Write the Model and InferenceService by hand |

## Get the agent

There is no Linux release archive yet. Build the agent from a checkout, for
the host's architecture (`arm64` or `amd64`):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o llmkube-metal-agent ./cmd/metal-agent
```

The binary is static and needs nothing else from the host. Copy it to the
host and put it on `PATH`.

## Keep hosts apart

Every agent watches one namespace (`--namespace`) and considers every `metal`
service in it, so keep a Linux host's services apart from other hosts':

- **Use a namespace of its own (recommended).** Create the host's identity in
  that namespace (`llmkube metal token create edge-1 -n edge`) and put its
  Models and InferenceServices there. No other agent sees them.
- **Or allowlist every agent in a shared namespace**, Macs included, with
  `--inference-service-allowlist` (comma-separated InferenceService names).
  An allowlist only limits the agent it is set on. An agent without one
  claims the Linux host's services too: every agent registers the Ollama
  runtime, so another host running Ollama can quietly serve the registry's
  copy of the same tag, and one without it refuses the service and writes
  status over the Linux host's.

Making placement on a `MetalNode` explicit is proposed in
[#2481](https://github.com/defilantech/LLMKube/issues/2481).

## Join the host by hand

`llmkube metal join` runs only on macOS, so on Linux you create the host's
identity as for a Mac, mint its token yourself and write its kubeconfig.

Where you have cluster access, create the namespace and the host's identity.
This creates its ServiceAccount (`llmkube-metal-<name>`), RBAC, Lease and
`MetalNode`, plus a join Secret:

```sh
kubectl create namespace edge
llmkube metal token create edge-1 -n edge
```

Ignore the `llmkube metal join` line it prints. Delete the unused join Secret,
which revokes that join string, and mint the agent's token directly:

```sh
kubectl -n edge delete secret llmkube-metal-edge-1-join
kubectl -n edge create token llmkube-metal-edge-1 \
  --duration=168h > token
chmod 600 token
```

Write a kubeconfig that reads that token file, with an API server address the
host can reach and the cluster's CA. `kubectl config view --raw --minify`
shows both for your current context; a kind cluster's `127.0.0.1` address is
not reachable from another host.

```yaml
apiVersion: v1
kind: Config
clusters:
- name: llmkube
  cluster:
    server: https://<api-server>:6443
    certificate-authority-data: <base64 CA>
users:
- name: edge-1
  user:
    tokenFile: /path/to/token
contexts:
- name: edge-1
  context: {cluster: llmkube, user: edge-1, namespace: edge}
current-context: edge-1
```

Copy both files to the host, readable only by the user the agent runs as
(0600), and start the agent as that user (not root) under the supervisor of
your choice, such as a systemd unit or a container:

```sh
llmkube-metal-agent \
  --kubeconfig /path/to/kubeconfig \
  --namespace edge \
  --metal-node edge-1 \
  --renew-service-account-token \
  --runtime ollama
```

`--renew-service-account-token` keeps the token file current: the agent
renews at half the token's lifetime, so a host that is off for longer than
that needs a new token, minted as above.

The model store and state directory default to
`$XDG_DATA_HOME/llmkube/models` and `$XDG_DATA_HOME/llmkube/metal-agent`
(`~/.local/share/llmkube/...` when `XDG_DATA_HOME` is unset); `--model-store`
and `--state-dir` override them. A model store under `/tmp` or `/var/tmp` is
refused.

The cluster reaches the agent on its ingress port (`--ingress-port`, 9443), so
open that port to the cluster in the host's firewall. Then wait for the
`MetalNode` to report `Reachable=True`:

```sh
kubectl get metalnode edge-1 -n edge
```

To remove the host, stop the agent and run `llmkube metal remove edge-1 -n
edge` where you have cluster access.

## Engines

As on a Mac, `spec.runtime` picks an InferenceService's engine and the agent's
`--runtime` is the fallback.

- **Ollama** (`runtime: ollama`): the agent talks to an Ollama-compatible
  daemon on `--ollama-port` (11434, Ollama's default) and does not start it.
  Run the daemon under its own supervisor, listening on `127.0.0.1`
  (`OLLAMA_HOST=127.0.0.1`): the agent warns when it also answers on the
  host's address, where it bypasses the ingress.
- **llama.cpp** (`runtime: llamacpp`, and the agent's default `--runtime
  llama-server`): the agent starts `llama-server`, found in `/opt/homebrew/bin`,
  `/usr/local/bin` or `/usr/bin`, or named with `--llama-server`. It is
  untested on Linux. Without llama-server the agent starts only when
  `--runtime` selects another engine, and refuses a service that selects
  llama.cpp with `RuntimeNotAvailable`.

## Memory

On Linux the agent reads `/proc/meminfo`, capped by the cgroup v2 limits from
its own cgroup up to the root: total memory is the smallest `memory.max` on
that path, and available memory the least room left under any of them. A
systemd `MemoryMax=` or a container `--memory` limit therefore sizes the
download cap, and the budget for engines the agent starts, which inherit its
cgroup.

Only cgroup v2 limits are read. On a host whose memory controller is on
cgroup v1, as on a hybrid v1/v2 host, the agent logs once at startup that
its memory figures are the host's.

Ollama is a daemon the agent only talks to, running outside its cgroup, so
`runtime: ollama` services are admitted against the host's `MemTotal` and
`MemAvailable`, whatever the agent's own limit, and the memory watchdog never
picks one to evict. `llmkube_metal_agent_memory_budget_bytes` reports that
host-wide budget.

For a fit decision that does not depend on the machine, as in tests, set
`spec.hardware.memoryBudget` on the Model. It may not exceed the host budget.
