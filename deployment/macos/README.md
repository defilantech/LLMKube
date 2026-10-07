# LLMKube Metal Agent for macOS

This directory contains the macOS launchd configuration for the LLMKube Metal Agent, which enables Metal GPU acceleration for local Kubernetes LLM deployments.

## Prerequisites

1. **macOS with Apple Silicon** (M1/M2/M3/M4) or Intel Mac with Metal 2+ support
2. **Access to a Kubernetes cluster** — either a remote cluster (recommended) or local minikube
3. **llama.cpp** with Metal support:
   ```bash
   brew install llama.cpp
   ```
4. **LLMKube operator** installed in your cluster:
   ```bash
   kubectl apply -f https://github.com/defilantech/llmkube/releases/latest/download/install.yaml
   ```
5. **`--host-ip` flag** (required when using a remote cluster): the Metal Agent must be started with `--host-ip <your-mac-ip>` so that the agent's ingress is registered in Kubernetes at the Mac's reachable IP address instead of `localhost`

## Installation

### Option 1: Using Makefile (Recommended)

```bash
# Build and install Metal agent
make install-metal-agent
```

This will:
- Build the Metal agent binary
- Install to `/usr/local/bin/llmkube-metal-agent`
- Install launchd service
- Start the service automatically

### Option 2: Manual Installation

```bash
# Build the agent
make build-metal-agent

# Copy to /usr/local/bin
sudo cp bin/llmkube-metal-agent /usr/local/bin/

# Install launchd plist (launchd does not expand ~ or $HOME, so render the
# __HOME__ placeholder in the log paths; `make install-metal-agent` does this)
mkdir -p ~/Library/LaunchAgents
mkdir -p -m 0700 ~/Library/Logs/llmkube
sed 's|__HOME__|'"$HOME"'|g' deployment/macos/com.llmkube.metal-agent.plist \
  > ~/Library/LaunchAgents/com.llmkube.metal-agent.plist

# Load the service
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## Usage

Once installed, the Metal agent runs automatically in the background and watches for InferenceService resources in your Kubernetes cluster.

### Deploy a Model with Metal Acceleration

```bash
# Deploy from catalog
llmkube deploy llama-3.1-8b --accelerator metal

# Or deploy custom model
llmkube deploy my-model --accelerator metal \
  --source https://huggingface.co/.../model.gguf
```

### Check Agent Status

```bash
# Check if agent is running
launchctl list | grep llmkube

# View agent logs
tail -f ~/Library/Logs/llmkube/metal-agent.log

# Check running processes
ps aux | grep llmkube-metal-agent

# Health check (liveness)
curl http://localhost:9090/healthz

# Readiness check (at least one process healthy, or no processes yet)
curl http://localhost:9090/readyz
```

### Verify Metal Acceleration

```bash
# Check Metal support
system_profiler SPDisplaysDataType | grep Metal

# Monitor GPU usage while inference is running
sudo powermetrics --samplers gpu_power -i 1000
```

## Configuration

The launchd plist can be customized by editing `com.llmkube.metal-agent.plist`:

```xml
<key>ProgramArguments</key>
<array>
    <string>/usr/local/bin/llmkube-metal-agent</string>
    <string>--namespace</string>
    <string>default</string>              <!-- Kubernetes namespace to watch -->
    <!-- Optional: --model-store <path>; default ~/Library/Application Support/llmkube/models -->
    <string>--llama-server</string>
    <string>/usr/local/bin/llama-server</string>  <!-- Path to llama-server binary -->
    <string>--port</string>
    <string>9090</string>                 <!-- Agent metrics port -->
</array>
```

### Model store

Downloaded models and the per-engine logs live in the model store, by default
`~/Library/Application Support/llmkube/models`. The agent creates it (mode
0700) on first start. The store is judged on the directory it resolves to (the
target of a symlinked store), and the agent refuses to start unless:

- the store is not at or under `/tmp`, `/var/tmp` or their `/private` forms
  (macOS resolves `/tmp` to `/private/tmp`), even when the agent owns it:
  those directories are emptied at boot, so another local user could create
  the path first,
- the store is owned by the agent's user and is not writable by the group or
  other users,
- every directory above it, up to `/`, is owned by root or by the agent's
  user, and is not group- or other-writable unless it has the sticky bit
  (like `/Users/Shared`). Otherwise the owner of that directory, or anyone who
  can write it, could move the store aside and put their own in its place.

The error names the path, its owner uid and mode, and the fix (`chown`,
`chmod go-w`, or a different `--model-store`). The agent then uses the
resolved directory, so repointing a symlinked `--model-store` needs a restart.
`--model-store` must be absolute or start with `~/` (expanded against the
agent user's home).

A volume that does not store Unix permissions, such as an exFAT or FAT
external disk under `/Volumes`, reports every directory as mode 0777, so a
store there always fails the check. Put the store on an APFS or HFS+ volume.

To see the owner and mode of the store and every directory above it, set
`STORE` to your store path and run this in `sh`, `bash` or `zsh`:

```bash
STORE="$HOME/Library/Application Support/llmkube/models"; p=$(cd "$STORE" 2>/dev/null && pwd -P || echo "$STORE"); while :; do ls -ld "$p" 2>/dev/null || echo "not created yet: $p"; [ "$p" = / ] && break; p=$(dirname "$p"); done
```

Each line must show the agent's user or `root` as the owner, and no `w` in
the group or other position unless the mode ends in `t` (sticky). A store
that does not exist yet prints `not created yet`; the agent creates it 0700.

### `--allowed-model-roots` flag (local model paths)

By default, the agent only allows a Model's local source, an oMLX `pagedSSDCacheDir`, and the path values of path-valued `extraArgs` (for example `--chat-template-file`, `--lora`, `--mmproj`) to resolve into the model store (`--model-store`). A relative `extraArgs` path value resolves under the model store. Everything else is refused before the model is admitted or an engine starts.

To allow local model sources outside the model store, pass a comma-separated list of absolute directories:

```bash
llmkube-metal-agent --allowed-model-roots /Users/you/llmkube-models
```

Symlinks in a local source are followed, so a local source (or an `owner/repo` source resolved under the store) that is itself a symlink into the Hugging Face cache needs the cache's real directory added, not just the model store. Add the cache root itself (`/Users/you/.cache/huggingface/hub`), not a `models--org--name/snapshots/<rev>` subdirectory: the snapshot's files are symlinks into the cache's `blobs/` directory, and a GGUF's shards resolve there too, outside any narrower root you might otherwise pick:

```bash
llmkube-metal-agent --allowed-model-roots /Users/you/.cache/huggingface/hub
```

To serve a GGUF that is already on the Mac, set the Model's `spec.source` to its absolute path (or a `file://` URI) and add its directory to `--allowed-model-roots`. Do not place the file, or a symlink to it, in the cache slot of a downloaded source (`<model-store>/<model>/<file>` for an `https`, `hf` or `s3` source): the agent writes that slot itself and refuses to start from a symlink there, with reason `ModelSourceNotAllowed`.

To set this in the launchd plist, add these lines to the `ProgramArguments` array:

```xml
    <string>--allowed-model-roots</string>
    <string>/Users/you/llmkube-models,/Users/you/.cache/huggingface/hub</string>
```

`~` in a root expands to the agent user's home directory (for example `~/llmkube-models`); launchd runs the agent as a specific user, so this is the same home `--allowed-model-roots` sees whether you invoke the binary directly or through the plist.

A relative path, or a root the agent cannot resolve at startup, stops the agent from starting. A root that does not exist is logged and ignored until the agent restarts: roots are resolved once at startup, so creating the directory later does not retroactively allow it.

The root match is case-sensitive, so spell a root exactly as it exists on disk; on the default case-insensitive APFS volume, a path that differs from a configured root only in case is still treated as outside it and refused.

Every model source is checked, whatever its scheme: an absolute local path, a `file://` URL, and a scheme-less relative source such as an `owner/repo` id (resolved under the model store). Sources with a scheme other than `file://` (`https`, `hf`, `s3`, `pvc`, `oci`) are not path-checked; any source with a `..` path segment is refused, no matter its scheme.

Path-valued `extraArgs` are checked against these same roots; a refusal suggests adding the directory to `--allowed-model-roots`, or setting `--allow-unsafe-extra-args`. See "Security model" below for the full `extraArgs` policy.

A refusal shows up as a Warning event on the InferenceService:

```bash
kubectl describe inferenceservice <name>
```

