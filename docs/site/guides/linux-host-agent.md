---
title: Linux Host Agent (experimental)
description: Run the metal-agent on a Linux arm64 or amd64 host, joined with llmkube metal join as a systemd user unit or by hand, serving models through a daemon engine such as Ollama, or hailo-ollama on a Hailo-10H.
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

This page covers any Linux host first. [Hailo-10H hosts](#hailo-10h-hosts)
adds what serving on a Hailo-10H takes, with the
[Raspberry Pi 5 and AI HAT+ 2](#raspberry-pi-5-with-an-ai-hat-2) as the
tested example.

Tracking issue: [#2481](https://github.com/defilantech/LLMKube/issues/2481).

## What has been tested

| | Status |
|---|---|
| linux/arm64 agent, `runtime: ollama` with stock Ollama on CPU | Tested in kind: an agent container next to stock Ollama, joined by hand, reached `Reachable=True` and served chat completions through the relay |
| `runtime: ollama` against `hailo-ollama` 5.1.1 on a Hailo-10H | End to end on a Raspberry Pi 5 with an AI HAT+ 2: `qwen2.5-instruct:1.5b` and `llama3.2:3b` served through the relay to a kind cluster, and back by itself after a power loss with the agent and hailo-ollama under systemd. To test without the hardware, see [Testing without a Hailo-10H](#testing-without-a-hailo-10h) |
| Linux arm64 host with a 16K-page kernel (Raspberry Pi 5) | The agent, Hailo's driver and runtime, and hailo-ollama run on Raspberry Pi OS's `6.18.50+rpt-rpi-2712` kernel, and the agent served a kind cluster on the same LAN |
| linux/amd64 | Builds. Untested on hardware |
| Release artifacts | `LLMKube-metal-agent_<version>_linux_arm64.tar.gz` and `_linux_amd64.tar.gz` on the GitHub release, static (`CGO_ENABLED=0`), listed in the release's `checksums.txt`. No Homebrew cask or distro package. The released binary is untested on hardware |
| llama.cpp (`llama-server`) on Linux | Untested. The agent's llama.cpp defaults (flash attention, mlock) are tuned for Apple Silicon |
| `llmkube metal join` | End to end on the arm64 host above: joined as a systemd user unit with lingering on, came back by itself after a power cut (`Reachable=True` 30-40 s after boot), and `metal leave` removed the unit, kubeconfig and token. Go tests cover join and leave with a fake `systemctl`, and CI installs, starts and removes the unit against a real `systemctl --user`. See [Join the host](#join-the-host) |
| `MetalNode` hardware | Reported: the chip (the device-tree model, or the CPU name from `/proc/cpuinfo`) and the memory, capped by the agent's cgroup v2 limit. GPU cores and the Metal version stay empty |
| `llmkube deploy` for a Linux host | Untested. Write the Model and InferenceService by hand |

## Get the agent

A GitHub release that ships Linux archives carries
`LLMKube-metal-agent_<version>_linux_arm64.tar.gz` and
`LLMKube-metal-agent_<version>_linux_amd64.tar.gz`, with their SHA-256 in
the release's `checksums.txt`. On the host, for its architecture (`arm64` or
`amd64`), with the version without its leading `v`:

```sh
VERSION=<version>
ARCH=arm64
BASE=https://github.com/defilantech/LLMKube/releases/download/v${VERSION}
curl -fLO "${BASE}/LLMKube-metal-agent_${VERSION}_linux_${ARCH}.tar.gz"
curl -fLO "${BASE}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar xzf "LLMKube-metal-agent_${VERSION}_linux_${ARCH}.tar.gz" llmkube-metal-agent
```

For a release without Linux archives, or an unreleased change, build the
agent from a checkout:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o llmkube-metal-agent ./cmd/metal-agent
```

Either way the binary is static and needs nothing else from the host. Put
it on the host's `PATH`, or next to `llmkube`.

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

## Join the host

Where you have cluster access, create the namespace and the host's identity,
as for a Mac. This creates its ServiceAccount (`llmkube-metal-<name>`), RBAC,
Lease and `MetalNode`, plus a join Secret:

```sh
kubectl create namespace edge
llmkube metal token create edge-1 -n edge
```

`token create` prints a `llmkube metal join <join-string>` line. Run it on the
host, as the user the agent runs as (not root), with `llmkube` and
`llmkube-metal-agent` on `PATH` or next to each other. Add the agent flags the
host's engine needs; for Ollama:

```sh
llmkube metal join <join-string> \
  --agent-arg runtime=ollama
```

As on a Mac, join trusts only the CA in the join string and prints its pin to
compare. It mints the agent's own token, writes it and a kubeconfig (both
0600) under `$XDG_DATA_HOME/llmkube/metal-agent` (`~/.local/share/...` when
`XDG_DATA_HOME` is unset), deletes the join Secret, and waits up to `--wait`
(3 minutes) for the `MetalNode` to report `Reachable=True`. That directory is
the agent's `--state-dir` default, created 0700 and owned by the joining
user, so the agent can write the renewed token next to the old one.

Instead of a LaunchAgent, join installs the agent as the
`llmkube-metal-agent.service` systemd user unit and starts it with
`systemctl --user daemon-reload` and `systemctl --user enable --now`. The
unit goes where the user's systemd instance loads it from: its own
`$XDG_CONFIG_HOME/systemd/user`, normally `~/.config/systemd/user`, which
join checks against the instance's `UnitPath`. The shell's `XDG_CONFIG_HOME`
does not count, since systemd does not read it. The unit sets
`XDG_DATA_HOME` to the data home join wrote the credentials under, so the
agent's default state directory and model store are there whatever the
instance's environment says. Its log is in the user journal:

```sh
journalctl --user -u llmkube-metal-agent.service -f
```

Before it spends the join string, join refuses to run as root, checks that
this user's systemd instance answers (`systemctl --user show-environment`),
and checks that the agent's ports are free, as on a Mac. A port held by the
agent join replaces passes: join tells it by the unit's main pid, from lsof
or, without lsof, from `/proc`. Any failure before
join deletes the join Secret, such as an API server name the host cannot
resolve, leaves the join string usable: fix the cause and run join again.

A user's systemd instance runs only while that user has a login session,
unless lingering is on. Without it the agent stops at logout and does not
start at boot. join prints a hint when lingering is off and never turns it
on itself. On a dedicated host, an admin runs:

```sh
sudo loginctl enable-linger <user>
```

Over SSH, join needs a session that sets `XDG_RUNTIME_DIR`, or lingering, to
reach the user's instance.

join refuses a missing `llama-server` only for the llama.cpp runtime (the
agent's default) or an explicit `--llama-server`, as on a Mac. For
`--agent-arg runtime=ollama` it goes on without one and leaves
`--llama-server` out of the unit. It looks in the agent's search paths
(`/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`) and on `PATH`;
`--llama-server` names one elsewhere.

join also refuses, before it spends the join string, a macOS-only engine the
agent would refuse at startup on Linux (see [Engines](#engines)):
`--agent-arg runtime=` `mlx-server`, `omlx`, `vllm-swift` or `tensorfold`, or
one of their binary flags, such as `--agent-arg mlx-server-bin=...`.

Running join again replaces the unit and keeps the agent arguments it does not
set itself. join refuses to replace a `llmkube-metal-agent.service` it did not
write unless `--force`, which keeps a copy under the state directory. It also
refuses while a drop-in (`systemctl --user cat llmkube-metal-agent.service`
lists them) sets `ExecStart`, since systemd would run that command line
instead of join's. `llmkube metal leave` stops and disables the unit, runs
`stop-engines` with the command line systemd ran (drop-ins included),
removes the unit, kubeconfig and token, and reloads systemd. It finds the
kubeconfig and token where the unit's `--kubeconfig` names them, so it does
not depend on the shell's `XDG_DATA_HOME`.

The unit sets `KillMode=process`: stopping or restarting it stops the agent
but not the engines it started, which the next agent adopts, as on a Mac.

The agent renews its token at half its lifetime (`--token-lifetime`, 7 days
by default), which needs the API server. A host that is off for longer comes
back with an expired token, and the agent logs `the agent's token has expired
and cannot be renewed`. Join it again: run `llmkube metal token create` where
you have cluster access, then `llmkube metal join` on the host.

## Join the host by hand

On a host without a systemd user instance, such as a container, or under a
supervisor of your own, create the host's identity as above, mint its token
yourself and write its kubeconfig.

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

Name the API server by a DNS name, an IP address, or a name in the host's
`/etc/hosts`, not an mDNS `.local` name. The agent is a static
(`CGO_ENABLED=0`) build, as are the `llmkube` CLI's Linux builds, and Go's
own resolver reads `/etc/hosts` and asks DNS without going through
nsswitch. A `.local` name that `getent hosts` resolves through
`mdns4_minimal` therefore fails with
`lookup <name>.local on <dns-server>:53: no such host`.

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

Copy both files to the host into a directory the agent's user owns, readable
only by that user (0600), and start the agent as that user (not root) under
the supervisor of your choice, such as a systemd unit or a container. Renewal
writes the new token to a temporary file in the token's directory and renames
it over the old one, so the agent's user needs write access to that
directory: in a container, mount the directory, not the token file alone.

```sh
llmkube-metal-agent \
  --kubeconfig /path/to/kubeconfig \
  --namespace edge \
  --metal-node edge-1 \
  --renew-service-account-token \
  --runtime ollama
```

`--renew-service-account-token` keeps the token file current: the agent
renews at half the token's lifetime, which needs the API server. A host
switched off or off the network for longer than the token's lifetime (7 days
with `--duration=168h`) always comes back with an expired token, and one away
for more than half of it may (less if the API server caps token lifetimes).
The agent then logs `the agent's token has expired and cannot be renewed`,
saying to join the host again and, for a host joined by hand, giving the
`kubectl create token` command to run; mint a new token as above and write it
over the token file.

The model store and state directory default to
`$XDG_DATA_HOME/llmkube/models` and `$XDG_DATA_HOME/llmkube/metal-agent`
(`~/.local/share/llmkube/...` when `XDG_DATA_HOME` is unset); `--model-store`
and `--state-dir` override them. A model store under `/tmp` or `/var/tmp` is
refused.

The cluster reaches the agent on its ingress port (`--ingress-port`, 9443), so
open that port to the cluster in the host's firewall. The `MetalNode` lists
the host's LAN addresses first, then tailnet ones, for the dial-back to try.
With the API server on the tailnet, the host's tailnet addresses come first,
then the LAN ones. With the API server on loopback (a kind cluster on the
same host), Docker Desktop's `192.168.65.254` comes last, after them. Then
wait for the `MetalNode` to report `Reachable=True`:

```sh
kubectl get metalnode edge-1 -n edge
```

To remove the host, stop the agent and run `llmkube metal remove edge-1 -n
edge` where you have cluster access.

## Engines

As on a Mac, `spec.runtime` picks an InferenceService's engine and the agent's
`--runtime` is the fallback. On Linux the agent serves only the two engines
below: it refuses to start when `--runtime` or a binary flag selects a
macOS-only engine (`mlx-server`, `omlx`, `vllm-swift`, `tensorfold`, or their
`--mlx-server-bin`, `--omlx-bin`, `--vllm-swift-bin`, `--tensorfold-bin`), and
a service whose `spec.runtime` names one is refused with
`RuntimeNotAvailable`.

- **Ollama** (`runtime: ollama`): the agent talks to an Ollama-compatible
  daemon on `--ollama-port` (11434, Ollama's default) and does not start it.
  Run the daemon under its own supervisor, listening on `127.0.0.1`
  (`OLLAMA_HOST=127.0.0.1`): the agent warns when it also answers on the
  host's address, where it bypasses the ingress. hailo-ollama on a Hailo-10H
  is such a daemon: use `runtime: ollama` with a `hailo://` source (see
  [Hailo-10H hosts](#hailo-10h-hosts)).
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
download cap, the `MetalNode` capacity, and the budget for engines the agent
starts, which inherit its cgroup.

Only cgroup v2 limits are read. On a host whose memory controller is on
cgroup v1, as on a hybrid v1/v2 host, the agent warns once at startup that
its memory figures are the host's: it then admits engines against host
memory, while the v1 limit can still OOM-kill them.

Ollama and hailo-ollama are daemons the agent only talks to, running outside
its cgroup, so `runtime: ollama` services are admitted against the host's
`MemTotal` and `MemAvailable`, whatever the agent's own limit, and the memory
watchdog never picks one to evict. `llmkube_metal_agent_memory_budget_bytes`
reports that host-wide budget.

For a fit decision that does not depend on the machine, as in tests, set
`spec.hardware.memoryBudget` on the Model. A budget larger than the one its
engine is admitted against (the cgroup's, or the host's for Ollama) is
clamped to it with a `MemoryBudgetClamped` Warning event, unless the agent
runs with `--allow-model-memory-budget-override`.

## Hailo-10H hosts

Hailo's [hailo-ollama](https://github.com/hailo-ai/hailo_model_zoo_genai)
serves an Ollama-compatible API on a Hailo-10H. It runs on HailoRT and the
Hailo PCIe driver; hailo-ollama 5.1.1 matches HailoRT 5.1.1. Install those for
your board first (on a Raspberry Pi 5, see
[below](#raspberry-pi-5-with-an-ai-hat-2)); `hailortcli fw-control identify`
should then report a `HAILO10H` device.

### Install hailo-ollama

hailo-ollama 5.1.1 is Hailo's `hailo_gen_ai_model_zoo` package, for arm64:

```sh
curl -fLO https://dev-public.hailo.ai/2025_12/Hailo10/hailo_gen_ai_model_zoo_5.1.1_arm64.deb
sudo dpkg -i hailo_gen_ai_model_zoo_5.1.1_arm64.deb
```

hailo-ollama 5.1.1 takes its listen address only from `hailo-ollama.json`,
and the packaged one (`/etc/xdg/hailo-ollama/hailo-ollama.json`) listens on
`0.0.0.0:8000`, an unauthenticated API on your network. Give the user that
runs it a copy listening on `127.0.0.1`, which hailo-ollama reads first:

```sh
mkdir -p ~/.config/hailo-ollama
sed 's/"0.0.0.0"/"127.0.0.1"/' \
  /etc/xdg/hailo-ollama/hailo-ollama.json \
  > ~/.config/hailo-ollama/hailo-ollama.json
```

The package's install script makes `/usr/share/hailo-ollama/models`, where
hailo-ollama 5.1.1 caches the HEFs it pulls, writable by every user.

### Start hailo-ollama at boot

The package does not start hailo-ollama at boot. Run it as a systemd user
service, and turn on lingering so that user's services start at boot without
a login session:

```sh
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/hailo-ollama.service <<'EOF'
[Unit]
Description=hailo-ollama on a Hailo-10H
After=network-online.target

[Service]
ExecStart=/usr/bin/hailo-ollama
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now hailo-ollama.service
sudo loginctl enable-linger "$USER"
```

Run the agent with `--runtime ollama --ollama-port 8000`, hailo-ollama's
port. With `llmkube metal join`, pass them as `--agent-arg runtime=ollama
--agent-arg ollama-port=8000`; a re-join keeps them. The unit join writes
pins `XDG_DATA_HOME`, so the agent looks for hailo-ollama's manifests where
hailo-ollama keeps them when it runs as the same user. If it runs as another
user or from another data directory, also pass
`--agent-arg hailo-manifests-dir=<its models/manifests directory>`.

### Serve a hailo:// model

Name the model with a `hailo://` source: a tag in the model zoo of the
installed hailo-ollama, which `GET /hailo/v1/list` lists. hailo-ollama 5.1.1
has `qwen2.5-instruct:1.5b`, `qwen2.5-coder:1.5b`, `qwen2:1.5b`, `llama3.2:3b`
and `deepseek_r1_distill_qwen:1.5b`. hailo-ollama maps the tag to a HEF
compiled for the Hailo-10H through the manifests it installs, and pulls it
from Hailo's model host (`dev-public.hailo.ai`) into its own cache, so neither
the controller nor the agent downloads anything. The Model needs
`accelerator: metal`, or the controller marks it Failed with `InvalidSource`.
The InferenceService needs `runtime: ollama`, or the agent refuses to serve
it with `RuntimeCannotServeSource`; one that leaves `spec.runtime` empty takes
the agent's `--runtime`. With the validating webhook enabled (multitenancy),
an InferenceService that names another runtime is rejected when it is
created. The tag must be `name:tag`, as `GET /hailo/v1/list` spells it:
hailo-ollama has no default tag, so a bare `name` is refused, and so is a
`/`, which could name another registry.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen25-15b
spec:
  source: hailo://qwen2.5-instruct:1.5b
  hardware:
    accelerator: metal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen-hailo
spec:
  modelRef: qwen25-15b
  runtime: ollama
  replicas: 1
```

Like any Model change, editing the source does not restart a running
service; change the InferenceService (or restart the agent) to pick it up.

Before it pulls, the agent checks that the daemon on `--ollama-port` is
hailo-ollama (it serves `GET /hailo/v1/list`) and that it lists the tag, so a
host running stock Ollama refuses the service instead of pulling a different
model under the same name. hailo-ollama answers the pull only once the HEF is
on disk; the agent waits up to two hours for it.

Memory admission sizes a `hailo://` model the way it sizes an `https://` one,
with a HEAD request and nothing downloaded. The agent reads the HEF's SHA-256
(`hef_h10h`) from hailo-ollama's manifest for the tag,
`<name>/<tag>/manifest.json` under the manifests directory, and HEADs
`<--hailo-library-url>/blob/sha256_<digest>`. By default the agent finds the
manifests directory the way hailo-ollama does, from its own user's
environment: the first `hailo-ollama` directory under `$XDG_DATA_HOME`
(`~/.local/share`), then `$XDG_DATA_DIRS` (`/usr/share:/usr/local/share`),
plus `models/manifests`. For the package that is
`/usr/share/hailo-ollama/models/manifests`. When hailo-ollama runs as another
user or with other XDG data directories, set `--hailo-manifests-dir` to its
manifests directory; otherwise admission cannot size the model, and the
error names the path it tried. The HEAD is guarded against private
addresses like any other download and verifies TLS against the system's CA
certificates. `dev-public.hailo.ai` is public, so it needs no
`--allowed-download-hosts` entry; that flag only lets a private, loopback or
link-local address through, which matters when `--hailo-library-url` points
at a mirror on your network or at the simulator.

### Known limits

Checked on a Raspberry Pi 5 with an AI HAT+ 2:

- Clients must send the hailo-ollama tag (`qwen2.5-instruct:1.5b`) as
  `model` in the request body. Nothing rewrites the service's model name.
- hailo-ollama serves only chat and generation. Through the relay it
  answers `GET /`, `POST /api/chat`, `POST /api/generate`, `GET /api/tags`,
  `POST /api/show`, `GET /api/version`, `GET /api/ps` and
  `POST /v1/chat/completions`. It answers 404 on `/api/embed`,
  `/api/embeddings`, `/health`, `/metrics`, `/v1/models`, `/v1/models/{id}`,
  `/v1/completions`, `/v1/embeddings` and `/v1/responses`, which the ingress
  forwards for `runtime: ollama`, so a 404 on those comes from hailo-ollama,
  not the routing. The agent calls its other two routes, `POST /api/pull` and
  `GET /hailo/v1/list`, on the host only.
- `created` in `/v1/chat/completions` is in nanoseconds.
- Requests need `Content-Type: application/json`.
- hailo-ollama keeps one model loaded and reloads on every switch, so run one
  `InferenceService` per Hailo-10H.
- hailo-ollama runs one generation at a time and does not stop one when its
  client disconnects, so a request that times out keeps the device busy until
  its answer is done, and the next request waits behind it. Set `max_tokens`
  on requests to bound that.
- The memory estimate is the HEF's size (2.2 GiB for `qwen2.5-instruct:1.5b`),
  but the budget it is checked against is the host's RAM, not the Hailo-10H's
  own memory. To test the device's limit, use the simulator's
  `HAILO_SIM_DEVICE_MEMORY`.

### Raspberry Pi 5 with an AI HAT+ 2

The AI HAT+ 2 is a Hailo-10H with 8 GB of its own memory on the Pi 5's PCIe
connector. Raspberry Pi ships HailoRT 5.1.1 and the driver for it in its apt
repository. Install them and reboot:

```sh
sudo apt install dkms hailo-h10-all
sudo reboot
```

Then install and start hailo-ollama as above, and join the Pi with
`llmkube metal join` and the two `--agent-arg` flags; the tested Pi runs the
agent and hailo-ollama as systemd user units with lingering on.

- Raspberry Pi OS runs the Pi 5 on a kernel with 16K pages
  (`6.18.50+rpt-rpi-2712`). The agent, `hailo-h10-all` and hailo-ollama run
  on it.
- The Pi 5 has no battery-backed clock and boots with the last saved time
  until network time corrects it. Without network time, the agent's
  heartbeats look stale and TLS certificate checks can fail.

## Testing without a Hailo-10H

`test/hailosim` builds the real hailo-ollama server against HailoRT's headers
and a fake `libhailort` that emulates a Hailo-10H. See its
[README](https://github.com/defilantech/LLMKube/tree/main/test/hailosim) for
the settings it takes.

```sh
make docker-build-hailo-sim
docker run --rm -p 8000:8000 localhost/llmkube-hailo-sim:dev
hack/hailo-probe.sh http://127.0.0.1:8000
```

`hack/hailo-probe.sh` checks every endpoint the agent's Ollama executor and
ingress use. Run it on a real host to compare against the simulator.

To serve a `hailo://` model from the simulator, start it with `--name
hailo-sim` and run an agent in its network namespace (`docker run --network
container:hailo-sim`) with `--runtime ollama --ollama-port 8000`, as on a
real host. The agent then sizes models from the simulator instead of Hailo's
library: copy the manifests out of the container (`docker cp
hailo-sim:/usr/share/hailo-ollama/models/manifests ./manifests`), mount them
into the agent's container, and pass `--hailo-manifests-dir` with their path,
`--hailo-library-url http://127.0.0.1:8001` (the simulator's
`HAILO_SIM_LIBRARY_LISTEN`) and `--allowed-download-hosts 127.0.0.1`, since
the size check refuses loopback addresses that the allowlist does not name.
