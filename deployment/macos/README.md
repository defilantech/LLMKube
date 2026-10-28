# LLMKube Metal Agent for macOS

This directory contains the macOS launchd configuration for the LLMKube Metal Agent, which enables Metal GPU acceleration for local Kubernetes LLM deployments.

## Prerequisites

1. **macOS with Apple Silicon** (M1 to M5). An Intel Mac with Metal 2+ can run the agent, but the bundled plist points `--llama-server` at the Apple Silicon Homebrew path (`/opt/homebrew/bin/llama-server`); change it to `/usr/local/bin/llama-server` there.
2. **Access to a Kubernetes cluster** — either a remote cluster (recommended) or local minikube
3. **llama.cpp** with Metal support:
   ```bash
   brew install llama.cpp
   ```
4. **LLMKube operator** installed in your cluster with Helm:
   ```bash
   helm repo add llmkube https://defilantech.github.io/LLMKube
   helm install llmkube llmkube/llmkube \
     -n llmkube-system --create-namespace
   ```
5. **To build the agent from source:** the Xcode Command Line Tools (`xcode-select --install`) and Go 1.27.3 or newer, the version `go.mod` pins.
6. **`--host-ip` flag** (recommended with a remote cluster): without it the agent auto-detects an address (see "`--host-ip` flag (remote cluster)" below), which may not be one your cluster can reach. Start the agent with `--host-ip <your-mac-ip>` so that its ingress is registered in Kubernetes at an address you chose.

## Installation

### Option 1: Homebrew (Recommended)

```bash
brew install defilantech/tap/llmkube defilantech/tap/llmkube-metal-agent
```

This installs the release binary as `$(brew --prefix)/bin/llmkube-metal-agent`
(signed as described under Option 3), `llama.cpp`, and the `llmkube` CLI.
Name both: the `llmkube-metal-agent` cask does not depend on the `llmkube`
formula, because Homebrew trusts only the third-party tap items named on the
command line (see `brew trust`) and refuses to load a dependency from a tap
you have not trusted. The cask runs on macOS only; the CLI formula also
installs on Linux.

The cask does not start the agent. Joining the Mac to a cluster writes the
LaunchAgent and starts it:

```bash
llmkube metal join <join-string>   # your cluster admin: llmkube metal token create <mac-name>
```

To run the agent without joining, for example with a kubeconfig you already
have, run the Homebrew binary under launchd by hand: the plist below is the
Option 3 one, pointed at the Homebrew binary. Before
the last command, add `--kubeconfig <file>` to `ProgramArguments` in the
rendered plist (see "Choosing the cluster" below).

```bash
v=$(brew list --cask --versions llmkube-metal-agent | awk '{print $2}')
mkdir -p ~/Library/LaunchAgents
mkdir -p -m 0700 ~/Library/Logs/llmkube
sed -e 's|__HOME__|'"$HOME"'|g' \
    -e 's|/usr/local/bin/llmkube-metal-agent|'"$(brew --prefix)"'/bin/llmkube-metal-agent|' \
    "$(brew --caskroom)/llmkube-metal-agent/$v/deployment/macos/com.llmkube.metal-agent.plist" \
  > ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

To upgrade, upgrade the cask and restart the agent. A LaunchAgent that names
`$(brew --prefix)/bin/llmkube-metal-agent`, as the one above does, survives
the upgrade because Homebrew relinks that path to the new version, but the
running agent keeps the old binary until it restarts:

```bash
brew upgrade --cask llmkube-metal-agent
launchctl kickstart -k gui/$(id -u)/com.llmkube.metal-agent
```

If the agent does not come back after an upgrade that changed how the binary
is signed, reload it as in "Applying a plist change" below.

`brew uninstall --cask llmkube-metal-agent` removes the binary only and
leaves the LaunchAgent loaded (Homebrew runs a cask's uninstall steps on every
upgrade too, so the cask leaves the LaunchAgent to `--zap`); boot the job out
yourself if you are not reinstalling. `brew uninstall --zap --cask
llmkube-metal-agent` also removes the LaunchAgent, the agent's logs
(`~/Library/Logs/llmkube`) and its state
(`~/Library/Application Support/llmkube/metal-agent`). Downloaded models stay
in the model store either way.

### Option 2: Build from source with the Makefile

```bash
# Build and install Metal agent
make install-metal-agent
```

This will:
- Build the Metal agent binary
- Install to `/usr/local/bin/llmkube-metal-agent`
- Install launchd service
- Start the service automatically

### Option 3: Manual Installation

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

# Load and start the service
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

The `llmkube-metal-agent` binary in a release archive
(`LLMKube-metal-agent_<version>_darwin_<arch>.tar.gz`) can replace the build
step: from the directory you extracted the archive into, copy
`llmkube-metal-agent` instead of `bin/llmkube-metal-agent`. The archive carries
`deployment/macos/` but not the Makefile.

Release binaries carry an embedded Info.plist (bundle identifier
`dev.llmkube.metal-agent`, a display name and the Local Network usage
description) and a code signature bound to it. A release built with the project's signing
secrets is signed with a Developer ID certificate and notarized by Apple; check
a binary with:

```bash
codesign -dv --verbose=4 llmkube-metal-agent   # Identifier=dev.llmkube.metal-agent, Info.plist entries=9
spctl --assess --type install -vv llmkube-metal-agent   # accepted, source=Notarized Developer ID
```

A bare executable cannot carry a stapled notarization ticket, so Gatekeeper
checks it online the first time a downloaded copy runs. A release built
without the secrets is ad-hoc signed: it runs once copied into place, but if
macOS refuses a downloaded copy, build from source. A binary from
`make build-metal-agent` has no Info.plist and only the Go linker's ad-hoc
signature. If an agent already running under launchd fails to start after you
replace its binary with a differently signed one, reload it as in "Applying a
plist change" below.

## Usage

Once installed, the Metal agent runs automatically in the background and watches for InferenceService resources in your Kubernetes cluster.

### Deploy a Model with Metal Acceleration

```bash
# Deploy from catalog (--context caps the 131072-token catalog default so the
# model fits the agent's memory budget on most Macs)
llmkube deploy llama-3.1-8b --accelerator metal --context 8192

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