```
Warning  ModelSourceNotAllowed  ...  model path /Users/you/other-models/model.gguf resolves to
/Users/you/other-models/model.gguf, outside the allowed model roots [/Users/you/llmkube-models];
add its directory to the agent's --allowed-model-roots to allow it
```

(macOS resolves `/tmp` to `/private/tmp`, so a root or a resolved path under `/tmp` prints with that `/private` prefix.)

The same message is on the status:

```bash
kubectl get isvc <name> -o jsonpath='{.status.schedulingMessage}'
```

Upgrade note: if you already point local model sources, or a `pagedSSDCacheDir`, outside the model store, add their root to `--allowed-model-roots` before upgrading, or the agent will start refusing them. This mirrors the controller's own `--allowed-host-path-roots` (Helm `modelSource.allowedHostPathRoots`), which is enforced separately, at the controller. A Model the controller already marked `Failed` (for example with reason `SourceNotAllowed`) is now also refused by the agent, instead of being served anyway.

### `--allowed-download-hosts` flag (remote model sources)

Every download the agent makes for a Model source (`http`, `https`, `hf`, `s3`, the HEAD probes used for memory sizing, and any redirect along the way) is refused if the host resolves to a loopback, link-local (including the cloud metadata address `169.254.169.254`), RFC 1918, CGNAT, unique-local or unspecified address. The guard checks the resolved IP, not the hostname, so it also catches a public name that redirects or DNS-rebinds to one of those ranges.

To allow a specific internal source anyway, for example a LAN MinIO, pass a comma-separated list of hostnames or CIDRs:

```bash
llmkube-metal-agent --allowed-download-hosts=minio.lan,10.20.0.0/16
```

To set this in the launchd plist:

```xml
    <string>--allowed-download-hosts</string>
    <string>minio.lan,10.20.0.0/16</string>
```

A refused download names the flag in its error and Event message. The controller enforces the same rule on its own downloads via `modelSource.allowedRemoteHosts` (Helm), from the same shared guard package; the two allowlists are configured separately and are not merged.

A few behavior notes:

- HTTP proxy environment variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and their lowercase forms) are honored. The target host is still checked against the allowlist even when the request goes through a proxy; the proxy's own address is trusted as configured.
- Redirects are capped at 5 hops, and each hop's target is checked again before it is dialed.
- For `s3` sources, only requests to the host in `AWS_ENDPOINT_URL` (same scheme, host and port) are SigV4-signed. A redirect to another host is followed unsigned, so the store's credentials never reach it; a presigned-URL redirect still works because it carries its own signature. An AWS path-style endpoint that answers 307 toward `<bucket>.s3.<region>.amazonaws.com` therefore now fails with 403 instead of being re-signed: set `AWS_ENDPOINT_URL` to the bucket's regional endpoint (for example `https://s3.<region>.amazonaws.com`). The controller's own s3 reads behave the same way.
- The dial timeout is 10 seconds; there is no overall download timeout, so a slow but healthy transfer of a large model is not cut short.
- A DNS64 address is judged by its embedded IPv4 address, so a public IPv4-only host stays reachable on a DNS64 network while the loopback and private ranges stay blocked.

### `spec.sha256` digest verification

When a `Model`'s `spec.sha256` is set, the agent verifies a downloaded source against it before the file is used. A mismatch deletes the file and refuses to start the InferenceService with reason `ModelDigestMismatch` (a Warning Event, plus `status.schedulingStatus`/`status.schedulingMessage`). A verified file gets a `<file>.sha256` stamp beside it, so a later restart does not re-hash a potentially huge model unless the stamp is missing or stale. The stamp is the same `digest size mtime` triple the cluster-side download init container writes, so a cache populated by either one is legible to the other; a bare-digest stamp from an older agent release is not trusted, and is hashed once and rewritten.

`spec.sha256` is enforced for sources the agent downloads for llama-server (the `llamacpp` runtime): `http`, `https`, `hf` and `s3`. A local-path or `file://` source is loaded in place and is never hashed. The other Metal runtimes (mlx-server, vllm-swift, oMLX, TensorFold and Ollama) do not download through the agent and ignore `spec.sha256` in 0.10.1.

The agent remembers a mismatch so it does not re-download the same bad source on every reconcile. Changing the Model's `source` or `sha256` clears that memo automatically, since it is keyed to the exact source and digest it was recorded against; the next reconcile then retries the download. If the spec is unchanged (for example, the same URL now serves a corrected file), recreate the `Model` or the `InferenceService` instead: deleting either one clears the memo, so the next reconcile downloads and verifies it again.

### Choosing the cluster (`--kubeconfig`, `--kube-context`)

The agent finds its cluster the same way controller-runtime tools do: the
`--kubeconfig` flag, then `$KUBECONFIG`, then `~/.kube/config`. By default it
uses that kubeconfig's **current context**, so if you switch contexts with
`kubectl config use-context` and the agent restarts, it connects to whatever
cluster you last selected.

Pin the agent to its cluster so your kubectl context never moves it:

- `--kubeconfig /path/to/file` points the agent at a dedicated kubeconfig.
- `--kube-context <name>` picks a context other than the current one.

Both can be set at install time:

```bash
make install-metal-agent METAL_KUBECONFIG=$HOME/.kube/llmkube-metal-agent.yaml
make install-metal-agent METAL_KUBE_CONTEXT=lab
```

or by adding them to `ProgramArguments` in the plist. The agent logs the
context and API server it connected to at startup:

```bash
grep "connected to Kubernetes" ~/Library/Logs/llmkube/metal-agent.log
```

### Dedicated kubeconfig with least-privilege RBAC

The agent doesn't need your admin credentials.
[`metal-agent-rbac.yaml`](metal-agent-rbac.yaml) creates a ServiceAccount
with only the permissions the agent uses (read InferenceServices and Models,
update InferenceService status, manage the Services and EndpointSlices that
publish each server, write events, read the relay token Secret and
`sourceSecretRef` secrets). Mint a kubeconfig from it:

```bash
NS=default                                  # the namespace the agent watches (--namespace)
KC=$HOME/.kube/llmkube-metal-agent.yaml
CA=$(mktemp)                                # private temp file for the cluster CA

kubectl apply -n "$NS" -f deployment/macos/metal-agent-rbac.yaml

SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
CA_FILE=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority}')
if [ -n "$CA_DATA" ]; then
  echo "$CA_DATA" | base64 -d > "$CA"
elif [ -n "$CA_FILE" ]; then
  cp "$CA_FILE" "$CA"
fi
test -s "$CA" || { echo "no cluster CA found in either certificate-authority-data or certificate-authority; set --certificate-authority on the set-cluster command below by hand" >&2; exit 1; }

TOKEN=$(kubectl create token llmkube-metal-agent -n "$NS" --duration=8760h)

kubectl --kubeconfig "$KC" config set-cluster llmkube \
  --server "$SERVER" --certificate-authority "$CA" --embed-certs
kubectl --kubeconfig "$KC" config set-credentials llmkube-metal-agent --token "$TOKEN"
kubectl --kubeconfig "$KC" config set-context llmkube \
  --cluster llmkube --user llmkube-metal-agent --namespace "$NS"
kubectl --kubeconfig "$KC" config use-context llmkube
chmod 600 "$KC"; rm "$CA"

# Check what the agent can do, then install with it
kubectl --kubeconfig "$KC" auth can-i --list -n "$NS"
make install-metal-agent METAL_KUBECONFIG="$KC"
```

The current context's cluster entry usually carries `certificate-authority-data`
(the CA embedded inline, base64-encoded); some kubeconfigs instead carry
`certificate-authority` as a file path, and a cluster with no CA configured at
all (for example one already using `insecure-skip-tls-verify`) has neither.
The recipe above tries the inline data first, falls back to copying the file,
and refuses to continue rather than minting a kubeconfig with an empty,
unusable CA.

`kubectl create token` issues a time-limited token, and the API server may
cap `--duration` below what you ask for. When it expires the agent logs
`Unauthorized`; rerun the `create token` and `set-credentials` steps. The
manifest is namespace-scoped: running the agent with `--namespace ""` (all
namespaces) needs the same rules in a ClusterRole and ClusterRoleBinding.

The agent's client also looks up API groups through the API server's
discovery endpoints (`/api`, `/apis`), which any authenticated identity can
normally read through the default `system:discovery` ClusterRoleBinding. If
your cluster's admins removed that binding, the ServiceAccount above needs it
granted explicitly, for example
`kubectl create clusterrolebinding llmkube-metal-agent-discovery --clusterrole=system:discovery --serviceaccount="$NS:llmkube-metal-agent"`.