The agent is configured through the `ProgramArguments` array of the installed
plist, `~/Library/LaunchAgents/com.llmkube.metal-agent.plist`. The template in
this directory starts with:

```xml
<key>ProgramArguments</key>
<array>
    <string>/usr/local/bin/llmkube-metal-agent</string>
    <string>--namespace</string>
    <string>default</string>              <!-- Kubernetes namespace to watch -->
    <!-- Optional: --model-store <path>; default ~/Library/Application Support/llmkube/models -->
    <string>--llama-server</string>
    <string>/opt/homebrew/bin/llama-server</string>  <!-- Path to llama-server binary -->
    <string>--port</string>
    <string>9090</string>                 <!-- Agent metrics port -->
    <string>--log-level</string>
    <string>info</string>
</array>
```

`make install-metal-agent` rewrites the installed plist from this template
every time it runs (adding only `--kubeconfig` and `--kube-context` when
`METAL_KUBECONFIG` or `METAL_KUBE_CONTEXT` is set), so flags you added to the
installed copy are lost on a reinstall. Re-add them afterwards, or keep them in
your checkout's copy of the template.

### Applying a plist change

launchd keeps running the definition it loaded, so after editing the installed
plist, unload the job, wait for it to go away, and load it again:

```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

`bootout` returns before launchd has finished, and a `bootstrap` that runs too
soon can fail with `Bootstrap failed: 5: Input/output error`, hence the wait.
To restart the agent without a plist change, run
`launchctl kickstart -k gui/$(id -u)/com.llmkube.metal-agent`.
Either way the engines keep running and the new agent adopts them; see
[Agent restarts, upgrades and stopping engines](#agent-restarts-upgrades-and-stopping-engines).

### Agent restarts, upgrades and stopping engines

Engines keep running when the agent stops: `SIGTERM`, `launchctl kickstart -k`,
a crash and `make install-metal-agent` upgrades all leave them serving. The next
agent process adopts each engine with the same PID and no model reload, and logs
`adopted engine from a previous agent process`. In relay mode, requests through
the Service fail only for the few seconds the agent itself is down; in legacy
mode they keep succeeding.

Adoption checks each engine before trusting it. Its PID must be alive, lead
the recorded process group and have the recorded kernel start time. A process
that fails these checks is provably not the engine the agent started, so it is
never signalled and its record is discarded. One that passes them is ours, and
is adopted when it also runs the recorded executable (the resolved binary or
the path it was started through, so a `brew upgrade` that repoints
`/opt/homebrew/bin/llama-server` does not prevent adoption), answers its health
check, and was started for the same bind mode (legacy `0.0.0.0` or relay
loopback). An engine that is ours but cannot be adopted is stopped and started
fresh; one that cannot be stopped keeps its record for the next attempt.
Ollama services are re-attached while the Ollama daemon answers its health
check. A service no longer assigned to this Mac (allowlist change,
reassignment, Model deleted) is stopped after the agent's first list.
`llmkube_metal_agent_engines_adopted_total{result}` counts the outcomes.

oMLX services still reload when the agent restarts; adopting the oMLX daemon is
planned.

To stop the engines, stop the agent first and wait for it to exit, then run
`stop-engines`. `bootout` returns while the agent is still stopping, and an
agent still running can start an engine after `stop-engines` has read the
records, or let its health monitor respawn one; either engine outlives the
agent:

```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
llmkube-metal-agent stop-engines
```

Run `stop-engines` as the user the agent runs as, not with `sudo`: it refuses
records that user does not own, or that others could write.

`stop-engines` stops every engine the agent started (as a process group, after
the same start-time check), unloads the model of each Ollama service, and
removes each record once its engine is confirmed stopped. A record whose
process is gone, or is now another program, is removed without a signal. An
engine it cannot stop keeps its record, and the command exits 1 after trying
the rest. With a kubeconfig it first withdraws each service's endpoint. It
takes `--state-dir`, `--namespace`, `--kubeconfig`, `--kube-context`,
`--ollama-port` and `--log-level`. Only `--state-dir` must match the agent's,
since the records live under it; `--namespace` only affects log lines.
`make uninstall-metal-agent` does both steps and then removes the binary and
plist.

Adopted engines keep the flags they started with, so a new agent default does
not reach a running engine. After an upgrade, to apply new defaults, stop the
engines and start the agent again:

```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
llmkube-metal-agent stop-engines
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

Rolling the agent back to a release that predates engine adoption doubles
every model: the older agent does not know the running engines, leaves them
loaded and starts a second copy of each. Before installing an older agent,
stop the agent and run `stop-engines` with the current binary, then install
the older one:

```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
llmkube-metal-agent stop-engines   # the current binary, before it is replaced
# then install the older agent and load its plist
```

If `stop-engines` exits 1, an engine is still running; check its output and
run it again before installing the older agent.

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
- The dial timeout is 10 seconds; there is no overall download timeout, so a slow but healthy transfer of a large model is not cut short. What bounds a download is its size: see "`--max-download-size` flag (download cap)" below.
- A DNS64 address is judged by its embedded IPv4 address, so a public IPv4-only host stays reachable on a DNS64 network while the loopback and private ranges stay blocked.

### `spec.sha256` digest verification

When a `Model`'s `spec.sha256` is set, the agent verifies a downloaded source against it before the file is used. A mismatch deletes the file and refuses to start the InferenceService with reason `ModelDigestMismatch` (a Warning Event, plus `status.schedulingStatus`/`status.schedulingMessage`). A verified file gets a `<file>.sha256` stamp beside it, so a later restart does not re-hash a potentially huge model unless the stamp is missing or stale. The stamp is the same `digest size mtime` triple the cluster-side download init container writes, so a cache populated by either one is legible to the other; a bare-digest stamp from an older agent release is not trusted, and is hashed once and rewritten.

`spec.sha256` is enforced for sources the agent downloads for llama-server (the `llamacpp` runtime): `http`, `https`, `hf` and `s3`. A local-path or `file://` source is loaded in place and is never hashed. The other Metal runtimes (mlx-server, vllm-swift, oMLX, TensorFold and Ollama) do not download through the agent and ignore `spec.sha256`.

The agent remembers a mismatch so it does not re-download the same bad source on every reconcile. Changing the Model's `source` or `sha256` clears that memo automatically, since it is keyed to the exact source and digest it was recorded against; the next reconcile then retries the download. If the spec is unchanged (for example, the same URL now serves a corrected file), recreate the `Model` or the `InferenceService` instead: deleting either one clears the memo, so the next reconcile downloads and verifies it again.

### `--max-download-size` flag (download cap)

The agent caps the bytes a single Model download may write, so a broken or hostile source cannot fill the Mac's disk. The cap counts bytes a resumed download already has on disk, and it covers every download the agent makes for llama-server (`http`, `https`, `hf` and `s3`, including the from-zero restart when a resume is not possible):

- A source whose declared size (the HEAD probe, the response's `Content-Length`, or the s3 object size) is over the cap is refused before anything is written, and a partial left for it by an earlier attempt is removed.
- A body that omits its `Content-Length` or keeps streaming is stopped one byte past the cap, and its partial file is removed.

The cap does not cover downloads the agent does not make itself: Ollama pulls, which the Ollama daemon writes into its own store, and llama-server's own download flags (`-hf`, `--model-url` and similar), which are only allowed with `--allow-unsafe-extra-args`.

Either way the start is refused with reason `ModelDownloadTooLarge` (a Warning Event, plus `status.schedulingStatus`/`status.schedulingMessage`). Downloads within the cap, resumed ones included, behave exactly as before.

The default is the Mac's total physical memory. The agent wires model weights into memory (mlock), and memory admission only starts a model that fits the memory budget, so a single model file larger than RAM is not one the agent can serve. Set the cap explicitly as a byte count or a Kubernetes quantity, or `0` to disable it:

```bash
llmkube-metal-agent --max-download-size=200Gi
```

To set this in the launchd plist:

```xml
    <string>--max-download-size</string>
    <string>200Gi</string>
```

The agent logs the effective cap as `maxDownloadBytes` at startup. It refuses to start if the flag does not parse, or if the flag is unset and the total memory cannot be read.

Like a digest mismatch, the refusal is remembered, so the agent does not download the same source up to the cap on every reconcile. Changing the Model's `source` (or `sha256`) retries, and so does restarting the agent, which is also how a raised cap takes effect. Deleting the `Model` or the `InferenceService` clears the memo too, once the agent's next poll (every 5 seconds) sees it gone, so recreate it after that. A file placed in the cache slot (`<model-store>/<model>/<file>`) by hand does not lift the refusal: the memo is checked before the agent looks for a cached file, so the start stays refused until one of those resets or an agent restart.

Upgrade note: if you deliberately serve a model file larger than the Mac's physical memory (for example with a Model `spec.hardware.memoryBudget` above the Mac's RAM), raise `--max-download-size` or set it to `0` before upgrading, or the agent will refuse that download.

### Choosing the cluster (`--kubeconfig`, `--kube-context`)

The agent finds its cluster the same way controller-runtime tools do: the
`--kubeconfig` flag, then `$KUBECONFIG`, then `~/.kube/config`. Under launchd
the agent does not see your shell's `$KUBECONFIG`: the bundled plist sets only
`PATH` and `GGML_METAL_ENABLE`, so without `--kubeconfig` it reads
`~/.kube/config`. By default it uses that kubeconfig's **current context**, so
if you switch contexts with `kubectl config use-context` and the agent
restarts, it connects to whatever cluster you last selected.

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
`sourceSecretRef` secrets).

If you installed LLMKube with Helm, the chart can create the same identity for
you, so a `helm upgrade` keeps the agent's permissions current instead of
leaving them to a file someone applied by hand:

```yaml
metalAgent:
  rbac:
    enabled: true
    scope: namespaced        # cluster here means "all namespaces", and the
                             # namespaces list must then be removed
    namespaces: [default]    # the namespace(s) the agent watches; must exist
    secretNames: [llmkube-metal-relay]  # pin the Secret read; omit to read all
```

`make check-metal-agent-rbac` fails CI if the chart's rules drift from
`metal-agent-rbac.yaml`. The chart never creates the token Secret: mint the
kubeconfig below and skip its `kubectl apply` step. Without Helm, apply the
manifest as the recipe does.

Mint a kubeconfig from the identity:

```bash
NS=default                                  # the namespace the agent watches (--namespace)
KC=$HOME/.kube/llmkube-metal-agent.yaml
CA=$(mktemp)                                # private temp file for the cluster CA

# Without Helm. With metalAgent.rbac.enabled the chart already created these.
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
namespaces) needs the same rules in a ClusterRole and ClusterRoleBinding,
which `metalAgent.rbac.scope: cluster` renders.

Moving from the hand-applied manifest to the chart: Helm refuses to adopt
objects it did not create, so an install that already applied
`metal-agent-rbac.yaml` fails with `invalid ownership metadata`. Delete the
three objects first, then upgrade with `metalAgent.rbac.enabled: true`.

Under `scope: cluster` the ClusterRoleBinding names the ServiceAccount in the
release namespace, because that is where the chart creates it. If you set
`serviceAccount.create: false`, the identity you bind must live there.
Deleting the ServiceAccount gives its replacement a new UID, and the API
server validates a bound token against that UID, so the token you minted for
the old one stops working: re-run `kubectl create token` and the
`set-credentials` step after the upgrade, then restart the agent.

Update an existing `metalAgent.rbac.namespaces` list and the namespaces you
removed have their ServiceAccount, Role and RoleBinding deleted on upgrade,
because Helm removes everything that leaves the release manifest. Adding a
namespace back creates a fresh ServiceAccount with a new UID, so the token
minted for the previous one stops working: re-run `kubectl create token` and
`set-credentials` for that namespace. The namespaces the chart installs into
must exist: Helm creates only the release namespace, not the ones the chart
names.

The agent's client also looks up API groups through the API server's
discovery endpoints (`/api`, `/apis`), which any authenticated identity can
normally read through the default `system:discovery` ClusterRoleBinding. If
your cluster's admins removed that binding, the ServiceAccount above needs it
granted explicitly, for example
`kubectl create clusterrolebinding llmkube-metal-agent-discovery --clusterrole=system:discovery --serviceaccount="$NS:llmkube-metal-agent"`.

### `--metal-node` flag (MetalNode reporting) and the per-Mac Role

`--metal-node <name>` makes the agent report to its `MetalNode`, the
cluster's object for this Mac (see the
[macOS Metal guide](https://llmkube.com/docs/guides/macos-metal) for the
resource, its conditions and the controller's dial-back). The agent never
writes the MetalNode. Every 15 seconds it gets and updates one Lease in
`--namespace` (named like the MetalNode, or `--metal-node-lease <name>`),
setting `renewTime`, `holderIdentity` and the `llmkube.ai/metal-node-report`
annotation with its version, hardware, ingress port and SPKI pin, and the
addresses it believes the cluster can reach it at. It never creates the
Lease: if the Lease is missing it logs
`Lease <namespace>/<name> not found; run llmkube metal token create` at most
once a minute and keeps serving. Without the flag the agent makes no Lease
call at all. With `--metal-node`, `--ingress-port` must be 1024 or above: the
cluster refuses to dial a privileged port, and the agent logs why instead of
renewing with a report the controller would reject.

`--renew-service-account-token` keeps the agent's own token current. The
kubeconfig's user must read its token from `tokenFile` (the agent refuses to
start otherwise). At half the token's lifetime the agent requests a token with
the same lifetime for its own ServiceAccount, through TokenRequest
(`serviceaccounts/token` `create`, granted by the per-Mac Role below), and
replaces the file atomically; client-go re-reads it within a minute. If the
ServiceAccount is gone (`llmkube metal remove`) or the token has expired,
renewal fails and the agent logs that the Mac must join again.
`llmkube metal join` sets up the kubeconfig and the flag.

The shared Role in `metal-agent-rbac.yaml` does not grant Leases, and must
not: RBAC cannot scope a shared rule to "the caller's own Lease", so it would
let every Mac renew every other Mac's heartbeat. Each Mac instead gets its own
ServiceAccount, bound to the shared Role, plus a small per-Mac Role scoped by
`resourceNames` to that Mac's objects.

`llmkube metal token create <mac>` creates these objects, every one labeled
`inference.llmkube.dev/metal-node: <mac>`, and refuses to take over an
existing object of the same name that lacks that label. It also creates a
single-use join Secret, `llmkube-metal-<mac>-join`. For a Mac named
`mac-studio`, the command creates the equivalent of:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: llmkube-metal-mac-studio
  labels:
    inference.llmkube.dev/metal-node: mac-studio
automountServiceAccountToken: false
---
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: mac-studio
  labels:
    inference.llmkube.dev/metal-node: mac-studio   # also what lets the controller watch it
spec:
  holderIdentity: mac-studio
  leaseDurationSeconds: 45
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: MetalNode
metadata:
  name: mac-studio
  labels:
    inference.llmkube.dev/metal-node: mac-studio
spec:
  serviceAccountName: llmkube-metal-mac-studio
  leaseName: mac-studio
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: llmkube-metal-mac-studio
  labels:
    inference.llmkube.dev/metal-node: mac-studio
rules:
  # The agent's heartbeat and report. get and update only: the Lease is
  # pre-created, and RBAC cannot restrict create by name.
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    resourceNames: ["mac-studio"]
    verbs: ["get", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: llmkube-metal-mac-studio
  labels:
    inference.llmkube.dev/metal-node: mac-studio
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: llmkube-metal-mac-studio
subjects:
  - kind: ServiceAccount
    name: llmkube-metal-mac-studio
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: llmkube-metal-mac-studio-shared
  labels:
    inference.llmkube.dev/metal-node: mac-studio
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: llmkube-metal-agent
subjects:
  - kind: ServiceAccount
    name: llmkube-metal-mac-studio
```