### `--host-ip` flag (remote cluster)

When your Kubernetes cluster runs on a different machine (Linux server, cloud, etc.), the Metal Agent needs to register the Mac's reachable IP address so that the in-cluster relay can reach the agent's ingress (see "Network exposure" under "Security model"):

```bash
# Find your Mac's IP on the local network
ipconfig getifaddr en0

# Start the agent with --host-ip
llmkube-metal-agent --host-ip 192.168.1.50

# Or with a Tailscale / WireGuard address
llmkube-metal-agent --host-ip 100.64.0.10
```

Without `--host-ip`, the agent registers `localhost` as the endpoint — which only works when K8s is on the same machine (e.g. minikube).

The IP must be reachable from the cluster's nodes on `--ingress-port` (below).

To set this in the launchd plist, add these lines to the `ProgramArguments` array:

```xml
    <string>--host-ip</string>
    <string>192.168.1.50</string>         <!-- Your Mac's reachable IP -->
```

After editing, reload the service:
```bash
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

### Ingress and client-proxy flags

Engines listen on `127.0.0.1` only; the cluster reaches them through the
agent's authenticated TLS ingress. See "Network exposure" under "Security
model" for the full flow.

| Flag | Default | Purpose |
|------|---------|---------|
| `--ingress-port` | `9443` | TLS port of the ingress, on all interfaces. Open this port, not engine ports, in the macOS firewall. |
| `--state-dir` | `~/Library/Application Support/llmkube/metal-agent` | Agent state. The ingress key and certificate live in `<state-dir>/ingress/`. A leading `~` is expanded to the agent's home and a relative path is made absolute; the resolved path is logged at startup. |
| `--legacy-direct-endpoints` | `false` | DEPRECATED, removed in a future release. Engines on all interfaces without authentication, registered directly, no ingress. Only for a controller that predates relay support. |
| `--client-port` | `9999` | Listener on `127.0.0.1:<port>` that forwards `/v1/*` to the current engine, for clients on the Mac itself. `0` disables it. |

`--ingress-port` must differ from `--port` and `--client-port`.

The client-proxy listener only answers a request whose `Host` header names this loopback proxy (`127.0.0.1`, `localhost` or `[::1]` on `--client-port`); anything else gets 421, which blocks DNS rebinding. It also applies the same runtime path allowlist as the ingress. See "Network exposure" under "Security model" below for the full behavior.

### `--memory-fraction` flag (memory budget)

The Metal Agent estimates model memory requirements (weights + KV cache + overhead) before starting `llama-server`. If the model won't fit in the memory budget, the agent refuses to start it and sets the InferenceService status to `InsufficientMemory`.

By default, the budget is auto-detected based on total system RAM:

| Total RAM | Default Fraction | Budget |
|-----------|-----------------|--------|
| 16 GB | 67% | ~10.7 GB |
| 36 GB | 67% | ~24.1 GB |
| 48 GB | 75% | 36 GB |
| 64 GB | 75% | 48 GB |

To override:

```bash
# Use 50% of memory (conservative, leaves room for other apps)
llmkube-metal-agent --memory-fraction 0.5

# Use 90% of memory (dedicated inference machine)
llmkube-metal-agent --memory-fraction 0.9
```

To set this in the launchd plist:

```xml
    <string>--memory-fraction</string>
    <string>0.75</string>                 <!-- 75% of system memory -->
```

## Apple Silicon Power Metrics (for InferCost)

The Metal Agent can publish real-time CPU / GPU / ANE / Combined power gauges sourced from macOS `powermetrics`. These gauges are designed to be scraped by [InferCost](https://github.com/defilantech/infercost) for per-token cost attribution on Apple Silicon (where DCGM doesn't exist). The feature is **disabled by default** because `powermetrics` requires root.

### Enable

1. Install the NOPASSWD sudoers fragment so the agent can call `powermetrics` without a password. **The shipped fragment pins both the binary path and its argument vector** so the grant is `powermetrics --samplers cpu_power,gpu_power -i <numeric-interval>` only, not `powermetrics --output-file=/wherever`.

   The easy way (one command, sudo prompts you):

   ```bash
   make install-powermetrics-sudo
   ```

   This renders the placeholder for the current user, syntax-checks with `visudo -cf`, and atomically installs to `/etc/sudoers.d/llmkube-powermetrics` with `0440 root:wheel` ownership. The grant is read back to your terminal so you can see exactly what was just authorized.

   To **uninstall**:

   ```bash
   make uninstall-powermetrics-sudo
   ```

   The fully manual equivalent (if you want to inspect every step):

   ```bash
   TMP=$(mktemp)
   sed "s/__LLMKUBE_USER__/$(whoami)/" deployment/macos/sudoers.d/llmkube-powermetrics > "$TMP"
   sudo visudo -cf "$TMP"
   sudo install -m 0440 -o root -g wheel "$TMP" /etc/sudoers.d/llmkube-powermetrics
   rm "$TMP"
   ```

2. Add `--apple-power-enabled` to the agent's `ProgramArguments` and reload launchd:

   ```xml
       <string>--apple-power-enabled</string>
   ```

   Then:

   ```bash
   launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
   launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
   ```

3. Verify the gauges are populated:

   ```bash
   curl -s http://localhost:9090/metrics | grep apple_power
   ```

   With no inference running you'll see ~1-3 W combined; running a model under load drives it to 35-50 W on M-series.

### Gauges exposed

| Metric | Description |
|--------|-------------|
| `llmkube_metal_agent_apple_power_combined_watts` | CPU + GPU + ANE package power |
| `llmkube_metal_agent_apple_power_gpu_watts` | GPU subsystem power |
| `llmkube_metal_agent_apple_power_cpu_watts` | CPU subsystem power |
| `llmkube_metal_agent_apple_power_ane_watts` | Apple Neural Engine power (typically 0 for llama.cpp / MLX inference, which uses GPU compute) |

All four gauges read 0 unless `--apple-power-enabled` is set.

### Tuning

- `--apple-power-interval=1s` — sampling cadence; tighter values don't add fidelity because `powermetrics` rounds to whole milliseconds.
- `--powermetrics-bin=/usr/bin/powermetrics` — override only if you've installed `powermetrics` somewhere non-standard.

## Health Checks & Monitoring

The Metal Agent exposes an HTTP server on `127.0.0.1:9090` (configurable via `--port`) with health check and Prometheus metrics endpoints. The server binds to localhost only; to expose it for remote Prometheus scraping, use a reverse proxy or SSH tunnel.

### Endpoints

| Endpoint | Purpose | Success | Failure |
|----------|---------|---------|---------|
| `GET /healthz` | Liveness probe — agent process is alive | Always 200 | — |
| `GET /readyz` | Readiness probe — at least one process healthy (or no processes) | 200 | 503 (all unhealthy) |
| `GET /metrics` | Prometheus metrics | 200 | — |

### Prometheus Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `llmkube_metal_agent_managed_processes` | Gauge | Number of llama-server processes currently managed |
| `llmkube_metal_agent_process_healthy` | Gauge | Whether a process is healthy (1) or not (0). Labels: `name`, `namespace` |
| `llmkube_metal_agent_process_restarts_total` | Counter | Total process restarts triggered by health monitoring. Labels: `name`, `namespace` |
| `llmkube_metal_agent_health_check_duration_seconds` | Histogram | Duration of health check probes. Labels: `name`, `namespace` |
| `llmkube_metal_agent_memory_budget_bytes` | Gauge | Total memory budget for model serving |
| `llmkube_metal_agent_memory_estimated_bytes` | Gauge | Estimated memory per process. Labels: `name`, `namespace` |
| `llmkube_metal_agent_apple_power_combined_watts` | Gauge | Combined CPU + GPU + ANE package power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_gpu_watts` | Gauge | GPU subsystem power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_cpu_watts` | Gauge | CPU subsystem power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_ane_watts` | Gauge | Apple Neural Engine power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_ingress_requests_total` | Counter | Requests answered by the authenticated ingress. Labels: `code` (HTTP status) |
| `llmkube_metal_agent_client_proxy_requests_total` | Counter | Requests answered by the host-side client proxy. Labels: `outcome` (`bad_host`, `no_backend`, `path_forbidden`, or the response's status class) |

Standard Go runtime and process metrics (`go_*`, `process_*`) are also available.

### Continuous Health Monitoring

The agent polls each managed llama-server process every 30 seconds via its `/health` endpoint. On failure:

1. The process is marked unhealthy (`Healthy=false`, `process_healthy` gauge set to 0)
2. The agent re-fetches the InferenceService from Kubernetes
3. `ensureProcess()` is called to restart the llama-server
4. The `process_restarts_total` counter is incremented

When a previously unhealthy process recovers, it is marked healthy again automatically.

### Scraping with Prometheus

The health server binds to `127.0.0.1` by default. If Prometheus runs on the same Mac, scrape directly:

```yaml
scrape_configs:
  - job_name: 'llmkube-metal-agent'
    static_configs:
      - targets: ['localhost:9090']
        labels:
          instance: 'metal-agent'
```

For remote Prometheus, use an SSH tunnel: `ssh -L 9090:localhost:9090 <your-mac>`.

Quick verification:

```bash
# Check all endpoints
curl http://localhost:9090/healthz   # → "ok"
curl http://localhost:9090/readyz    # → "ready" or "not ready"
curl http://localhost:9090/metrics   # → Prometheus text format

# Check specific metric
curl -s http://localhost:9090/metrics | grep llmkube_metal_agent_managed_processes
```

## Security model

### Trust boundary

Anyone who can create or update `InferenceService` and `Model` objects in the
agent's watched namespace is untrusted for the Mac. They can pick any allowed
model and tune the engine, but they cannot choose where it reads or writes
files, what address or port it listens on, or what code it runs. The operator
who sets the agent's own flags, in the launchd plist, is trusted.

### What the agent checks before an engine starts

The agent runs these checks before starting any engine. A refusal starts
nothing: no engine process, no Service or EndpointSlice write. It shows up as
a Warning event, and the reason and message land in
`status.schedulingStatus` and `status.schedulingMessage` (the phase stays
`Creating`, or `Pending` while the Model is not ready):

```bash
kubectl describe inferenceservice <name>
```

- **Endpoint names** (`EndpointNameConflict`): the agent never overwrites a
  Service or EndpointSlice it does not already own. An InferenceService named
  after an unrelated object (for example `kubernetes`) is refused instead of
  taking over that object.
- **Model sources and roots** (`ModelSourceNotAllowed`): a `Model` the
  controller already marked `Failed` is refused, and any local model source
  (or oMLX `pagedSSDCacheDir`) must resolve inside the allowed roots. See
  "`--allowed-model-roots` flag (local model paths)" above.
- **extraArgs** (`ExtraArgsRejected`): every flag in `spec.extraArgs` is
  checked against a typed, per-engine allowlist. See below.
- **Remote sources** (download refused by the SSRF guard): a Model source
  that resolves to a private, loopback or link-local address is refused
  unless it is in `--allowed-download-hosts`. See "`--allowed-download-hosts`
  flag (remote model sources)" above.
- **Digest mismatch** (`ModelDigestMismatch`): when `spec.sha256` is set, a
  downloaded source that does not match it is deleted and refused. See
  "`spec.sha256` digest verification" above.
- **Cache slot** (`ModelSourceNotAllowed`): for a downloaded source, the
  file at `<model-store>/<model>/<file>` must be a regular file the agent
  wrote. A symlink there is refused, not followed.

### The extraArgs typed allowlist

Every flag an engine's own `--help` lists is pinned from a recorded build and
typed as one of: a flag that takes no value, a flag that takes a value the
engine never opens as a file (numbers, enum names, sampling parameters,
inline templates, repo ids), or a flag that takes a path.

Pinned engine versions:

- llama-server 0.5.0 (build 11146, commit `7fe450e19`)
- mlx-server, defilantech build of 2026-05-15
- tensorfold v0.3.4.1
- vllm-swift 0.4.2

A flag is refused when it would change where or how the engine listens or
is registered (`--host`, `--port`, and the other listener, registration and
multi-node flags listed under `--allow-unsafe-extra-args` below), what it
serves or logs to disk (`--path`, `--log-file`, `--slot-save-path`, and
similar), set auth keys or TLS material (`--api-key`, `--ssl-key-file`, and
similar), download a model (`-hf`, `--model-url`, and similar), override the
model or model id the agent chose (`--model`, `--alias`, `--served-model-name`,
TensorFold's `--name`), enable built-in tools or an MCP server (`--tools`,
`--mcp-servers-config`, and similar), or select a llama-server preset (the
`--fim-*` and `*-default` flags). For vllm-swift, flags that load Python code
(`--trust-remote-code`, anything ending in `-cls`, `-plugin` or `-config`
other than `--generation-config` and `--override-generation-config`),
dotted-key flags (`--flag.key value`), and inline JSON values are refused
too.

Path flags (`--chat-template-file`, `--model-draft`, `--mmproj`, `--lora`,
and their equivalents on other engines) must resolve inside the roots
configured by `--allowed-model-roots`.

A flag the pinned table does not know at all, for example one added in a
newer engine release, is refused as unknown rather than silently allowed.

### `--allow-unsafe-extra-args`

`--allow-unsafe-extra-args` relaxes:

- unknown flags (not in the pinned table),
- stray, non-flag arguments,
- the refused flags that do not touch the listener: file serving, logging,
  auth keys and TLS material; model downloads; model and model id
  overrides; built-in tools and MCP servers; llama-server presets,
- vllm-swift's code-loading, dotted-key and inline-JSON rules,
- the path checks (path-inside-roots, and a path value that does not
  decode).

It never relaxes:

- listener and registration flags: `--host`, `--port`, llama-server's
  `--reuse-port`, and vllm-swift's `--uds`, `--headless` and
  `--api-server-count`/`-asc`,
- multi-node and distributed flags: vllm-swift's `--data-parallel-address`,
  `--data-parallel-size`, `--data-parallel-size-local`,
  `--data-parallel-rpc-port`, `--data-parallel-external-lb`,
  `--data-parallel-hybrid-lb`, `--data-parallel-start-rank`,
  `--data-parallel-rank`, `--data-parallel-backend` (and their `-dp*` short
  forms), `--master-addr`, `--master-port`, `--nnodes`/`-n`,
  `--node-rank`/`-r`, `--kv-events-config`, `--kv-transfer-config`,
  `--ec-transfer-config`, `--weight-transfer-config` and
  `--distributed-executor-backend`; TensorFold's `--tp`, `--rank`, `--master`
  and `--master-port`,
- vllm-swift's `--config`, and any `-O` token other than `-O0` to `-O3`,
  `-O=N` or `-O N` with N from 0 to 3 (vLLM rewrites other `-O` tokens,
  which can carry other flags),
- other spellings an engine accepts for any of these: argparse
  abbreviations (tensorfold, vllm-swift), `_` spellings (llama-server,
  vllm-swift) and dotted spellings (vllm-swift),
- a bare `--` or `-` separator on tensorfold and vllm-swift,
- extraArgs on a runtime this policy does not otherwise recognize.

Turn this on only for a Mac whose InferenceService authors are fully trusted.

With it set, llama-server's own download flags (`-hf`/`--hf-repo`,
`-hff`/`--hf-file`, `-mu`/`--model-url`, `-dr`/`--docker-repo`,
`--hf-repo-draft`, `-mmu`/`--mmproj-url`, and similar) are allowed, and the
engine then fetches that file itself. Those downloads bypass the agent's SSRF
guard (`--allowed-download-hosts`) and the `spec.sha256` check, which apply
only to downloads the agent makes.

### Practical notes

- On vllm-swift, any `-config` flag other than `--generation-config` and
  `--override-generation-config`, and any inline JSON value other than one
  given to `--override-generation-config`, need
  `--allow-unsafe-extra-args`. `--config` itself is never allowed.
- On vllm-swift, pass a negative decimal value in the `--flag=value` form
  (`--flag=-0.5`, not `--flag -0.5`): as a separate token, vLLM's parser
  reads `-0.5` as a dotted-key flag, so the policy refuses it unless
  `--allow-unsafe-extra-args` is set.
- TensorFold's `--drafter` accepts `auto`, `none`, a Hugging Face repo id, or
  a path inside the allowed roots.
- llama-server's `--no-mmap` is not a flag in the pinned 0.5.0 build and is
  refused as unknown.
- vllm-swift's `--trust-request-chat-template`, `--enable-prompt-embeds` and
  `--enable-mm-embeds` are refused: the first lets any API client, not only
  the InferenceService writer, supply a chat template for the server to
  render, and the other two let any API client submit a serialized tensor
  the server deserializes with `torch.load` (the class of bug fixed as
  CVE-2025-62164). All three are relaxable with `--allow-unsafe-extra-args`.
- vllm-swift's `--tokenizer`, `--hf-config-path` and `--generation-config`,
  and the path half of a `--lora-modules name=path` entry, must resolve to a
  path that already exists inside the allowed roots; a Hugging Face
  `owner/name` repo id is not accepted there. Without this, a repo id would
  pass as a not-yet-existing path and vLLM would download it into the
  Hugging Face cache, outside every allowed root.
- A `--no-X=value` spelling (for example `--no-enable-prompt-embeds=false`)
  is refused exactly like `X` itself, on any runtime. Only a bare `--no-X`,
  with no value, is treated as the negation that turns `X` off.

### Network exposure

Engines never listen on the network. The agent starts llama-server,
mlx-server, TensorFold, vllm-swift and oMLX bound to `127.0.0.1`, and serves
one authenticated TLS ingress on all interfaces, on `--ingress-port` (default
9443). Traffic from the cluster reaches an engine only through that ingress.

Ollama is the exception because the operator starts it, not the agent. Make
sure `OLLAMA_HOST` is not set to `0.0.0.0` (unset it, or set it to
`127.0.0.1`) so the Ollama daemon binds loopback. At startup the agent checks
whether the Ollama port answers on the host IP and logs a warning if it does:
an Ollama reachable there bypasses the ingress. It runs the same check on the
oMLX port (`--omlx-port`): an oMLX daemon still running from an earlier agent
may be bound to all interfaces, so stop it and let the agent start it on
`127.0.0.1`.

#### How a request reaches the Mac

1. The agent registers a Service and EndpointSlice named `<isvc>-agent`
   (Service port 8443, named `https`, pointing at the Mac's host IP and the
   ingress port). It no longer writes an `<isvc>` EndpointSlice and deletes
   the one an earlier version left behind.
2. The controller creates a relay Deployment `<isvc>-relay` (the
   router-proxy image in `--relay` mode) and adopts the `<isvc>` Service in
   place: its selector moves to the relay pods, while its ClusterIP and DNS
   name stay the same. The Service port follows `spec.endpoint.port` (default
   8080) and its type follows `spec.endpoint.type` (`NodePort` is honored).
3. Clients keep calling `<isvc>.<ns>.svc:<port>`. The relay forwards each
   request to `<isvc>-agent`, and the ingress forwards it to the engine on
   loopback. SSE streaming passes through.

```
client pod ──▶ <isvc> Service ──▶ <isvc>-relay pod ──TLS──▶ agent ingress :9443 ──▶ engine on 127.0.0.1
               (ClusterIP, port    (in the cluster)          (on the Mac)
                unchanged)
```

#### What is authenticated

- **The agent, to the relay.** On first start the agent generates a private
  key and a self-signed certificate in `<state-dir>/ingress/` (default
  `~/Library/Application Support/llmkube/metal-agent/ingress/`; the key file
  is mode 0600, and the agent will not load a key that is readable by group
  or others). It publishes the certificate's SPKI SHA-256 pin as the annotation
  `llmkube.ai/agent-ingress-spki` on the `<isvc>-agent` EndpointSlice. The
  relay accepts only a certificate matching that pin.
- **The relay, to the agent.** The controller creates a Secret
  `llmkube-metal-relay` (key `token`, label
  `llmkube.ai/managed-by: llmkube-controller`) in each namespace with Metal
  InferenceServices. The relay mounts it and sends the token in the
  `X-LLMKube-Relay-Token` header, along with `X-LLMKube-Target: <ns>/<name>`
  naming the InferenceService. The agent reads the same Secret.

Only the controller may create that Secret. Otherwise someone who can create
Secrets in a namespace, but not read them, could create it first with a
token they know.

- **The control is the admission policy.** The Helm chart installs a
  ValidatingAdmissionPolicy (Kubernetes 1.30 or newer, value
  `metalRelay.secretCreatePolicy.enabled`, default `true`) that refuses to
  create `llmkube-metal-relay` for anyone but the controller's
  ServiceAccount. Updates and deletes are allowed, so rotation and
  revocation below still work.
- **Without the policy, the protection is RBAC.** On clusters older than
  1.30, or with the policy disabled, do not grant Secret create in
  namespaces with Metal models to anyone you would not trust with the relay
  token.
- **The label checks are a safety net, not a control.** Whoever creates the
  Secret can set the `llmkube.ai/managed-by` label too, so the checks below
  do not stop a deliberate attacker. They catch a Secret created before the
  policy existed, or created by hand by mistake:
  - The controller does not use an existing `llmkube-metal-relay` without
    the label. It records a `RelaySecretNotManaged` Warning Event and
    `status.schedulingStatus` on the InferenceService, and does not create
    or update the relay Deployment in that namespace.
  - The agent treats a Secret without the label as holding no token, so its
    ingress answers 401 for that namespace, and logs a warning naming the
    Secret.

To recover from `RelaySecretNotManaged`, delete the Secret; the controller
recreates it with a new token and the label.

The ingress answers:

| Status | When |
|--------|------|
| 400 | The `X-LLMKube-Target` header is missing or malformed |
| 401 | The token is missing, wrong, or belongs to another namespace, or the agent serves nothing in the target's namespace |
| 404 | The target is not running on this agent |
| 503 | The target's engine is not ready |
| 403 | The method and path are not on the runtime's allowlist |

The pin is rooted in Kubernetes write access: anyone who can write Services
or EndpointSlices in the namespace could already redirect the
InferenceService's traffic, so publishing the pin there adds no new trust.
Write access to a legacy core/v1 `Endpoints` object of the same name is
equivalent, since Kubernetes mirrors it into an EndpointSlice: treat both as
sensitive.

#### Path allowlist

An authenticated caller still reaches only these paths. Matching is exact
(method and path, case-sensitive); the query string is ignored, and a
non-canonical path (`..`, `.`, `//`, a trailing slash) is refused rather
than cleaned. Every runtime gets the common set:

| Method | Path |
|--------|------|
| GET | `/health` |
| GET | `/v1/models` |
| GET | `/v1/models/{id}` (one path segment) |
| POST | `/v1/chat/completions` |
| POST | `/v1/completions` |
| POST | `/v1/embeddings` |
| POST | `/v1/responses` |
| GET | `/metrics` |

Additions per runtime:

| Runtime | Additional paths |
|---------|------------------|
| llama-server (`llamacpp`) | POST `/completion`, `/completions`, `/tokenize`, `/detokenize`, `/apply-template`, `/embedding`, `/embeddings`, `/infill`, `/rerank`, `/reranking`, `/v1/rerank`, `/v1/messages`, `/v1/messages/count_tokens`; GET `/props`, `/models` |
| vllm-swift | POST `/tokenize`, `/detokenize`, `/v1/messages`, `/pooling`, `/score`, `/v1/score`, `/rerank`, `/v1/rerank`, `/v2/rerank`; GET `/version`, `/ping` |
| ollama | GET `/`, `/api/tags`, `/api/version`, `/api/ps`; POST `/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings`, `/api/show` |
| mlx-server, tensorfold, omlx | none (common set only) |

Everything else answers 403, including admin endpoints such as llama-server's
`/slots` and `POST /props`, vLLM's `/sleep` and LoRA loading, and Ollama's
pull, push, delete, create, copy and blob endpoints.

#### Client proxy

The client proxy on `127.0.0.1:<client-port>` (see "Ingress and client-proxy
flags" above) is for callers on the Mac itself, not the cluster, but it
applies the same rules:

- It answers 421 unless the request's `Host` header is `127.0.0.1`,
  `localhost` or `[::1]` on the proxy's own port. This blocks DNS rebinding:
  a page served from a public domain that resolves to `127.0.0.1` presents
  that domain as `Host`, not `localhost`, so it is rejected before it can
  reach a child process or even learn whether one is running.
- It applies the same runtime path allowlist the TLS ingress does (see
  "Path allowlist" above), so an admin path such as `/slots` still answers
  403.
- It rewrites the outbound `Host` header to the current engine's address and
  refuses protocol upgrades, the same as the ingress.

The client proxy has no token or certificate check of its own: any process
on the Mac that can reach `127.0.0.1:<client-port>` is trusted, but only for
the paths on the allowlist above.

The engines themselves still listen without authentication on their own
loopback ports (`127.0.0.1:<engine-port>`), with no `Host` check and no path
allowlist. The client proxy's checks do not cover a process that dials an
engine port directly, so any local process on the Mac can reach an engine's
admin endpoints (for example llama-server's `/slots`). Loopback is the trust
boundary: treat every local user and process on the Mac as able to use the
engines.

#### NetworkPolicy and metrics

Relay pods carry the label `inference.llmkube.dev/service: <isvc>`, the same
label pod-backed InferenceService pods carry. A NetworkPolicy that selects
InferenceService pods by that label also decides who can call a Metal
InferenceService. The chart's inference PodMonitor scrapes the engine's
`/metrics` through the relay. Engines without a `/metrics` endpoint (such as
TensorFold) scrape as an empty 200, so the target is not marked down.

#### Rotation

- **Token:** update the Secret `llmkube-metal-relay` in place with a new
  value, keeping its `llmkube.ai/managed-by: llmkube-controller` label:

  ```bash
  kubectl -n <ns> patch secret llmkube-metal-relay --type merge \
    -p "{\"stringData\":{\"token\":\"$(openssl rand -hex 32)\"}}"
  ```

  The agent accepts both the old and the new token for 10 minutes, and relays
  pick up the new file when the kubelet syncs the mounted Secret (typically
  within a minute or two), so traffic keeps flowing. Do not delete and
  recreate the Secret by hand to rotate: the admission policy refuses the
  create, and a Secret without the label is ignored, so relays in the
  namespace stop until it is deleted and the controller recreates it.
- **Revoking a token:** delete the Secret. The agent stops accepting the old
  token immediately (within its 5-second cache), so relays get 401 until the
  controller recreates the Secret with a new token and the relays reload it.
  Expect a short outage; use the in-place update above for routine rotation.
- **Ingress key:** stop the agent, delete `<state-dir>/ingress`, and start
  it. It generates a new key and publishes the new pin, and the controller
  rolls the relay to it.

#### Events

On a Metal InferenceService (`kubectl describe inferenceservice <name>`):

- From the controller: `RelayCreated`, `ServiceAdopted`, `RelayRemoved`
  (Normal); `RelayReconcileFailed`, `InvalidAgentIngressPin`,
  `RelaySecretNotManaged` (Warning).
- From the agent: `RelayNotAdopted`, `ExtraArgsRejected`,
  `EndpointNameConflict`, `ModelSourceNotAllowed`, `ServiceNameTooLong`,
  `ModelDigestMismatch` (Warning).

`ServiceNameTooLong` means the InferenceService name is too long for the
`<isvc>-agent` Service (at most 57 characters, since the name plus `-agent`
must fit a 63-character DNS label); rename it.

`RelayNotAdopted` means the controller had not adopted `<isvc>` for a relay
5 minutes after the agent registered `<isvc>-agent`, usually because the
controller predates relay support.

#### Upgrading

- Upgrade the Helm chart (controller) first, then the agents. The
  router-proxy image must match the controller version; the chart defaults
  do.
- In the macOS firewall, allow incoming connections on `--ingress-port`.
  Engine ports no longer need to be open.
- On its first relay registration for an InferenceService, the agent
  removes its own legacy `<isvc>` EndpointSlice. Until that InferenceService's
  relay pod is Ready (the first start pulls the router-proxy image), its
  Service has no endpoints, so expect a short window of failed requests per
  Metal Service during the upgrade.
- Anything that dialled `<mac-ip>:<engine-port>` directly stops working by
  design. Call the Service instead, or, on the Mac itself, the agent's client
  proxy on `127.0.0.1:<client-port>` (see "Ingress and client-proxy flags"
  above).

#### Upgrading to 0.10.1

0.10.1 hardens downloads, the model store, the client proxy and vllm-swift's
`extraArgs`.

##### Upgrade blockers

Each of these stops an agent, or an InferenceService, after the upgrade until
it is fixed. Check them before upgrading:

- **Re-render the launchd plist (required when upgrading from 0.10.0).** The
  0.10.0 plist passed `--model-store /tmp/llmkube-models` explicitly and sent
  the agent's output to `/tmp/llmkube-metal-agent.log`. Swapping the binary
  and running `launchctl kickstart -k` keeps that plist, and 0.10.1 refuses a
  model store under `/tmp`, so the agent will not start. Boot the job out and
  reinstall, which renders the new plist (default store
  `~/Library/Application Support/llmkube/models`, logs in
  `~/Library/Logs/llmkube/metal-agent.log`) and loads it:

  ```bash
  launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
  while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
  make install-metal-agent
  launchctl print gui/$(id -u)/com.llmkube.metal-agent | grep 'state = running'
  ```

  `make install-metal-agent` alone is not enough for a job that is already
  loaded: launchd keeps running the old definition until the job is booted
  out. Wait for the unload before reinstalling: `bootout` returns before
  launchd has finished, and a `bootstrap` that runs too soon can fail with
  `Bootstrap failed: 5: Input/output error` and leave the agent unloaded.
  The last command prints `state = running` once the agent is back. If you maintain your own plist, remove the `--model-store /tmp/...`
  pair (or point it at a directory the agent owns outside `/tmp`), move
  `StandardOutPath` and `StandardErrorPath` out of `/tmp`, and set
  `WorkingDirectory` to the agent user's home. Nothing moves by itself: until
  the plist changes, the agent refuses to start.
- **Model store ownership.** The store and every directory above it must pass
  the checks in "Model store" above: the store owned by the agent's user and
  not group- or other-writable, and every ancestor owned by root or the agent's
  user and not group- or other-writable unless sticky. A store on an exFAT or
  FAT volume always fails; use APFS or HFS+. Run the one-liner in "Model
  store" against your store path to see each directory's owner and mode.
- **A symlink in a downloaded source's cache slot.** Serving a hand-placed
  GGUF by putting it, or a symlink to it, at `<model-store>/<model>/<file>`
  with an `https`, `hf` or `s3` URL in `spec.source` no longer works: the
  InferenceService is refused with `ModelSourceNotAllowed`. Set the Model's
  `spec.source` to the file's absolute path (or a `file://` URI) and add its
  directory to `--allowed-model-roots`.
- **LAN download sources.** If any Model source is fetched from a LAN mirror
  (an internal MinIO, registry, or similar), add its host or CIDR to
  `--allowed-download-hosts`, or the agent will refuse to fetch it. See
  "`--allowed-download-hosts` flag (remote model sources)" above. Also
  expect macOS to revoke the new binary's Local Network access; see "LAN
  download fails with `no route to host` after an upgrade" under
  Troubleshooting.
- **vllm-swift `extraArgs`.** If any InferenceService's `extraArgs` sets
  vllm-swift's `--trust-request-chat-template`, `--enable-prompt-embeds` or
  `--enable-mm-embeds`, or points `--tokenizer`, `--hf-config-path`,
  `--generation-config` or a `--lora-modules` path at a Hugging Face repo id
  instead of a path already on disk, it will start failing
  `ExtraArgsRejected` after the upgrade. Fix the InferenceService or set
  `--allow-unsafe-extra-args` on that agent. See "Practical notes" above.

After the plist is re-rendered, expect a one-time re-download of every model
on first start: the new default store is a directory the agent has never
populated. The old `/tmp` store was cleared on every reboot anyway.

##### Behavior changes in 0.10.1

- The controller's GGUF metadata reads honor `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY` (and their lowercase forms), as the agent's downloads do. The
  target host is still checked against the allowlist when a proxy is used.
- A NAT64 address in `64:ff9b::/96` is judged by the IPv4 address embedded in
  it, so a public IPv4-only host stays reachable on a DNS64 network while
  loopback and private ranges stay blocked.
- Downloads follow at most 5 redirects (the agent allowed 10 before).
- The dial timeout for downloads is 10 seconds (it was 30).
- Downloads use HTTP/1.1 only.
- `spec.sha256` is now enforced for sources the agent downloads for
  llama-server; see "`spec.sha256` digest verification" above.

#### `--legacy-direct-endpoints` (deprecated)

`--legacy-direct-endpoints` restores the old behavior: engines bind all
interfaces without authentication, the agent registers `<isvc>` directly,
and no ingress runs. At startup in this mode the agent deletes its own
`<isvc>-agent` Service and EndpointSlice left from relay mode. It exists only
for a controller that predates relay support and will be removed in a future
release. With it, keep the Mac on a
trusted network, behind its own firewall, or reachable only over Tailscale.

## Troubleshooting

### Agent won't start

```bash
# Check logs
cat ~/Library/Logs/llmkube/metal-agent.log

# Verify llama-server is installed
which llama-server

# Verify Metal support
llmkube-metal-agent --version
```

### Metal not detected

```bash
# Verify GPU info
system_profiler SPDisplaysDataType

# Check for Metal support
system_profiler SPDisplaysDataType | grep "Metal"
```

### Model rejected with InsufficientMemory

The Metal Agent performs a pre-flight memory check before starting each model. If the estimated memory exceeds the budget, the InferenceService status will show `InsufficientMemory`:

```bash
# Check the scheduling status
kubectl get inferenceservices -o wide

# View the detailed message
kubectl get isvc <name> -o jsonpath='{.status.schedulingMessage}'
```

To resolve:
- **Use a smaller quantization** (e.g. Q4_K_M instead of Q8_0) to reduce model weight size
- **Reduce context size** in the InferenceService spec to lower KV cache requirements
- **Increase the memory fraction** with `--memory-fraction 0.9` if this is a dedicated inference machine
- **Close other applications** to free unified memory

### Model download blocked by the SSRF guard

The Model's source resolves to a private, loopback or link-local address, which the agent refuses to fetch from by default:

```bash
kubectl describe inferenceservice <name>
# Warning  ...  connection to <host> (<ip>) blocked by SSRF guard (GHSA-jw3m-8q7m-f35r); allowlist via --allowed-download-hosts
```

If the source is a LAN mirror you trust (an internal MinIO or registry), add its host or CIDR to `--allowed-download-hosts` and restart the agent. See "`--allowed-download-hosts` flag (remote model sources)" above.

### LAN download fails with `no route to host` after an upgrade

The guard allowed the host, but macOS blocked the connection. macOS Local Network Privacy ties its permission to the agent binary, so a new binary loses access to the local network until it is granted again, and the refusal surfaces as `connect: no route to host` rather than a permission error. Downloads from the internet and the cluster's traffic to the agent's ingress are not affected. Grant the agent access in System Settings, Privacy & Security, Local Network, or reach the mirror over a route macOS does not treat as local (for example a VPN or tailnet address).

### `ModelDigestMismatch`

The downloaded source's SHA256 does not match the Model's `spec.sha256`. The bad file was already deleted. Fixing `spec.sha256` or `spec.source` retries on the next reconcile; if the spec was already right and the source was fixed upstream, recreate the Model (or the InferenceService) to retry. See "`spec.sha256` digest verification" above.

### Can't connect to Kubernetes

```bash
# Verify kubectl can reach your cluster
kubectl get nodes

# Check which context is active
kubectl config current-context

# Check kubeconfig path
echo $KUBECONFIG

# See which cluster and context the agent actually connected to
grep "connecting to Kubernetes\|connected to Kubernetes" ~/Library/Logs/llmkube/metal-agent.log

# If using minikube locally
minikube status
```

### Remote cluster: pods can't reach the model

```bash
# Verify --host-ip was set correctly
# The IP must be reachable from the K8s nodes
ping <your-mac-ip>   # run from a K8s node

# Check that the agent registered its ingress with the right IP and the
# ingress port, and published the llmkube.ai/agent-ingress-spki annotation
kubectl get endpointslice <isvc>-agent -o yaml

# Check the relay pod and the adopted Service
kubectl get deploy,pods -l inference.llmkube.dev/metal-relay=<isvc>
kubectl logs deploy/<isvc>-relay
kubectl describe inferenceservice <isvc>   # look for RelayNotAdopted, InvalidAgentIngressPin, RelayReconcileFailed, RelaySecretNotManaged

# Verify the firewall isn't blocking the ingress port (--ingress-port, default 9443)
# macOS may prompt to allow incoming connections on first run

# If using Tailscale / WireGuard, verify the tunnel is up
tailscale status   # or wg show
```

## Uninstallation

```bash
# Using Makefile
make uninstall-metal-agent

# Or manually
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
sudo rm /usr/local/bin/llmkube-metal-agent
rm ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## How It Works

1. **Metal Agent** runs as a native macOS process (not in Kubernetes)
2. **Watches** for InferenceService resources in Kubernetes
3. **Downloads** models from HuggingFace when needed
4. **Validates** that the model fits in the system's memory budget
5. **Spawns** llama-server processes with Metal acceleration
6. **Registers** its authenticated TLS ingress back to Kubernetes as `<isvc>-agent` (engines themselves listen on `127.0.0.1` only)
7. **Monitors** process health every 30s and auto-restarts on failure
8. **Exposes** health checks and Prometheus metrics on port 9090
9. **Pods** call the `<isvc>` Service, which the controller points at an in-cluster relay (`<isvc>-relay`) that forwards to the ingress

### Remote cluster (Recommended)

K8s runs on a Linux server or cloud; the Mac dedicates all resources to inference:

```
┌──────────────────────────────┐        ┌──────────────────────────────┐
│ Linux Server / Cloud         │        │ macOS (Your Mac)             │
│                              │        │                              │
│  ┌────────────────────────┐  │  LAN/  │  ┌────────────────────────┐  │
│  │ Kubernetes             │  │  VPN   │  │ Metal Agent            │  │
│  │  LLMKube Operator      │  │        │  │  --host-ip <mac-ip>   │  │
│  │  InferenceService CRD  │◄─┼────────┼──│  Watches K8s API      │  │
│  │  <isvc> Service        │  │        │  │  Spawns llama-server  │  │
│  │   → <isvc>-relay pod   │──┼─TLS────┼─►│  Ingress :9443 (TLS)  │  │
│  └────────────────────────┘  │ pinned │  └────────────────────────┘  │
│                              │ +token │               ↓ 127.0.0.1    │
│                              │        │  ┌────────────────────────┐  │
│                              │        │  │ llama-server (Metal)   │  │
│                              │        │  │  Direct GPU access ✅  │  │
│                              │        │  │  All unified memory    │  │
│                              │        │  └────────────────────────┘  │
└──────────────────────────────┘        └──────────────────────────────┘
```

### Co-located (minikube on same Mac)

Everything on one machine — simpler but minikube consumes resources:

```
┌─────────────────────────────────────────────────┐
│              macOS (Your Mac)                    │
│                                                  │
│  ┌──────────────────────────────────────────┐   │
│  │   Minikube (Kubernetes in VM)            │   │
│  │   - Creates InferenceService CRD         │   │
│  │   - Service → relay → agent ingress      │   │
│  └──────────────────────────────────────────┘   │
│                     ↓                            │
│  ┌──────────────────────────────────────────┐   │
│  │   Metal Agent (Native Process)           │   │
│  │   - Watches K8s for InferenceService     │   │
│  │   - Spawns llama-server with Metal       │   │
│  └──────────────────────────────────────────┘   │
│                     ↓                            │
│  ┌──────────────────────────────────────────┐   │
│  │   llama-server (Metal Accelerated)       │   │
│  │   - Listens on 127.0.0.1 only            │   │
│  │   - Direct Metal GPU access ✅           │   │
│  └──────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
```

## Choosing a Runtime per InferenceService

The agent picks the runtime for each InferenceService in this order:

1. `spec.runtime` on the InferenceService, if set. The metal-agent runtimes are
   `llamacpp`, `mlx-server`, `omlx`, `vllm-swift`, `ollama`, and `tensorfold`.
2. The agent's `--runtime` flag, when `spec.runtime` is unset.
3. `llama-server` (llama.cpp), when neither is set.

One agent can serve several runtimes at once, as long as each binary is
configured (`--mlx-server-bin`, `--omlx-bin`, `--vllm-swift-bin`,
`--tensorfold-bin`, or `--ollama-port`) or selected with `--runtime`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen-mlx
spec:
  modelRef: qwen3-4b-mlx   # a Model with hardware.accelerator: metal
  runtime: mlx-server
```

`mlx-server`, `omlx`, `vllm-swift`, `ollama`, and `tensorfold` only work on the metal-agent.
The operator rejects them on an InferenceService whose Model does not set
`hardware.accelerator: metal`.

InferenceServices created with LLMKube 0.9.30 or earlier were stored with
`runtime: llamacpp`, because the CRD used to set that default. They keep
serving llama.cpp and ignore `--runtime`. To hand one of them back to the
agent's flag, remove the field:

```bash
kubectl patch inferenceservice <name> --type=json -p '[{"op":"remove","path":"/spec/runtime"}]'
```

### TensorFold

[TensorFold](https://github.com/ashhart/TensorFold) (MIT) serves an
OpenAI-compatible endpoint on MLX with exact speculative decoding: a draft
model proposes tokens and the target model verifies them, so the output is the
same as decoding without drafts, only faster.

The operator installs and pins the engine; LLMKube only launches it. Install a
pinned release with [uv](https://docs.astral.sh/uv/):

```bash
uv tool install --python 3.12 --with "mlx==0.31.2" \
  "git+https://github.com/ashhart/TensorFold.git@v0.3.4.1"
```

Pin MLX to 0.31.2. On M5 hardware, MLX 0.32.2 fails the load-time exactness
self-check for Nemotron, and TensorFold then keeps serving without drafts, so
the only symptom is lost speed. Confirm the version inside the tool's
environment after installing:

```bash
~/.local/share/uv/tools/tensorfold/bin/python -c "import mlx.core as mx; print(mx.__version__)"
```

Drafts come from the Hugging Face cache. `--drafter auto` (TensorFold's
default) uses the model family's draft model only if it is already there, so
pull it once as the user the agent runs as:

```bash
tensorfold pull z-lab/Qwen3.8-27B-DFlash2
```

Start the agent with the binary. Without `--tensorfold-bin`, the agent looks in
`~/.local/bin/tensorfold` (where `uv tool install` puts it),
`/opt/homebrew/bin/tensorfold`, and `/usr/local/bin/tensorfold` when
`--runtime tensorfold` is set:

```bash
llmkube-metal-agent --tensorfold-bin ~/.local/bin/tensorfold
```

How the agent runs it:

- The Model must have `format: mlx` and `hardware.accelerator: metal`. The
  agent does not download MLX directories; stage the directory first. An
  absolute `spec.source` is used as-is (the operator must allow its root with
  `--allowed-host-path-roots`). A Hugging Face style `owner/repo` source is
  read from `<model-store>/owner/repo`.
- Each InferenceService gets its own `tensorfold serve` process on an
  ephemeral port, bound to `127.0.0.1`, with `--name` set to `spec.modelRef` (the
  model ID clients send) and `--no-update-check`.
- `spec.contextSize` becomes `--context`. Set it: when it is unset the agent
  uses 2048, as for the other metal runtimes, rather than the model's full
  window.
- TensorFold's own flags (`--no-thinking`, `--drafter`, `--max-tokens`) go in
  `spec.extraArgs`. A flag set there replaces the one the agent would add.
  `--alias` and `--name` are refused unless `--allow-unsafe-extra-args` is
  set: the agent sets the model id itself. See "Security model" above for the
  full `extraArgs` policy.
- Output goes to `<model-store>/tensorfold-<namespace>-<name>.log`. When
  TensorFold refuses a checkpoint and exits, the InferenceService error carries
  the exit status and the last lines of that log. `--tensorfold-startup-timeout`
  (default 10m) bounds only a child that stays up without answering `/health`;
  a first run compiles kernels and runs the exactness check, so it is slower
  than later ones.

See `config/samples/inferenceservice_qwen38_27b_tensorfold.yaml` for a full
Model and InferenceService.

## oMLX Runtime (MLX Backend)

The Metal Agent supports an alternative runtime using [oMLX](https://github.com/jundot/omlx), an MLX-based inference server for Apple Silicon. oMLX provides roughly 40% faster generation compared to llama-server Metal on the same hardware.

### Prerequisites

Install oMLX via Homebrew:

```bash
brew tap jundot/omlx https://github.com/jundot/omlx
brew install omlx
```

Download an MLX-format model (models from the `mlx-community` HuggingFace org):

```bash
pip install huggingface-hub
huggingface-cli download mlx-community/Llama-3.2-3B-Instruct-4bit \
  --local-dir ~/.omlx/models/Llama-3.2-3B-Instruct-4bit
```

### Usage

Start the Metal Agent with the oMLX runtime:

```bash
llmkube-metal-agent --runtime omlx --model-store ~/.omlx/models
```

Deploy an MLX model:

```bash
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-3b-mlx
spec:
  source: /path/to/models/Llama-3.2-3B-Instruct-4bit
  format: mlx
  hardware:
    accelerator: metal
    gpu:
      enabled: true
      count: 1
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-3b-mlx
spec:
  modelRef: llama-3b-mlx
  replicas: 1
EOF
```

The agent will start the oMLX daemon, load the model, and register the endpoint.

### Model identifier

oMLX serves each model under the basename of its model-store directory
(the leaf directory in a two-level `org/model` layout), and `/v1/models`
lists those names. A client that reaches the `<isvc>` Service directly or
the agent's `--client-port` proxy must send that basename as the OpenAI
`model` field, not the Model or InferenceService name. With the example
above, the id is `Llama-3.2-3B-Instruct-4bit`, not `llama-3b-mlx`.

A ModelRouter backend that points at an oMLX InferenceService whose
`spec.runtime` is `omlx` translates the published model name to the
basename, so clients routed through the ModelRouter can use the
InferenceService name. Set `spec.runtime: omlx` explicitly; the
controller cannot infer it from the agent's `--runtime` default.

To serve a name other than the directory basename, set a per-model
`model_alias` in the oMLX admin panel (persisted to
`~/.omlx/model_settings.json`). oMLX then accepts both the alias and the
directory name.

### Differences from llama-server

| | llama-server | oMLX |
|---|---|---|
| Model format | GGUF | MLX (safetensors) |
| Process model | One per model | One daemon, all models |
| Memory management | Pre-flight estimation | LRU eviction |
| Metrics | Prometheus native | JSON only |

### oMLX Flags

```bash
llmkube-metal-agent \
  --runtime omlx \
  --model-store ~/.omlx/models \
  --omlx-port 8000 \         # oMLX server port (default: 8000)
  --omlx-bin /path/to/omlx   # Auto-detected from Homebrew if not set
```

## Ollama Runtime

The Metal Agent also supports [Ollama](https://ollama.com) as a runtime backend. Since Ollama 0.19 uses MLX natively on Apple Silicon, this gives you fast inference with the tool most Mac users already have installed.

### Prerequisites

Install Ollama if you haven't already:

```bash
brew install ollama
```

### Usage

Start Ollama (if not already running as a menu bar app):

```bash
ollama serve
```

Leave `OLLAMA_HOST` unset (or set it to `127.0.0.1`) so Ollama binds loopback;
the agent serves it to the cluster through its ingress. See "Network
exposure" under "Security model".

Start the Metal Agent with the Ollama runtime:

```bash
llmkube-metal-agent --runtime ollama
```

Deploy a model. The agent will pull the model through Ollama automatically:

```bash
llmkube deploy llama-3.2-3b --gpu --accelerator metal
```

The agent maps LLMKube catalog names to Ollama model tags (e.g., `llama-3.2-3b` becomes `llama3.2:3b`). If the model isn't already downloaded, Ollama pulls it from the Ollama registry.

### Differences from llama-server and oMLX

| | llama-server | oMLX | Ollama |
|---|---|---|---|
| Model format | GGUF | MLX | GGUF (via Ollama registry) |
| Model download | Manual / init container | Manual | Automatic (`/api/pull`) |
| Install base | llama.cpp users | Small | Most Mac users |
| CRD changes needed | None | MLX format | None |

### Ollama Flags

```bash
llmkube-metal-agent \
  --runtime ollama \
  --ollama-port 11434    # Ollama server port (default: 11434)
```

## Performance

Expected performance on M4 Max (32 GPU cores):
- **Llama 3.2 3B**: 80-120 tok/s (llama-server), ~115 tok/s (oMLX/Ollama MLX)
- **Llama 3.1 8B**: 40-60 tok/s (llama-server)
- **Mistral 7B**: 45-65 tok/s (llama-server)

oMLX and Ollama (0.19+) both use Apple's MLX framework for Apple Silicon inference.

## Security

See "Security model" above for the trust boundary, the checks the agent runs
before starting an engine, the `extraArgs` policy, and network exposure
(engines listen on `127.0.0.1`; the cluster reaches them through the agent's
authenticated ingress and an in-cluster relay).

## Support

- GitHub Issues: https://github.com/defilantech/llmkube/issues
- Documentation: https://github.com/defilantech/llmkube#metal-support