The Role above shows the Lease rule the agent's MetalNode reporting needs;
the command's per-Mac Role also carries the self-scoped rules joining needs.

`cmd/metal-agent/rbac_test.go` keeps the two apart: the shared Role must grant
exactly the agent's shared calls and no Lease rule. Deleting the Mac's
ServiceAccount revokes every token minted for it.

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

Without `--host-ip`, the agent auto-detects an address from the Mac's network interfaces, in this order:

1. a Tailscale address (100.64.0.0/10), if the Mac has one;
2. otherwise the first other routable IPv4 address, skipping loopback, link-local, IPv6, and the Docker and VM bridge ranges 192.168.65.0/24, 172.17.0.0/16 and 10.96.0.0/12;
3. only when neither exists, a co-located DNS fallback for a cluster on the same Mac: the IP address `host.minikube.internal` resolves to on the Mac, else the one `host.docker.internal` resolves to, else 192.168.65.254. It registers the address, never the hostname, and logs `no routable interface for host-IP auto-detect; using co-located DNS fallback`.

It does not register `localhost`: the interface pick skips loopback addresses, and the fallback registers whatever those names resolve to on the Mac. The agent logs an interface pick as `auto-detected host IP` (with the interface and the rejected candidates), and every registration as `registered endpoint` with a `hostIP` field:

```bash
grep 'auto-detected host IP\|co-located DNS fallback\|registered endpoint' ~/Library/Logs/llmkube/metal-agent.log
```

Auto-detection can pick an address the cluster cannot reach, for example a personal tailnet on a work Mac, or the wrong interface on a Mac with several. For a remote cluster, set `--host-ip` explicitly, ideally to an address with a DHCP reservation so it does not change.

The IP must be reachable from the cluster's nodes on `--ingress-port` (below).

To set this in the launchd plist, add these lines to the `ProgramArguments` array:

```xml
    <string>--host-ip</string>
    <string>192.168.1.50</string>         <!-- Your Mac's reachable IP -->
```

After editing, reload the service (see "Applying a plist change" above):
```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

### `--inference-service-allowlist` flag (several Macs on one cluster)

By default an agent claims every InferenceService in its `--namespace` whose
Model sets `hardware.accelerator: metal`. With more than one Mac on a cluster,
every agent would start every Metal model. Give each agent a comma-separated
list of the InferenceService names it should serve:

```bash
llmkube-metal-agent --inference-service-allowlist phi-4-mini,qwen-coder
```

To set this in the launchd plist:

```xml
    <string>--inference-service-allowlist</string>
    <string>phi-4-mini,qwen-coder</string>
```

Names are matched exactly against `metadata.name`. An agent without the flag
still claims everything, so set it on every agent; an InferenceService on none
of the lists is then not served.

### Ingress and client-proxy flags

Engines listen on `127.0.0.1` only; the cluster reaches them through the
agent's authenticated TLS ingress. See "Network exposure" under "Security
model" for the full flow.

| Flag | Default | Purpose |
|------|---------|---------|
| `--ingress-port` | `9443` | TLS port of the ingress, on all interfaces. Open this port, not engine ports, in the macOS firewall. |
| `--state-dir` | `~/Library/Application Support/llmkube/metal-agent` | Agent state: the ingress key and certificate in `<state-dir>/ingress/` (relay mode) and a record of each running engine in `<state-dir>/engines/` (both modes), which the next agent process uses to adopt engines that outlived the previous one (see [Agent restarts, upgrades and stopping engines](#agent-restarts-upgrades-and-stopping-engines)). `<state-dir>` and `engines/` are 0700; record files 0600. A leading `~` is expanded to the agent's home and a relative path is made absolute; the resolved path is logged at startup. |
| `--legacy-direct-endpoints` | `false` | DEPRECATED, removed in a future release. Engines on all interfaces without authentication, registered directly, no ingress. Only for a controller that predates relay support. |
| `--client-port` | `9999` | Listener on `127.0.0.1:<port>` that forwards `/v1/*` to the current engine, for clients on the Mac itself. `0` disables it. |

`--ingress-port` must differ from `--port` and `--client-port`.

The client-proxy listener only answers a request whose `Host` header names this loopback proxy (`127.0.0.1`, `localhost` or `[::1]` on `--client-port`); anything else gets 421, which blocks DNS rebinding. It also applies the same runtime path allowlist as the ingress. See "Network exposure" under "Security model" below for the full behavior.

### `--memory-fraction` flag (memory budget)

The Metal Agent estimates model memory requirements (weights + KV cache + overhead) before starting `llama-server`. If the model won't fit in the memory budget, the agent refuses to start it and sets the InferenceService status to `InsufficientMemory`.

The KV-cache term is sized from the model's GGUF attention shape (KV head count and key/value length, i.e. grouped-query attention) when the file provides it. The charge can be lower or higher than the old embedding-width formula: lower for GQA models whose KV head count is well below the attention head count (Llama 3.1), higher for head-dim models whose heads times head_dim exceeds the embedding width (Gemma), so a model admitted under the old estimate can be refused after this change. When the GGUF omits the key/value lengths, the head dims are derived as the embedding width divided by the attention head count, which is what llama-server itself uses for such files (Llama 3.1 and Qwen 2.5 GGUFs carry the KV head count but not the lengths). The embedding-width fallback remains for GGUFs without a usable attention shape at all: no scalar KV head count (absent, or stored per layer as an array on hybrid archs), no scalar head count, more KV heads than attention heads, or a head count that does not divide the embedding width when a key or value length is missing from the file. Two known over-charges remain by design: models with sliding-window layers (Gemma 3) are charged the full-attention head dims for every layer, and DeepSeek MLA-layout files are charged their stated head dims even though llama-server stores only the compressed key cache, so they are over-charged. Older deepseek2 conversions carry plain head dims and no `*_mla` keys, run the non-MLA path, and are charged exactly what llama-server allocates.

The estimate charges the total `--ctx-size` pool: llama-server shares that pool across `--parallel` slots rather than multiplying it, so admission does not scale with slot count.

A Model parsed by an older controller keeps the embedding-width estimate until its GGUF metadata is re-parsed, which happens on the next download, or after any change to the Model's spec when the Available condition records an older generation than the spec (a Ready status written before that condition existed has no such record and needs the Model recreated). Editing a local source's file on disk does nothing under the default `IfNotPresent` refresh policy; with `refreshPolicy: OnChange` the size and mtime change counts as drift, triggering a re-download and a fresh parse.

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

**The budget is shared by every model on the Mac.** A new model is admitted only if its estimate fits in the budget minus what is already committed to running models and to starts in flight. On a 64 GB Mac (48 GB budget), a second 40 GB model is refused while a first one is running, and the refusal names the services holding the memory:

```text
estimated 40.1 GB required, 40.1 GB of the 48.0 GB budget already committed to 1 service (default/llama-a), 8.0 GB free
```

A service that stops (deleted, scaled to zero, evicted under memory pressure) gives its share back. `llmkube_metal_agent_memory_committed_bytes` reports the committed total next to `llmkube_metal_agent_memory_budget_bytes`.

**A Model can lower its own budget, not raise the Mac's.** `spec.hardware.memoryBudget` and `spec.hardware.memoryFraction` on a Model still cap that model, but a value above this Mac's budget is clamped to it, with a `MemoryBudgetClamped` Warning event on the InferenceService. To let Model budgets exceed the Mac's (for example on a dedicated machine whose owner manages placement by hand), start the agent with `--allow-model-memory-budget-override`; an overriding Model budget then also raises the shared limit for that model's admission.

**Turning off mlock for one service.** The agent starts llama-server with its weights locked in memory (`--load-mode mmap+mlock`, or `--mlock` on llama.cpp builds before 0.5.0), because macOS otherwise reclaims unlocked GPU memory under pressure. A service that must not wire its weights sets its own load mode in `extraArgs`, which replaces the agent's:

```yaml
spec:
  extraArgs: ["--load-mode", "mmap"]
```

Admission still counts the full estimate for such a service.

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
   launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
   while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
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
| `llmkube_metal_agent_managed_processes` | Gauge | Number of inference processes currently managed, across every runtime (the metric's help text says llama-server) |
| `llmkube_metal_agent_process_healthy` | Gauge | Whether a process is healthy (1) or not (0). Labels: `name`, `namespace` |
| `llmkube_metal_agent_process_restarts_total` | Counter | Engine restart attempts, counted per attempt (a retry after a failed restart counts again). Labels: `name`, `namespace` |
| `llmkube_metal_agent_health_check_duration_seconds` | Histogram | Duration of health check probes. Labels: `name`, `namespace` |
| `llmkube_metal_agent_memory_budget_bytes` | Gauge | Total memory budget for model serving |
| `llmkube_metal_agent_memory_committed_bytes` | Gauge | Memory admitted to running and starting models; new models must fit in the budget minus this |
| `llmkube_metal_agent_memory_estimated_bytes` | Gauge | Estimated memory per process. Labels: `name`, `namespace` |
| `llmkube_metal_agent_system_memory_available_bytes` | Gauge | Available system memory, sampled by the memory watchdog (`--memory-watchdog-interval`, default 10s) |
| `llmkube_metal_agent_system_memory_wired_bytes` | Gauge | Wired (non-pageable) system memory, sampled by the memory watchdog |
| `llmkube_metal_agent_process_rss_bytes` | Gauge | Resident set size per managed process. Labels: `name` only (no `namespace`, so same-named InferenceServices in different namespaces share a series) |
| `llmkube_metal_agent_memory_pressure_level` | Gauge | Memory pressure level: 0 Normal, 1 Warning, 2 Critical (see `--memory-pressure-warning` and `--memory-pressure-critical`) |
| `llmkube_metal_agent_evictions_total` | Counter | Processes the memory watchdog evicted under Critical pressure (only with `--eviction-enabled`). Each eviction also emits an `Evicted` Warning event |
| `llmkube_metal_agent_engine_record_errors_total` | Counter | Engine record writes that failed and failed the start |
| `llmkube_metal_agent_engines_adopted_total` | Counter | Engine records evaluated at startup, by outcome. Labels: `result` (`adopted`, `stopped`, `discarded`) |
| `llmkube_metal_agent_evictions_skipped_total` | Counter | Times the watchdog was at Critical but did not evict. Labels: `reason` (`disabled`, `below_guard`, `empty`, `floor`, `all_protected`, `runtime_ineligible`) |
| `llmkube_metal_agent_watch_consecutive_failures` | Gauge | Consecutive failed Kubernetes lists by the InferenceService watcher; resets to 0 on success. The agent exits for a launchd restart at `--max-watch-failures` |
| `llmkube_metal_agent_watch_model_lookup_failing_services` | Gauge | InferenceServices whose referenced Model lookup is currently failing with an error other than NotFound; each is kept in its previous state until a lookup succeeds |
| `llmkube_metal_agent_fatal_exits_total` | Counter | Fatal-exit signals raised before the agent exits. Labels: `subsystem` (`watcher`, `health-server`) |
| `llmkube_metal_agent_apple_power_combined_watts` | Gauge | Combined CPU + GPU + ANE package power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_gpu_watts` | Gauge | GPU subsystem power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_cpu_watts` | Gauge | CPU subsystem power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_ane_watts` | Gauge | Apple Neural Engine power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_ingress_requests_total` | Counter | Requests answered by the authenticated ingress. Labels: `code` (HTTP status) |
| `llmkube_metal_agent_client_proxy_requests_total` | Counter | Requests answered by the host-side client proxy. Labels: `outcome` (`bad_host`, `no_backend`, `path_forbidden`, or the response's status class) |

Standard Go runtime and process metrics (`go_*`, `process_*`) are also available.

### Continuous Health Monitoring

The agent probes each engine every 30 seconds at `GET /health` (Ollama, which has no `/health` route, at `GET /`). It only probes an engine that has finished starting, so a model that is still loading is never counted as failing.

On the first failed probe the engine is marked unhealthy (`process_healthy` set to 0) and its endpoint is withdrawn (`Ready=false`), so the cluster stops routing to it. The engine keeps running, so one slow probe does not reload a large model. If a later probe succeeds, the engine is marked healthy and its endpoint re-registered.

After `--health-failure-threshold` consecutive failed probes (default `3`, about a minute after the first failure), the agent restarts the engine, much like a kubelet liveness probe:

1. It stops the unhealthy engine the way a spec change does (SIGTERM, then SIGKILL after 10 seconds; for oMLX and Ollama it unloads the model instead, since their daemon is shared) and stops tracking it. A `HealthRestart` Warning event records the stop.
2. It starts a new engine from the current InferenceService and re-registers the endpoint. `process_restarts_total` counts every restart attempt.
3. A restart that fails is retried on its own, without any change to the InferenceService. Each failure logs the next retry time and emits a `RestartFailed` Warning event.

Restarts back off per InferenceService: the first one runs at once, and each later one waits twice as long as the one before (10s, 20s, 40s, up to 5 minutes), whether the start failed or the new engine failed its health checks again. Other changes to the InferenceService (a status update, for example) do not start the engine early, but a spec change resets the backoff and starts the new spec at once. The backoff also resets once an engine has run for 10 minutes. An engine that did not stop cleanly (for example one that had to be killed) gets at least 10 seconds to release its memory before the new engine loads the weights, spec change or not.

Deleting the InferenceService, scaling it to zero, suspending it, evicting it under memory pressure and shutting down the agent all cancel a pending restart.

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
- **Download size** (`ModelDownloadTooLarge`): a download over
  `--max-download-size` (default: the Mac's total memory) is stopped, its
  partial removed, and refused. See "`--max-download-size` flag (download
  cap)" above.
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

In both modes the agent keeps one small JSON record per running engine under
`<state-dir>/engines/`, and a start fails if the record cannot be written.
Engines still stop when the agent stops in this release.

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
  `ModelDigestMismatch`, `ModelDownloadTooLarge` (Warning).

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

# Verify Metal support: the agent logs "Metal support detected" at startup,
# or "Metal support not detected" before it exits
grep "Metal support" ~/Library/Logs/llmkube/metal-agent.log

# Print the agent version (it exits before the Metal check, so this does
# not verify Metal)
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
- **Stop or scale down another service on the same Mac** when the message says the budget is already committed: the budget is shared by every model the agent runs
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

The guard allowed the host, but macOS blocked the connection. macOS Local Network Privacy ties its permission to the agent binary, so a new binary loses access to the local network until it is granted again, and the refusal surfaces as `connect: no route to host` rather than a permission error. It affects every connection the agent opens to a LAN address, including its connection to a Kubernetes API server on the LAN, not only model downloads; downloads from the internet are not affected. Incoming connections to the ingress are governed by the macOS firewall instead, which may ask again to allow them for the new binary. Grant the agent access in System Settings, Privacy & Security, Local Network, or reach the mirror over a route macOS does not treat as local (for example a VPN or tailnet address).

### `ModelDigestMismatch`

The downloaded source's SHA256 does not match the Model's `spec.sha256`. The bad file was already deleted. Fixing `spec.sha256` or `spec.source` retries on the next reconcile; if the spec was already right and the source was fixed upstream, recreate the Model (or the InferenceService) to retry. See "`spec.sha256` digest verification" above.

### `ModelDownloadTooLarge`

The Model's download went over `--max-download-size`: either the source declared a size over the cap (nothing was written) or the body ran past it (the partial was removed). The message says which, and names the cap in bytes. If the source is wrong, fix `spec.source`; the next reconcile retries. If the model really is that large, raise `--max-download-size` (or set it to `0`) and restart the agent. If the spec was already right and the source was fixed upstream, delete and recreate the Model (or the InferenceService), or restart the agent. Placing the file in the cache slot by hand is not enough: the remembered refusal is checked first, so it only lifts after one of those. See "`--max-download-size` flag (download cap)" above.

### Can't connect to Kubernetes

```bash
# Verify kubectl can reach your cluster
kubectl get nodes

# Check which context is active
kubectl config current-context

# Check kubeconfig path (the agent under launchd does not see this; it
# reads --kubeconfig, or ~/.kube/config)
echo $KUBECONFIG

# See which cluster and context the agent actually connected to
grep "connecting to Kubernetes\|connected to Kubernetes" ~/Library/Logs/llmkube/metal-agent.log

# If using minikube locally
minikube status
```

### Agent logs `referenced model lookup failed; keeping the inference service's previous state`

On every poll (every 5 seconds) the agent looks up the Model each InferenceService references, to decide whether it is a Metal service this agent runs. When that lookup fails with anything other than NotFound (an API server timeout, throttling, a 5xx while the control plane restarts, `Forbidden`), the agent cannot tell, so it leaves the service as it was: a running engine keeps serving and keeps its Service and EndpointSlice, and a service it was not running yet is not started. While the lookup fails, changes to the InferenceService (`replicas: 0`, `suspend`, flag changes) do not take effect; deleting the InferenceService, or restoring the agent's access, does.

The agent retries on every poll. It logs this warning once when a service's lookup starts failing, Debug lines while it keeps failing, and `referenced model lookup recovered` at Info when it succeeds again, and the `llmkube_metal_agent_watch_model_lookup_failing_services` gauge counts the services currently in this state. A warning followed by a recovery line around a control-plane restart is expected. If the gauge stays above zero, check that the agent's credentials can still read Models in its namespace:

```bash
grep "referenced model lookup" ~/Library/Logs/llmkube/metal-agent.log | tail -5
curl -s http://localhost:9090/metrics | grep llmkube_metal_agent_watch_model_lookup_failing_services
kubectl --kubeconfig <agent-kubeconfig> auth can-i get models.inference.llmkube.dev -n <namespace>
```

Of the possible lookup results, only a Model that no longer exists, or one that is no longer `accelerator: metal`, makes the agent stop the service.

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
# Homebrew (also removes the LaunchAgent, logs and agent state)
brew uninstall --zap --cask llmkube-metal-agent

# Using Makefile
make uninstall-metal-agent

# Or manually
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
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
7. **Monitors** engine health every 30s and restarts an engine that keeps failing its health checks, with backoff
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
the only symptom is lost speed. The install hint the agent logs when it cannot
find `tensorfold` omits this pin; use the command above. Confirm the version
inside the tool's environment after installing:

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
  absolute `spec.source` is used as-is: the controller must allow its root
  with `--allowed-host-path-roots` (Helm `modelSource.allowedHostPathRoots`),
  and the agent must have the directory in `--allowed-model-roots` (see
  "`--allowed-model-roots` flag (local model paths)" above). A Hugging Face
  style `owner/repo` source is read from `<model-store>/owner/repo`.
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

A client that reaches the `<isvc>` Service directly or the agent's
`--client-port` proxy sends the name `GET /v1/models` lists as the OpenAI
`model` field. For llama-server and oMLX that name is the served model
name: the InferenceService's `spec.modelRef` (falling back to the Model
name). With the example above it is `llama-3b-mlx`.

oMLX itself names each model after its model-store directory
(`Llama-3.2-3B-Instruct-4bit` above). So that it answers to the served
name too, the agent writes the served name as that directory's
`model_alias` in `model_settings.json` under the oMLX base path
(`~/.omlx`, passed to the daemon as `--base-path`) before it starts the
daemon. oMLX then lists the alias on `/v1/models` and accepts both the
alias and the directory name. The agent leaves every other setting in
that file alone.

Notes:

- oMLX reads `model_settings.json` only at startup. When a served name
  changes (a new `modelRef`) and the agent started the running daemon, the
  agent restarts the daemon to load it. Other models that daemon served are
  unloaded and load again on their next request. If the daemon on
  `--omlx-port` was started outside the agent, restart it yourself.
- The agent owns `model_alias` for the directories it serves. An alias set
  for one of those directories in the oMLX admin panel is replaced with the
  served name, and the same alias is removed from any other directory that
  claims it.

A ModelRouter backend that points at an oMLX InferenceService whose
`spec.runtime` is `omlx` rewrites the model name for you, so clients
routed through the ModelRouter can use the InferenceService name. Set
`spec.runtime: omlx` explicitly; the controller cannot infer it from the
agent's `--runtime` default.

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
- Documentation: https://github.com/defilantech/LLMKube#the-metal-agent
