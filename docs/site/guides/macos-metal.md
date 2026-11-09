---
title: macOS Metal Agent
description: Serve models for a Kubernetes cluster from an Apple Silicon Mac. A native macOS agent runs the engines with Metal, and the cluster reaches them through an in-cluster relay.
---

# macOS Metal Agent

The Metal agent lets an Apple Silicon Mac serve models for a Kubernetes
cluster. It runs on the Mac as a LaunchAgent, polls the cluster for
`InferenceService` resources whose Model sets `accelerator: metal`, and runs
the engine (llama.cpp's `llama-server` by default) natively, with Metal. The
engine listens on `127.0.0.1` only. The agent serves one authenticated TLS
port, and the operator puts an in-cluster relay in front of it, so pods call
the model through an ordinary Service.

The Mac is not a Kubernetes node: nothing schedules pods on it. The cluster
sees it as a `MetalNode` object and as the Services the agent registers.

The path, in order:

1. Check the [prerequisites](#prerequisites), especially the
   [network requirements](#network-requirements).
2. [Install the operator](#install-the-operator) with Helm.
3. [Install the agent and the CLI on the Mac](#install-on-the-mac) with
   Homebrew.
4. [Join the Mac](#join-a-mac) to the cluster: one command where you have
   cluster access, one on the Mac.
5. [Deploy a first model](#deploy-a-first-model) and query it.

After that: [running a Mac as a dedicated host](#running-a-mac-as-a-dedicated-host),
[restarts and upgrades](#restarts-and-upgrades),
[more than one Mac](#more-than-one-mac), [rejoining](#rejoining) and
[uninstalling](#uninstall). [How it works](#how-it-works), the
[reference](#reference) and [building from source](#appendix-build-from-source)
are at the end.

## Prerequisites

### The Mac

- **Apple Silicon** (M1 or later) on macOS 13 or later. An Intel Mac with
  Metal can run the agent, but much more slowly.
- **Memory.** 16 GB is enough for the first model below. The agent gives
  models 67% of RAM on a Mac with 36 GB or less and 75% above that, and
  refuses to start a model whose estimate does not fit (see
  [Memory budgets](#memory-budgets)).
- **A logged-in desktop session.** The agent is a LaunchAgent: it runs only
  while the user who joined the Mac has a desktop login session (a login at
  the Mac's screen; SSH alone is not one). Over SSH with nobody logged in,
  `join` cannot start it. See
  [Running a Mac as a dedicated host](#running-a-mac-as-a-dedicated-host).
- **Homebrew.**
- **Free ports.** The agent listens on TCP 9443 on every interface (its
  ingress) and on `127.0.0.1:9090` (health and metrics). If a local Prometheus
  or a `kubectl port-forward` already holds 9090 or 9443, the agent exits at
  startup and launchd keeps restarting it.

### Your workstation

The machine where you have admin access to the cluster. It can be the Mac
itself.

- `kubectl` and Helm, with an admin kubeconfig: `llmkube metal token create`
  creates a ServiceAccount, Roles, RoleBindings, a Lease, a Secret and a
  MetalNode for the Mac.
- The `llmkube` CLI: `brew install defilantech/tap/llmkube` (the formula also
  installs on Linux).

### Network requirements

Who opens which connection:

- **Mac to the API server.** The agent and `llmkube metal join` connect to
  the API server URL in the join string, which `token create` takes from
  your kubeconfig. If that URL works only on your workstation
  (`https://127.0.0.1:6443` on a k3s host, for example), pass
  `--server https://<address the Mac reaches>:6443` to `token create`.
- **Mac to the model source.** The agent downloads models itself: over HTTPS
  from Hugging Face in the example below. A source at a private or loopback
  address (a LAN mirror) also needs `--allowed-download-hosts` and
  [Local Network access](#local-network-access). The operator reads the
  GGUF header from the same URL to size the KV cache, so give the cluster
  outbound access to it too.
- **Cluster pods to the Mac, TCP 9443, IPv4.** The relay pod in front of
  each Metal InferenceService, and the verifier pod that checks the Mac (at
  join and every 10 minutes after), open TLS connections to the agent on
  `--ingress-port` (9443).
  The pods have to reach the Mac's address, not only the nodes. The agent
  registers IPv4 addresses only, and the verifier dials only addresses in
  `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` and `100.64.0.0/10` (the
  controller's `--metal-node-allowed-cidrs`). If the macOS firewall is on and
  asks whether `llmkube-metal-agent` may accept incoming connections, allow
  it.
- **Images the cluster pulls.** `ghcr.io/defilantech/llmkube-controller`
  (the operator), `ghcr.io/defilantech/llmkube-router-proxy` (the relay, one
  pod per Metal InferenceService) and `docker.io/curlimages/curl` (the
  verifier). Mirror them for an offline cluster; see
  [Air-gapped install](./air-gapped).

There is no outbound-only mode today. Nothing on the Mac dials out to carry
inference traffic: the cluster must be able to open connections to the Mac.

| Cluster and Mac | Result |
|---|---|
| Cluster on the same Mac (kind, minikube or k3d in Docker Desktop, OrbStack or Colima) | **Works.** The agent's connection to the API server on `127.0.0.1` stays on the Mac. Pods reach the agent at the Mac's LAN or tailnet address through the container runtime. |
| Cluster on your LAN (k3s, microk8s, kubeadm), Mac on the same LAN, no VPN on the Mac | **Works** once macOS gives the agent [Local Network access](#local-network-access). |
| Cluster on your LAN, and the Mac is also on a tailnet (Tailscale) the cluster is not on | **Works** once macOS gives the agent [Local Network access](#local-network-access). The agent registers the LAN address the join check verified, not the tailnet one. See [Which address the cluster uses](#which-address-the-cluster-uses). |
| Cluster and Mac on the same tailnet or VPN | **Works** when the cluster's pods (not only its nodes) can route to the Mac's tailnet address and the tailnet's access rules allow TCP 9443 to the Mac. |
| Managed cloud cluster (EKS, GKE, AKS), Mac at home or in an office behind NAT | **Does not work.** The cluster cannot open a connection to the Mac. `join` fails after 3 minutes with `Reachable=False (NoneReachable)`. |
| Cloud cluster with a VPN or tailnet that its pods route through to the Mac | **Needs** that VPN, as in the tailnet row, and a Mac address that does not overlap the cluster's VPC, pod or Service ranges. |

## Install the operator

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
helm install llmkube llmkube/llmkube -n llmkube-system --create-namespace
kubectl wait --for=condition=ready pod -l control-plane=controller-manager \
  -n llmkube-system --timeout=120s
```

Run `helm repo update` even if you just added the repository: on a machine
that already had it, `helm repo add` keeps the old index and `helm install`
would install an old release. For OpenShift, add `-f values-openshift.yaml`
(see [OpenShift install](./openshift-install)).

## Install on the Mac

```bash
brew install defilantech/tap/llmkube defilantech/tap/llmkube-metal-agent
```

This installs the agent as `$(brew --prefix)/bin/llmkube-metal-agent`
(Developer ID signed and notarized by Apple), the `llmkube` CLI, and
`llama.cpp`, the agent's default engine. Name both: the agent cask does not
depend on the CLI formula, because Homebrew trusts only the third-party tap
items you name on the command line (`brew trust`).

The cask does not start the agent; `join` does.

Homebrew installs the latest release. Keep the operator on the same one;
nothing checks the two against each other:

```bash
llmkube-metal-agent --version     # on the Mac
helm list -n llmkube-system       # APP VERSION is the operator's
```

If the Mac is ahead, run `helm repo update` and `helm upgrade` the operator
(see [Restarts and upgrades](#restarts-and-upgrades)).

## Join a Mac

Joining takes one command where you have cluster access and one on the Mac.
The Mac never sees your cluster credentials: it gets its own ServiceAccount
and a token it renews itself.

### Pick a namespace

The agent serves the Metal InferenceServices in one namespace, the one you
join it to, so your Models and InferenceServices go there too. Use a
namespace of its own rather than `default`. With the default Role, the joined
Mac's credential can read any Secret in that namespace by name: the agent
needs the relay token
Secret and the credentials Secrets your Models name, and it reads only those,
but the permission covers the whole namespace. It can also change Services
and EndpointSlices there (on Kubernetes 1.30 and later the chart's admission
policy limits that to the agent's own objects; see
[Limit the agent's writes](#limit-the-agents-writes-kubernetes-130)). Do not
share the namespace with unrelated workloads.

`token create` does not create the namespace:

```bash
kubectl create namespace metal
```

### On your workstation: create a join string

```bash
llmkube metal token create mac-studio -n metal
```

```
llmkube metal join llmkube://eyJ2IjoxLCJzZXJ2ZXIiOi...
valid for 30 minutes (until 2026-11-04 15:04 PST), single use, Mac mac-studio in namespace metal
CA pin: sha256:a5fee586b3b05aaff494d200d4c072e2e37126eaef344e2f295ad87479272655
```

`mac-studio` is the name the cluster will use for this Mac (a DNS label). The
first line is the whole command to run on the Mac; progress messages go to
stderr, so it copies cleanly. Until it is used or expires, the string is a
credential for the Mac's identity: paste it only on the Mac. What the command
creates is under [What `token create` creates](#what-token-create-creates).

### On the Mac: join

As the user logged in at the Mac's screen, paste the line:

```
$ llmkube metal join llmkube://eyJ2IjoxLCJzZXJ2ZXIiOi...
joining Mac mac-studio in namespace metal at https://192.168.1.10:6443
CA pin: sha256:a5fee586b3b05aaff494d200d4c072e2e37126eaef344e2f295ad87479272655 (compare it with the pin llmkube metal token create printed)
wrote /Users/you/Library/Application Support/llmkube/metal-agent/kubeconfig and its token (0600)
deleted the join Secret metal/llmkube-metal-mac-studio-join: the join string no longer works
started the com.llmkube.metal-agent LaunchAgent (/Users/you/Library/LaunchAgents/com.llmkube.metal-agent.plist)
waiting up to 3m0s for the cluster to verify it can reach this Mac
  ...
Mac mac-studio joined: the cluster reaches it at 192.168.1.50:9443 (log: /Users/you/Library/Logs/llmkube/metal-agent.log)
```

Compare the `CA pin` with the one `token create` printed: if they differ,
the string was altered on the way. If the API server is on your LAN, macOS
15 and later asks at this point whether the agent may use the local network;
allow it (see [Local Network access](#local-network-access)).

`join` mints the agent's own token, writes it and a kubeconfig under
`~/Library/Application Support/llmkube/metal-agent/`, writes the LaunchAgent
`~/Library/LaunchAgents/com.llmkube.metal-agent.plist`, starts it, and waits
until a pod in the cluster has completed a TLS handshake with the agent. The
address it prints is the one that pod reached, and the agent registers that
address for serving traffic too, unless you give it `--host-ip` (see
[Which address the cluster uses](#which-address-the-cluster-uses)).

```bash
kubectl get mn -n metal
# NAME         HOST IP        MEMORY   CHIP           REACHABLE   VERSION   AGE
# mac-studio   192.168.1.50   64Gi     Apple M4 Max   True        0.10.16   2m
```

The full sequence, the flags, and `llmkube metal leave` are under
[On the Mac: join and leave](#on-the-mac-join-and-leave).

### If join does not finish

`join` gives up after 3 minutes (`--wait`) and leaves the agent installed and
retrying. The last line it printed says what it was waiting for:

- **`waiting for the agent this join started to renew its Lease`.** The agent
  is not reaching the API server, or did not start. Read
  `~/Library/Logs/llmkube/metal-agent.log`. `connect: no route to host` to a
  LAN address is [Local Network access](#local-network-access).
  `address already in use` means another program holds port 9443 or 9090
  (`lsof -nP -iTCP:9090 -sTCP:LISTEN` names it).
- **`Reachable=False (NoneReachable)`.** The agent is connected, but no pod
  could open TCP 9443 to any of the Mac's addresses: check the
  [network requirements](#network-requirements) and the macOS firewall.
  `kubectl get mn mac-studio -n metal -o yaml` lists each address it tried
  under `status.candidates`.
- **`Reachable=False (NoCandidates)`.** None of the Mac's addresses is in the
  verifier's allowed ranges (private and `100.64.0.0/10` addresses only).
- **`waiting for the controller to observe generation 1`.** The operator is
  not running.
- **`the join string has expired or was already used`.** Run `token create`
  again for a new one.
- **`start the agent: launchctl bootstrap gui/<uid> ...`.** Nobody is logged
  in at the Mac's screen. The string is used up but the credentials are in
  place: log in and run the command the error prints.

### Local Network access

On macOS 15 and later, a program that connects to an address on your local
network needs the user's permission. The agent needs it when it reaches the
API server, or a model source, at a LAN address. A cluster on the same Mac
(API server on `127.0.0.1`) or one reached only over a tailnet does not need
it for the API server.

- **When macOS asks.** At the agent's first connection to a LAN address,
  usually during `join`. A dialog in the logged-in session asks whether LLMKube
  Metal Agent may find and connect to devices on your local network, with the
  reason "LLMKube Metal Agent connects this Mac to your Kubernetes cluster so
  it can serve models." Choose Allow. Nobody sees the dialog if no one is at
  the screen.
- **What a refusal looks like.** Not a permission error: the agent logs
  `connect: no route to host` for every connection to the LAN, `join` waits
  until it times out, and a running Mac stops heartbeating and drops out of
  the cluster a few minutes later.
- **Where to change it.** System Settings > Privacy & Security > Local
  Network. Turn the agent's entry on, or off and on again, then restart the
  agent with `launchctl kickstart -k gui/$(id -u)/com.llmkube.metal-agent`.
- **It is lost after every agent upgrade.** macOS ties the permission to the
  exact binary, so an upgraded agent starts without it, sometimes with no new
  prompt and with the old entry still shown as on. After each
  `brew upgrade --cask llmkube-metal-agent`, turn the entry off and on again
  and restart the agent.

If macOS asks in a setup that should not need it, allow it.

## Deploy a first model

Phi-4 mini at Q4_K_M is a 2.5 GB file. At the default 2048-token context it
fits the memory budget of a 16 GB Mac. Save this as `phi-4-mini.yaml`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: phi-4-mini
  namespace: metal
spec:
  source: https://huggingface.co/bartowski/microsoft_Phi-4-mini-instruct-GGUF/resolve/main/microsoft_Phi-4-mini-instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    accelerator: metal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: phi-4-mini
  namespace: metal
spec:
  modelRef: phi-4-mini
```

```bash
kubectl apply -f phi-4-mini.yaml
kubectl get inferenceservice phi-4-mini -n metal -w
# wait for PHASE Ready; the first start downloads the file to the Mac
```

The agent's log on the Mac shows the start:

```
"msg":"starting inference service","namespace":"metal","name":"phi-4-mini"
"msg":"registered endpoint","namespace":"metal","name":"phi-4-mini","service":"phi-4-mini-agent","hostIP":"192.168.1.50","port":9443,"enginePort":<allocated>
"msg":"started inference service","namespace":"metal","name":"phi-4-mini","port":<allocated>,"pid":<llama-server-pid>
```

`hostIP` in `registered endpoint` is the address serving traffic goes to. It
is the `HOST IP` that `kubectl get mn -n metal` shows unless the agent has
`--host-ip`; [Which address the cluster uses](#which-address-the-cluster-uses)
says when else they can differ.

The model stays in `~/Library/Application Support/llmkube/models/phi-4-mini/`
and is not downloaded again.

A model on a host on your LAN (an internal MinIO or mirror) needs
`--allowed-download-hosts` on the agent first: it refuses private and
loopback addresses unless allowlisted. A private source needs credentials in a
Secret named by the Model's `spec.sourceSecretRef` (`AWS_*` for `s3`,
`HF_TOKEN` for a gated Hugging Face repository), and the agent reads that
Secret only once its owner has labeled it:

```bash
kubectl label secret <name> -n metal inference.llmkube.dev/model-source-credentials=true
```

Any other Secret refuses the start with `SourceSecretNotAllowed`. The agent
re-checks a refused service on a backoff (5 seconds, doubling to once a
minute), so once the Secret is labeled the service starts within about a
minute. See
["Model-source credentials Secrets"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#model-source-credentials-secrets-sourcesecretref)
and ["Security model"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#security-model)
(which also covers `spec.sha256` verification) in `deployment/macos/README.md`.

### Query it

From a pod in the cluster, call the Service like any other InferenceService.
The port follows `spec.endpoint.port` (default 8080). The `model` field is the
served model name, the InferenceService's `spec.modelRef` (here the Model,
`modelRef` and InferenceService all share the name `phi-4-mini`):

```bash
kubectl run curl --rm -it --restart=Never -n metal --image=curlimages/curl -- \
  curl -sS http://phi-4-mini.metal.svc:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

The same on one line, for shells or paste targets that strip backslash
continuations:

```bash
kubectl run curl --rm -it --restart=Never -n metal --image=curlimages/curl -- curl -sS http://phi-4-mini.metal.svc:8080/v1/chat/completions -H 'content-type: application/json' -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

From a workstation, port-forward the Service:

```bash
kubectl port-forward -n metal svc/phi-4-mini 8080:8080
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

For a stable port on the cluster's nodes, set `spec.endpoint.type: NodePort`
(and optionally `spec.endpoint.nodePort`) on the InferenceService, or
`spec.endpoint.type: LoadBalancer` for a load balancer address.

`spec.endpoint.externalTrafficPolicy: Local` applies to this Service too. The
relay pod then sees the client's address, so a NetworkPolicy `ipBlock` that
selects `inference.llmkube.dev/service: <name>` (relay pods carry that label)
can admit by source IP. The relay runs one replica, so with `Local` only the
node that runs it answers on the NodePort, and a LoadBalancer sends traffic
only to that node.

The engine on the Mac receives connections from the agent's ingress on
`127.0.0.1`. The relay records the address it was called from in
`X-Forwarded-For`, appended to any chain an upstream gateway already set, and
the ingress passes that header on to the engine along with
`X-Forwarded-Proto` (the gateway's, when one set it; otherwise `http`). The
rightmost `X-Forwarded-For` entry is the address the relay saw: with `Local`,
the client's; with the default `Cluster`, a node's. Entries to its left came
from the caller and are only as trustworthy as whatever set them. The ingress
accepts these headers from any caller holding the namespace's relay token,
so treat that token as able to set them. Whether the engine's own logs show
the client depends on whether the engine reads `X-Forwarded-For`; one that
does not reports `127.0.0.1`.

On the Mac itself, the agent's client proxy on `127.0.0.1:9999`
(`--client-port`) forwards `/v1/*` to an engine. With one model running that
is the model; with several it picks one per request, so use the Service to
reach a specific model.

```bash
curl -sS http://127.0.0.1:9999/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

Dialling `<mac-ip>:<engine-port>` does not work: the engine is not listening
on the network.

## Running a Mac as a dedicated host

The agent is a LaunchAgent: launchd starts it in the desktop login session
of the user who joined the Mac, and it runs only while that session exists.
A Mac that serves around the clock needs:

- **A login after every restart.** Nothing runs until that user logs in. With
  FileVault on, macOS cannot log in automatically, so after a power cut or a
  macOS update the Mac serves nothing until someone logs in at its screen.
  Without FileVault, turn on automatic login for that user in System
  Settings > Users & Groups.
- **No sleep.** The agent does not keep the Mac awake. On a Mac mini or Mac
  Studio, turn on "Prevent automatic sleeping when the display is off" in
  System Settings > Energy (Energy Saver on macOS 13). A MacBook has the same
  setting under Battery > Options for when it is on power, and it sleeps when
  you close the lid. `pmset -g` shows `sleep 0` once the system no longer
  sleeps on its own.
- **A restart after power loss.** "Start up automatically after a power
  failure", in the same settings pane, together with automatic login, brings
  a desktop Mac back.

What the cluster sees when the Mac sleeps, loses its network, or restarts:

- **After about 15 seconds** the relay's readiness check fails, the Service
  has no ready endpoint and requests fail. A ModelRouter with a fallback
  backend moves to it. The InferenceService is no longer Ready and says
  `WaitingForMetalAgent`.
- **After about 2.5 to 3.5 minutes** the MetalNode's `AgentConnected`
  condition turns False (`HeartbeatStale`).
- **After 3 to 4.5 minutes** the agent's heartbeat on its EndpointSlice
  expires: the controller marks the endpoint not ready and the
  InferenceService reports `AgentHeartbeatStale` ("host may be offline").
- **After a wake** the engines are still loaded, and the Mac serves again
  once the agent is back on the network. **After a restart or a logout**
  every model loads again from disk.

## Restarts and upgrades

Engines run in their own process groups and keep running when the agent
stops. The next agent process adopts them, with the same PID and no model
reload, and logs `adopted engine from a previous agent process`. Requests
through the Mac fail while the agent itself is down.

| What happens | Engines | Picked up again without a reload |
|---|---|---|
| `launchctl kickstart -k`, or `bootout` then `bootstrap` | keep running | yes |
| The agent crashes or is killed, and launchd starts it again | keep running | yes |
| `brew upgrade --cask llmkube-metal-agent`, then `kickstart -k` | keep running | yes |
| `brew upgrade llama.cpp` | keep running the llama.cpp build they started with | yes |
| An oMLX service | its model is unloaded | no: it reloads |
| An engine still loading when the agent restarts, or one that misses its single 5-second health check | stopped | no: it reloads |
| Logout, restart, macOS update | stop | no: every model reloads after the next login |
| `llmkube metal leave`, or `llmkube-metal-agent stop-engines` | stopped on purpose | not applicable |

Upgrading from a release before 0.10.13, which had no adoption, reloads every
model once. Adopted engines keep the flags, environment and llama.cpp build
they started with. To apply new agent defaults or a new llama.cpp, stop the
engines and start the agent again:

```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
llmkube-metal-agent stop-engines
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

To upgrade, do the operator first, then each Mac:

```bash
# Where you have cluster access
helm repo update
helm upgrade llmkube llmkube/llmkube -n llmkube-system

# On each Mac
brew upgrade defilantech/tap/llmkube
brew upgrade --cask llmkube-metal-agent
launchctl kickstart -k gui/$(id -u)/com.llmkube.metal-agent
```

The running agent keeps the old binary until it restarts, hence the
`kickstart`. Then:

- **Local Network access is gone** for the new binary. If the Mac reaches the
  API server or a model source on your LAN, turn the agent's entry off and on
  in System Settings > Privacy & Security > Local Network and restart the
  agent again (see [Local Network access](#local-network-access)). Otherwise
  the Mac stops heartbeating a few minutes after the upgrade.
- **Check the versions match.** `llmkube-metal-agent --version` on the Mac,
  the `VERSION` column of `kubectl get mn -n <namespace>` for every Mac, and
  `APP VERSION` in `helm list -n llmkube-system`. Nothing enforces it.

Before upgrading across several releases, read "Upgrading" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#upgrading)
and the
[upgrade and rollback runbook](https://github.com/defilantech/LLMKube/blob/main/docs/operations/runbooks/upgrade-rollback.md):
some releases refuse an old configuration until it is fixed. Rolling the
agent back to a release from before 0.10.13 loads every model twice unless
you run `stop-engines` with the current binary first; see
["Agent restarts, upgrades and stopping engines"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#agent-restarts-upgrades-and-stopping-engines).

## More than one Mac

Each agent claims every `accelerator: metal` InferenceService in its
namespace, and `join` sets no limit. Two Macs joined to the same namespace
both start every Metal model there and overwrite each other's registration,
so traffic switches between them and the relay keeps restarting. Nothing
warns you. Use one of:

- **One namespace per Mac.** The simplest:

  ```bash
  kubectl create namespace metal-studio
  llmkube metal token create mac-studio -n metal-studio
  ```

  and put that Mac's Models and InferenceServices in `metal-studio`.
- **An allowlist on every Mac** in a shared namespace. Add
  `--inference-service-allowlist` with the comma-separated names each Mac
  serves to its plist (see [Changing the agent's flags](#changing-the-agents-flags)):

  ```xml
          <string>--inference-service-allowlist</string>
          <string>phi-4-mini,qwen-coder</string>
  ```

  Names match `metadata.name` exactly. A Mac without the flag still claims
  everything, so set it on every Mac. An InferenceService on no list is not
  served. `join --allowlist` sets it, and a rejoin keeps it.

`kubectl get mn -n <namespace>` lists the Macs joined to a namespace.

## Rejoining

Run `token create` and `join` again when:

- **The Mac was away too long.** The agent renews its 7-day token at half its
  lifetime, which needs the API server. A Mac switched off or off the network
  for more than 7 days always comes back with an expired token, and one away
  for more than 3.5 days may (less if the API server caps token lifetimes).
  The agent then logs
  `the agent's token has expired and cannot be renewed; join the Mac again`.
  `join --token-lifetime` asks for a different lifetime; the API server may
  issue a shorter one, and `join` warns when it does.
- **You revoked it** with `llmkube metal remove`, or want it in another
  namespace or cluster.

A rejoin rewrites the plist with `join`'s own arguments and keeps every other
argument the previous `join`'s plist had, hand edits included
(`--host-ip`, `--inference-service-allowlist`, `--memory-fraction` and the
rest), and prints what it keeps. A flag you give `join` again replaces the
kept value, and an empty `--host-ip=` or `--allowlist=` drops it. A plist
`join` did not write, replaced with `--force`, passes none of its arguments
on. Joining a different cluster
leaves the old cluster's MetalNode and ServiceAccount in place; run
`llmkube metal remove` there.

## Uninstall

In this order:

1. **Optional: delete the Metal InferenceServices** you no longer want while
   the agent still runs. The agent stops their engines and removes the
   Services and EndpointSlices it registered for them.
2. **On the Mac:** `llmkube metal leave`. It stops the agent, runs
   `stop-engines` (which stops every engine the agent started and marks their
   endpoints not ready), and removes the plist, the kubeconfig and the token.
3. **On your workstation:** `llmkube metal remove mac-studio -n metal`. It
   deletes the Mac's ServiceAccount first, which revokes every token the Mac
   holds, then its Role, RoleBindings, Lease, join Secret and MetalNode.
4. **On the Mac:**

   ```bash
   brew uninstall --zap --cask llmkube-metal-agent
   brew uninstall llmkube
   ```

   `--zap` also deletes the agent's state
   (`~/Library/Application Support/llmkube/metal-agent`, which holds the
   ingress key and the engine records) and its logs (`~/Library/Logs/llmkube`).
   Without `--zap`, Homebrew removes only the binary.

Do not run `brew uninstall --zap` first. It stops the agent, which leaves its
engines running with every model still in memory, and then deletes the
records `stop-engines` needs, so nothing will stop those engines except
killing them by hand. `leave` also needs the agent binary and the `llmkube`
CLI, so uninstall them last.

What stays:

- On the Mac, downloaded models in `~/Library/Application Support/llmkube/models`.
  Delete that folder to get the space back.
- In the cluster, the shared `llmkube-metal-agent` Role (`token create`
  creates it when the namespace has none), the controller's
  `llmkube-metal-relay` Secret, and for each InferenceService you kept, the
  InferenceService and its Model, the relay Deployment, and the agent's
  `<isvc>-agent` Service and EndpointSlice (marked not ready). If you made a
  namespace for the Mac, `kubectl delete namespace metal` removes all of it.

## Changing the agent's flags

`join` writes `~/Library/LaunchAgents/com.llmkube.metal-agent.plist` with its
own arguments (`--kubeconfig`, `--namespace`, `--metal-node`,
`--renew-service-account-token`, `--log-level` and `--llama-server`) plus the
ones you give it: `--host-ip`, `--allowlist`, and `--agent-arg <flag>=<value>`
for any other agent flag (see [On the Mac: join and leave](#on-the-mac-join-and-leave)). On a Mac
that is already joined, change a flag without a new join string by editing
that plist by hand:

1. Open the plist in a text editor. Keep its comment line
   `<!-- Written by llmkube metal join; llmkube metal leave removes it. -->`:
   `join` and `leave` recognize their own plist by it. Tools that rewrite the
   file, such as PlistBuddy or `plutil`, drop it.
2. Add each flag and its value as separate `<string>` entries at the end of
   the `ProgramArguments` array:

   ```xml
           <string>--host-ip</string>
           <string>192.168.1.50</string>
       </array>
   ```
3. Reload the agent. launchd keeps the definition it loaded, so unload it,
   wait until it is gone, and load the edited file:

   ```bash
   launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
   while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
   ```

   `bootout` returns before launchd has finished, and a `bootstrap` that runs
   too soon can fail with `Bootstrap failed: 5: Input/output error`, hence the
   wait. The engines keep running and are adopted.

A later `join` keeps these flags (see [Rejoining](#rejoining)).

| Flag | Why you would add it |
|---|---|
| `--host-ip <address>` | Register this address for serving, as given, instead of the one the MetalNode check verified ([Which address the cluster uses](#which-address-the-cluster-uses)). Use an address with a DHCP reservation: with a fixed `--host-ip`, an address change leaves the Mac unreachable until you edit the plist. |
| `--inference-service-allowlist <names>` | Serve only these InferenceServices ([More than one Mac](#more-than-one-mac)). |
| `--memory-fraction <0-1>` | Give models more or less of the Mac's memory ([Memory budgets](#memory-budgets)). |
| `--allowed-download-hosts <hosts,cidrs>` | Allow a model source on your LAN. |
| `--allowed-model-roots <dirs>` | Serve a GGUF that is already on the Mac. |
| `--port`, `--ingress-port`, `--client-port` | Move the agent off 9090, 9443 or 9999 when another program holds them. `--ingress-port` must be 1024 or above. |

`llmkube-metal-agent --help` lists every flag;
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#configuration)
explains them.

## How it works

### How traffic reaches the Mac

```
client pod ──▶ <isvc> Service ──▶ <isvc>-relay pod ──TLS──▶ agent ingress :9443 ──▶ engine on 127.0.0.1
               (in the cluster)   (in the cluster)          (on the Mac)
```

The agent starts `llama-server` on `127.0.0.1` and registers its TLS ingress
as the Service and EndpointSlice `phi-4-mini-agent`. The controller then
creates a relay Deployment `phi-4-mini-relay` in the cluster and points the
`phi-4-mini` Service at it. The relay pins the agent's certificate and
authenticates with a per-namespace token; the ingress forwards only an
allowlist of inference paths to the engine. Clients never need the Mac's IP
or the engine port.

```bash
kubectl get svc phi-4-mini -n metal
kubectl get pods -n metal -l inference.llmkube.dev/service=phi-4-mini
# the relay pod: phi-4-mini-relay-<hash>
```

For what the ingress authenticates, the path allowlist, token and key
rotation, see "Network exposure" in the
[`deployment/macos/README.md` security model](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#network-exposure).

### Which address the cluster uses

The MetalNode check, which `join` waits for and prints and `kubectl get mn`
shows as `HOST IP`, tries the agent's candidate addresses in an order that
follows the route to the API server, and records the first one a pod reaches
(see [The dial-back](#the-dial-back)). Serving traffic goes to the address the
agent writes into its `<isvc>-agent` EndpointSlice, which the relay dials:

- **`--host-ip`**, when it is set, as given. The check tries it first too.
- Otherwise, on an agent started with `--metal-node` (`join` sets it), **the
  address the check verified**: the MetalNode's `status.hostIP`, while
  `Reachable` is True for the agent's ingress key and port. The agent reads
  its MetalNode every 15 seconds for it.
- When the controller withdraws that verification (a failed recheck, or a
  result more than 13 minutes old with no newer check finished), **the last
  verified address**, for as long as it is still on one of the Mac's up
  interfaces.
  The first address the check tries may be one that already failed, so
  moving there on a transient failure would stop serving that was working.
  The agent logs
  `keeping the last verified host IP after verification was withdrawn` once.
  It moves when a different address is verified, when the kept one leaves
  the Mac's interfaces (a network change, a new DHCP lease), or when the
  agent restarts, since it remembers the address in memory only.
- Before any check has verified an address, and after the verified one has
  left the Mac, **the first address the check tries**: the MetalNode's
  `spec.hostIP` if set, else the agent's first candidate. A first candidate
  that verifies then needs no switch.
- Without `--metal-node`, or with no candidate at all, **an auto-detected
  address**: a Tailscale address (`100.64.0.0/10`) if the Mac has one,
  otherwise its first other routable IPv4 address (skipping loopback,
  link-local, and Docker or VM bridge ranges). Only when there is none does
  it fall back, for a cluster on the same Mac, to the address
  `host.minikube.internal` resolves to on the Mac, else the one
  `host.docker.internal` resolves to, else `192.168.65.254`, and logs
  `no routable interface for host-IP auto-detect; using co-located DNS fallback`.
  It is detected again at every heartbeat (every 30 seconds), so it follows
  a network change.

When the registered address changes, the agent re-registers its endpoints at
once and logs `host IP for the agent's endpoints changed; re-registering them`
with the old address, the new one and why. To see which address the agent
uses and why:

```bash
kubectl get mn mac-studio -n metal     # HOST IP: the address the check reached
grep "registered endpoint\|host IP for the agent's endpoints changed\|keeping the last verified host IP" \
  ~/Library/Logs/llmkube/metal-agent.log | tail -3   # hostIP: the serving address
```

The two differ when `--host-ip` names another address, and while a
verification is withdrawn: `HOST IP` is then empty and the agent keeps the
last verified address. To pin the serving address, set `--host-ip` (see
[Changing the agent's flags](#changing-the-agents-flags)); the check tries
it first, so both then use it when it verifies.

### Memory budgets

The agent estimates each model's memory cost (weights + KV cache +
overhead) before spawning `llama-server`. If the model won't fit
in the configured budget, the agent refuses to start it and marks
the InferenceService with `status.schedulingStatus: InsufficientMemory`.
Like every refusal, it is re-checked on a backoff of up to a minute,
so the service starts once memory frees up; a refusal that holds
produces one Warning Event, not one per re-check. The budget is shared by
every model on the Mac: a new model must fit in what running models and
starts in flight have not already taken.

The KV-cache term is sized from the model's GGUF attention shape
(KV head count and key/value length, i.e. grouped-query attention)
when the file provides it. The charge can be lower or higher than the
old embedding-width formula: lower for GQA models whose KV head count
is well below the attention head count (Llama 3.1), higher for
head-dim models whose heads times head_dim exceeds the embedding
width (Gemma), so a model admitted under the old estimate can be
refused after this change. When the GGUF omits the key/value lengths,
the head dims are derived as the embedding width divided by the
attention head count, which is what llama-server itself uses for such
files (Llama 3.1 and Qwen 2.5 GGUFs carry the KV head count but not
the lengths). The embedding-width fallback remains for GGUFs without
a usable attention shape at all: no scalar KV head count (absent, or
stored per layer as an array on hybrid archs), no scalar head count,
more KV heads than attention heads, or a head count that does not
divide the embedding width when a key or value length is missing from
the file.
Two known over-charges remain by design: models with sliding-window
layers (Gemma 3) are charged the full-attention head dims for every
layer, and DeepSeek MLA-layout files are charged their stated head
dims even though llama-server stores only the compressed key cache,
so they are over-charged. Older deepseek2 conversions carry plain
head dims and no `*_mla` keys, run the non-MLA path, and are charged
exactly what llama-server allocates.

The estimate charges the total `--ctx-size` pool: llama-server
shares that pool across `--parallel` slots rather than multiplying
it, so admission does not scale with slot count.

A Model parsed by an older controller keeps the
embedding-width estimate until its GGUF metadata is re-parsed, which
happens on the next download, or after any change to the Model's spec
when the Available condition records an older generation than the
spec (a Ready status written before that condition existed has no
such record and needs the Model recreated). Editing a local source's
file on disk does nothing under the default `IfNotPresent` refresh
policy; with `refreshPolicy: OnChange` the size and mtime change
counts as drift, triggering a re-download and a fresh parse.

Defaults are tuned by total system RAM:

| Total RAM | Default fraction | Budget |
|---|---|---|
| 16 GB | 67% | ~10.7 GB |
| 36 GB | 67% | ~24.1 GB |
| 48 GB | 75% | 36 GB |
| 64 GB | 75% | 48 GB |
| 128 GB | 75% | 96 GB |

Override the fraction with `--memory-fraction 0.9` for a dedicated
inference machine, or `0.5` if the Mac is also your daily-driver
workstation, in the plist as in
[Changing the agent's flags](#changing-the-agents-flags).

If the agent cannot complete the check at all (the model is not on
disk yet, the Model's `status.size` is not populated, and a HEAD
probe of the source fails), it **fails closed**: the process is not
started and the InferenceService is marked with
`status.schedulingStatus: MemoryCheckFailed` explaining why. On
Apple Silicon an unsized model is wired into unevictable memory, so
admitting it blind risks a host-level stall instead of a failed pod.
Set `--memory-check-mode warn` to restore the older log-and-proceed
behavior if you need an escape hatch.

A size probe that fails only because the source cannot be reached
right now (connection refused, no route to host, a timeout, a
temporary DNS failure, a 5xx or a 429) is retried on the next poll
without a status change or a Warning Event; nothing starts in the
meantime. The agent refuses with `MemoryCheckFailed` once such
failures have lasted 3 minutes, and at once for a definitive answer
such as a 401, 403 or 404.

The agent also implements **memory-pressure protection**: if
macOS reports critical memory pressure, the agent can evict the
lowest-priority running InferenceService, keep it stopped until
pressure returns to normal, and then start it again once available
memory has room for it. See the
[`Memory-pressure protection`](../memory-pressure-protection)
guide for tuning.

### ModelRouter integration

A Metal InferenceService can be a `ModelRouter` backend. Reference it by
name like any other local backend:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata: { name: hybrid-router, namespace: metal }
spec:
  backends:
    - name: local-mac
      inferenceServiceRef: { name: phi-4-mini }   # the InferenceService above
      tier: local
      capabilities: [chat]
    - name: cloud-opus
      external:
        provider: anthropic
        model: claude-opus-4-7
        credentialsSecretRef: { name: anthropic-key }
      tier: cloud
  rules:
    - name: pii-stays-on-mac
      match: { dataClassification: [pii] }
      route: { backends: [local-mac] }
      failClosed: true
  defaultRoute: local-mac
```

The router-proxy pod (which the controller schedules in the
cluster, *not* on the Mac) calls the `phi-4-mini` Service, and so
the relay in front of the agent's ingress, when the rule resolves
to `local-mac`. To the router the Mac-served backend looks like a
container-served one: same `InferenceServiceRef` shape, same
fail-closed semantics, same per-rule timeout budgets.

See the [`ModelRouter concept doc`](../concepts/model-router) for
the full policy model.

### Mixing Macs and GPU nodes

One controller and the same CRDs can serve models from NVIDIA nodes in the
cluster and from Macs: the Model's `accelerator` (`metal` or `cuda`) decides
which. Operationally:

- Put the Mac on a network the cluster's pods can reach (the same LAN, VPN
  or tailnet; see [Network requirements](#network-requirements)).
- On a Mac with more than one address, the agent registers the one the
  MetalNode check verified; set `--host-ip` only to pin another.
- With more than one Mac, follow [More than one Mac](#more-than-one-mac).

## Troubleshooting

**Agent not running after join**
Check `~/Library/Logs/llmkube/metal-agent.log` for the first-launch error, and
`launchctl print gui/$(id -u)/com.llmkube.metal-agent | grep state`. Common
causes: another program holds port 9090 or 9443 (`address already in use`),
or Local Network access was refused (`no route to host`).

**Pods can't reach the model**
First check which address the agent registered and why: `hostIP` in its
`registered endpoint` log line, and any
`host IP for the agent's endpoints changed` or
`keeping the last verified host IP after verification was withdrawn` line,
against `HOST IP` in `kubectl get mn` (see
[Which address the cluster uses](#which-address-the-cluster-uses)). Then test
the path from a pod, not a node, since the relay is a pod:

```bash
kubectl run reach --rm -it --restart=Never -n metal --image=curlimages/curl -- \
  curl -sSk -o /dev/null -w '%{http_code}\n' https://<mac-ip>:9443/health
# 400: the pod reached the agent (the request lacks the relay's headers)
# a curl error and 000: routing, a firewall, or the wrong address
```

Then check the agent's registration and the relay:

```bash
kubectl get endpointslice <inferenceservice-name>-agent -n metal -o yaml
# expect: the Mac's address, port 9443, and the
# llmkube.ai/agent-ingress-spki annotation
kubectl get pods -n metal -l inference.llmkube.dev/service=<inferenceservice-name>
kubectl logs -n metal deploy/<inferenceservice-name>-relay
kubectl describe inferenceservice <inferenceservice-name> -n metal
# look for RelayNotAdopted, InvalidAgentIngressPin, RelayReconcileFailed, RelaySecretNotManaged
```

**InferenceService stays at `WaitingForMetalAgent`**
It says this while the agent downloads the model on the first start. If it
stays there, check the agent log for the service's name:

- Nothing about it: no agent serves it. The Mac is joined to another
  namespace (`kubectl get mn -A`), it is asleep or logged out, or an
  allowlist leaves the service out.
- An error about it: the start failed (a download error, a full disk, an
  engine that exited while loading). Such a failure is logged but not shown
  on the InferenceService, and is tried again only when the InferenceService
  changes or the agent restarts.

**Mac drops out a few minutes after an upgrade**
Local Network access did not carry over to the new binary. See
[Local Network access](#local-network-access).

**InferenceService stuck in `InsufficientMemory`**
The agent's pre-flight estimator says the model won't fit. Either
shrink the model (use a smaller quantization), reduce the context
size in the `InferenceService` spec, stop another model on the same Mac,
or raise `--memory-fraction`. On a dedicated inference machine, `0.9` is
reasonable.

**InferenceService stuck in `MemoryCheckFailed`**
The agent could not size the model at all, so it refused to start
it. `status.schedulingMessage` lists every source it tried (local
file, Model `status.size`, HEAD probe of the source URL). The most
common causes are a gated Hugging Face repo rejecting the HEAD
probe (401), a source URL that does not return `Content-Length`, or
a source that stayed unreachable for the 3 minutes the agent retries
a failed probe quietly.
Fix the source so it can be probed, wait for the Model controller
to populate `status.size`, or start the agent with
`--memory-check-mode warn` to admit unsized models at your own
risk.

**Model download blocked by the SSRF guard, or `ModelDigestMismatch`**
The Model's source resolves to a private, loopback or link-local
address, which the agent refuses to fetch by default; the event
names `--allowed-download-hosts`. Or `spec.sha256` didn't match what
was downloaded, the bad file was deleted, and the InferenceService is
refused with reason `ModelDigestMismatch`. Both are covered in
"Security model" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#security-model).

**InferenceService refused with `ModelDownloadTooLarge`**
The Model's download went over the agent's `--max-download-size` cap
(default: the Mac's total memory, since a model file larger than RAM
cannot be wired into memory). A source that declared a larger size was
refused before anything was written; a body that streamed past the cap
was stopped and its partial file removed. Fix `spec.source` if it is
wrong, or, if the model really is that large, raise
`--max-download-size` (`0` disables the cap) and restart the agent. See
"`--max-download-size` flag (download cap)" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#--max-download-size-flag-download-cap).

**InferenceService refused with `SourceSecretNotAllowed`**
The Model's `spec.sourceSecretRef` names a Secret the agent will not
read: it is not labeled
`inference.llmkube.dev/model-source-credentials=true`, it does not
exist in the Model's namespace, the agent's RBAC does not let it read
it (a Secret outside `metalAgent.rbac.secretNames`), or it is the
relay token Secret `llmkube-metal-relay`. The message says which and
gives the `kubectl label` command; label, create, or allow the Secret
and the next poll starts the service. Keep
credentials out of the relay Secret. See
["Model-source credentials Secrets"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#model-source-credentials-secrets-sourcesecretref).

**Login Items lists the agent as "Defilan Technologies LLC"**
System Settings > General > Login Items & Extensions lists a release
agent under the name of the company that signs LLMKube's releases, not
as "LLMKube". macOS names a background item after the app it belongs
to. The agent is a plain executable that launchd starts, with no app
around it, so macOS uses the developer name on its Developer ID
certificate instead, and every agent the project signs shares that
one row. An agent you built yourself (`make install-metal-agent`) is
not Developer ID signed and shows under its executable name. After an
upgrade from a build like that, the old row can stay under that name
until macOS refreshes its records. Leave the toggle on: turning it off
stops macOS from starting the agent. To confirm the row is LLMKube's:

```bash
codesign -dv --verbose=4 "$(launchctl print gui/$(id -u)/com.llmkube.metal-agent | sed -n 's/^[[:space:]]*program = //p')" 2>&1 | grep -E '^(Identifier|Authority|TeamIdentifier)='
# Identifier=dev.llmkube.metal-agent
# Authority=Developer ID Application: Defilan Technologies LLC (SSJ3D786N9)
# Authority=Developer ID Certification Authority
# Authority=Apple Root CA
# TeamIdentifier=SSJ3D786N9
```

**macOS firewall**
The agent listens on `127.0.0.1:9090` for its own health and metrics
and serves its TLS ingress on `--ingress-port` (default 9443) for
inbound inference; engines listen on `127.0.0.1` only. If the macOS
firewall is on and asks whether `llmkube-metal-agent` may accept
incoming connections, allow it: the relay connects to the ingress
port.

**Agent log shows `referenced model lookup failed; keeping the inference service's previous state`**
The agent could not read the Model an `InferenceService` references
(an API server timeout, throttling, a control-plane restart, or a
lost `get` permission on `models`). It leaves the service as it
was, so a running model keeps serving, and retries on every poll
(every 5 seconds). While the lookup fails, changes to the
`InferenceService` (`replicas: 0`, `suspend`, flag changes) do not
take effect; deleting the `InferenceService`, or restoring the
agent's access, does. The warning is logged once when the lookup
starts failing, and `referenced model lookup recovered` follows when
it succeeds again; the
`llmkube_metal_agent_watch_model_lookup_failing_services` gauge
counts the services in this state. If it stays above zero, check the
agent's RBAC and its connection to the API server. Of the possible
lookup results, only a deleted Model, or one that is no longer
`accelerator: metal`, stops the service.

**Agent log shows `replicas=0; stopping process` unexpectedly**
A controller-side reconcile saw `spec.replicas=0` on the
`InferenceService`. Check whether something scaled it down
(another operator, a Helm upgrade reverting your spec, an
operator-managed argocd app pulling a stale value).

## Reference

### What `token create` creates

Everything is in the namespace you pass with `-n` (default: your
kubeconfig context's namespace, else `default`), named after the Mac
(`<mac>` must be a DNS label, at most 63 characters), and labeled
`app.kubernetes.io/managed-by=llmkube-cli` and
`inference.llmkube.dev/metal-node=<mac>`:

| Object | Name | Notes |
|---|---|---|
| ServiceAccount | `llmkube-metal-<mac>` | `automountServiceAccountToken: false`. The Mac's identity. |
| Role | `llmkube-metal-<mac>` | The per-Mac rules below. |
| RoleBinding | `llmkube-metal-<mac>` | Binds the per-Mac Role to the ServiceAccount. |
| RoleBinding | `llmkube-metal-<mac>-shared` | Binds the shared `llmkube-metal-agent` Role to the ServiceAccount. |
| Lease | `<mac>` | The agent's heartbeat. `leaseDurationSeconds: 45`; the label lets the MetalNode controller watch it. |
| MetalNode | `<mac>` | `spec.serviceAccountName` and `spec.leaseName` point at the two above. |
| Secret | `llmkube-metal-<mac>-join` | Opaque and empty: it exists only so the join token can be bound to it. |

The per-Mac Role grants only the Mac's own objects, all by
`resourceNames`:

```yaml
rules:
  # Renew its own credential (and no one else's).
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["llmkube-metal-<mac>"]
    verbs: ["create"]
  # Read its own MetalNode. The agent never writes it.
  - apiGroups: ["inference.llmkube.dev"]
    resources: ["metalnodes"]
    resourceNames: ["<mac>"]
    verbs: ["get"]
  # Heartbeat and report.
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    resourceNames: ["<mac>"]
    verbs: ["get", "update"]
  # Delete its join Secret once, which kills the join token.
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["llmkube-metal-<mac>-join"]
    verbs: ["delete"]
```

Everything else the agent does comes from the shared
`llmkube-metal-agent` Role, the same rules as
[`deployment/macos/metal-agent-rbac.yaml`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/metal-agent-rbac.yaml).
That Role includes `get` on Secrets in the namespace with no
`resourceNames`, which is why the Mac should have a namespace of its own.
If the namespace has no such Role, `token create` creates it from a
copy of that file built into the CLI. If one exists with different
rules (the chart's `metalAgent.rbac.secretNames` pins the Secret rule
to names, for example), it is left as it is and `token create` prints
a warning.

`token create` is safe to re-run. It updates its own objects, keeps
the Lease (the agent's heartbeat) and any `hostIP` or `verifier` you
set on the MetalNode, and refuses to take over an object with one of
these names that is not labeled for this Mac (or is labeled for
another Mac). It checks all of them before it writes anything, so a
refusal leaves the cluster as it was. If you created the
objects by hand (see [The objects for each Mac](#the-objects-for-each-mac)),
label them (`kubectl label ... inference.llmkube.dev/metal-node=<mac>`)
or delete them first.

| Flag | Default | Meaning |
|---|---|---|
| `-n`, `--namespace` | context namespace, else `default` | Namespace for the Mac's objects. |
| `--ttl` | `30m` | How long the join string works (`10m` to `24h`). |
| `--server` | the context's server | API server URL the Mac connects to, for when the Mac reaches it by another address. This machine must reach it too, to check its certificate, unless `--assume-same-server`. |
| `--assume-same-server` | off | When this machine cannot reach `--server` (an address only the Mac can reach), check the certificate the context's server presents instead. Nothing then proves `--server` is the same API server; the Mac still verifies it against the CA when it joins. |
| `--tls-server-name` | automatic | Name the Mac verifies the certificate against. It must be in the certificate. |
| `-o`, `--output` | `text` | `text` or `json`. `-o json` prints `joinCommand`, `expiresAt`, `caPin`, `namespace` and `mac` for scripts. |

### The join token

The join string carries the API server URL, the cluster's CA
certificate, the CA's pin, the namespace, the Mac's name and a join
token. The Mac trusts only that CA (no system roots).

- **Short-lived.** It works for 30 minutes (`--ttl`, from `10m`, the
  shortest token the API server issues, to `24h`). The printed expiry
  is the API server's.
- **Single use.** The token is bound to the join Secret. `join`
  trades it for the agent's own credential and then deletes the
  Secret, which invalidates the token even inside its 30 minutes
  (within ~10 seconds: the API server caches a token it just
  accepted).
- **A credential until it is used.** Until the string is redeemed or
  expires, whoever holds it can act as the Mac, including minting the
  Mac's own long-lived tokens. Treat it like a password: paste it
  only on the Mac.
- **One unused string per Mac.** Running `token create` again for the
  same Mac recreates the Secret, so a previous string that was never
  used stops working. It does **not** revoke a string that was
  already redeemed: its holder may already have minted tokens of
  their own. If a string may have leaked, run
  `llmkube metal remove <mac>` (that revokes every token for the Mac
  within ~10 seconds) and then `token create` again.
- **Checked before anything is created.** The command checks every
  field of the join string, then reads the API
  server's certificate and verifies it against your kubeconfig's CA.
  It refuses a kubeconfig with `insecure-skip-tls-verify` or no CA,
  since the Mac would have nothing to trust. When the server address
  is not in the certificate (a tailnet address, say), it sets the TLS
  server name to `kubernetes` if the certificate covers that, or
  stops and lists the names the certificate does cover.

### On the Mac: join and leave

`llmkube metal join <join-string>` runs on the Mac (it refuses to run
elsewhere). In order:

1. Decodes the string and checks the CA against its pin, then prints
   the pin. Compare it with the one `token create` printed.
2. Finds `llmkube-metal-agent`: `--agent-binary`, else on `PATH`,
   else next to `llmkube` (a release archive, a Homebrew prefix). Finds
   `llama-server`, which the agent cannot start without:
   `--llama-server`, else `/opt/homebrew/bin` or `/usr/local/bin`, else
   on `PATH`. Either one missing stops join before it changes anything,
   so the join string still works after you install it. So does a
   `com.llmkube.metal-agent` LaunchAgent that `join` did not write
   (`make install-metal-agent`, a playbook): it runs the agent with the
   Mac's own arguments, so `join` refuses to replace it without
   `--force`.
3. With the join token, which trusts only the string's CA, mints the
   agent's own token for the Mac's ServiceAccount (`--token-lifetime`,
   7 days by default).
4. Writes that token and a kubeconfig that reads it through
   `tokenFile`, both `0600`, in
   `~/Library/Application Support/llmkube/metal-agent/` (`0700`; the
   agent's state directory, which also holds its ingress identity).
5. Checks the new credentials can read the Mac's MetalNode, then
   deletes the join Secret: the join string stops working (within
   ~10 seconds, as above). A failure before this step leaves the
   string usable, so you can fix the cause and run `join` again.
6. Writes `~/Library/LaunchAgents/com.llmkube.metal-agent.plist`
   and starts it with `launchctl bootstrap gui/<uid>`, which needs a
   logged-in desktop session. A plist `join` wrote earlier is replaced
   without a copy, but the arguments it had that `join` does not set
   itself, hand edits included, carry over. A plist `join` did not
   write, replaced with `--force`, is kept as
   `com.llmkube.metal-agent.plist.bak` in the state directory, not in
   `LaunchAgents`, where launchd could load it at login; a later `join`
   does not overwrite that copy. The agent runs with `--kubeconfig`,
   `--namespace`, `--metal-node`, `--renew-service-account-token`,
   `--log-level` and `--llama-server`, plus the flags from
   `--host-ip`, `--allowlist` and `--agent-arg` and any kept from the
   previous `join`, and logs to `~/Library/Logs/llmkube/metal-agent.log`. If
   `launchctl bootstrap` fails, the string is already used, but the
   credentials are in place: the error prints the command that starts
   the agent once the cause is fixed.
7. Waits up to `--wait` (3 minutes; `0` does not wait) for the
   MetalNode to report `Reachable`, printing each change of
   `AgentConnected` and `Reachable`. Only status for the agent this
   `join` started counts: its Lease renewed after the old agent was
   stopped, the controller caught up with the MetalNode's generation,
   and `Reachable` verified for the ingress key and an address the new
   agent reports. On a re-join, the replaced agent's `Reachable=True`
   stays for minutes and does not end the wait. If the wait runs out,
   `join` fails with the last conditions it saw and leaves the agent
   installed and retrying.

| Flag | Default | Meaning |
|---|---|---|
| `--agent-binary` | on `PATH`, else next to `llmkube` | Path to `llmkube-metal-agent`. |
| `--llama-server` | Homebrew's, else on `PATH` | Path to `llama-server`; join passes the path it found to the agent. |
| `--token-lifetime` | `168h` (7 days) | Lifetime of the agent's token (at least `10m`). |
| `--wait` | `3m` | How long to wait for `Reachable`; `0` returns once the agent is started. |
| `--force` | off | Replace a `com.llmkube.metal-agent` LaunchAgent that `join` did not write. |
| `--host-ip` | auto-detect | IPv4 address the agent registers for the cluster to reach the Mac (agent `--host-ip`). |
| `--allowlist` | all | Comma-separated InferenceServices this Mac serves (agent `--inference-service-allowlist`). |
| `--agent-arg` | none | Any other agent flag as `<flag>=<value>`, such as `port=9091`; repeatable. Not the flags `join` sets itself. |

**Checks before the string is used.** `join` refuses to run as root, and
before it redeems the string it checks that this user has a logged-in
desktop session and that the agent's ports (`9090` and `9999` on
`127.0.0.1`, `9443`) are free. For a busy port it names the process, and
`--agent-arg port=9091` (or `ingress-port`, `client-port`) moves the agent
off it. A port held by the agent this `join` replaces passes.

**The agent renews its own token.** With
`--renew-service-account-token`, the agent requests a new token for
its own ServiceAccount at half the current one's lifetime and
replaces the token file; client-go re-reads the file, so nothing
restarts. The API server may issue a shorter token than asked for
(`--service-account-max-token-expiration`); `join` warns, and renewal
follows the token's real lifetime. A Mac that is off for longer than
the token has left cannot renew it: see [Rejoining](#rejoining).

`llmkube metal leave` undoes `join` on the Mac: it stops the
LaunchAgent and removes its plist, the kubeconfig and the token. Engines
outlive the agent, so once the agent has exited, `leave` runs its
`stop-engines` first, which stops the engines the agent started and
withdraws their endpoints. If `stop-engines` fails, `leave` removes
nothing; check its output and run `leave` again. An agent binary from
before `stop-engines` stops its engines when it exits, so `leave` skips
the step for it. The ingress identity and the model store stay. `leave`
refuses to run as root. A LaunchAgent `join` did not write is left running,
with its engines. If `join --force` replaced one, `leave`
prints the commands that put it back. It does not revoke
anything; the token stays valid until it expires, so also run
`llmkube metal remove` where you have cluster access.

### Revoking a Mac

```bash
llmkube metal remove mac-studio -n metal
```

`remove` deletes the Mac's ServiceAccount first and waits until it is
gone. That is the revocation: every token the Mac holds stops working
within ~10 seconds, including the ones minted from the join string or
by the agent itself (the API server keeps accepting a token it
accepted in the last ~10 seconds for up to ~10 seconds more). Then it
deletes the per-Mac Role, both RoleBindings, the Lease, the join
Secret and the MetalNode, and prints each one it removed. It never
deletes the shared `llmkube-metal-agent` Role, and objects that are
already gone are skipped, so running it twice is fine. It does not
touch the Mac itself; run `llmkube metal leave` there.

`remove` deletes only objects labeled
`inference.llmkube.dev/metal-node=<mac>`. Anything else with one of
the Mac's names (another application's Lease, objects labeled for
another Mac, a MetalNode applied by hand) is left in place and listed
as `skipped`. If the ServiceAccount is among them, nothing was
revoked and the command fails. For a Mac whose objects you applied by
hand without the label, `--include-unlabeled` also deletes unlabeled
objects with the Mac's names, with a warning for each; objects
labeled for another Mac are never deleted.

### Limit the agent's writes (Kubernetes 1.30+)

The agent's Role lets it create, update and delete any Service and
EndpointSlice in its namespace, because RBAC cannot scope a verb to a label.
A ValidatingAdmissionPolicy narrows that to the objects the agent manages:
for every ServiceAccount named `llmkube-metal-<name>`, a write must leave the
label `llmkube.ai/managed-by=metal-agent` on a Service, EndpointSlice or
Endpoints object, and an update or delete must find it there. An update of
the Service an InferenceService controls is also allowed, so the agent can
take it over after a CUDA to Metal switch or a relay hand-back. The
`llmkube-metal-` ServiceAccount prefix is reserved for Mac agents.

The Helm chart installs the policy by default
(`metalAgent.writePolicy.enabled`). Without Helm, a cluster admin applies
[`deployment/macos/metal-agent-write-policy.yaml`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/metal-agent-write-policy.yaml)
once; do not apply both. On Kubernetes older than 1.30 only the Role applies.
See
["Limiting the agent's writes"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#limiting-the-agents-writes-admission-policy-kubernetes-130)
for what it does and does not stop.

### MetalNode: the Mac as a cluster object

A `MetalNode` (short name `mn`) is one Mac, as the cluster sees it:
its hardware, the agent version, the address the cluster verified it
can reach, and whether the agent is alive. It is a custom resource,
not a Kubernetes `Node`, so nothing schedules pods onto it.

```bash
kubectl get mn -n metal
# NAME         HOST IP      MEMORY   CHIP           REACHABLE   VERSION   AGE
# mac-studio   100.64.0.2   128Gi    Apple M5 Max   True        0.10.16   3m
```

Three parties each write only what they know:

- **The admin** creates the MetalNode, a ServiceAccount for the Mac,
  and a `coordination.k8s.io` Lease for its heartbeat. `spec` names
  the ServiceAccount and the Lease, and can carry an optional
  `hostIP` (an address you know reaches the Mac; tried first and
  still verified) and `verifier` placement (`nodeSelector`,
  `tolerations`).
- **The agent** never writes the MetalNode. Started with
  `--metal-node <name>`, it renews the Lease every 15 seconds and
  puts what it knows in the Lease's `llmkube.ai/metal-node-report`
  annotation: version, chip, memory, GPU cores, Metal version, the
  ingress port, the ingress SPKI pin, the ingress certificate, and the
  addresses it thinks the cluster can reach it at, in order of
  preference. Before it reports, it reads its MetalNode's UID and makes
  its ingress certificate name it (see [The dial-back](#the-dial-back)).
- **The controller** is the only writer of MetalNode status. It
  copies the report into status, sets `AgentConnected` from the
  Lease's `renewTime`, and proves reachability with a dial-back
  before setting `Reachable` and `status.hostIP`.

`status.hostIP` is what the check verified. Without `--host-ip`, the agent
registers it for serving traffic, and keeps it through a withdrawn
verification while the address is still on the Mac (see
[Which address the cluster uses](#which-address-the-cluster-uses)).
`spec.hostIP` changes what the check tries first, and so where traffic goes
once that address verifies; `--host-ip` on the agent sets the serving
address directly, whether or not it verifies.

#### Running the agent with a MetalNode

`join` starts the agent with `--metal-node`. On a Mac set up another way,
add it to the plist:

```xml
<string>--metal-node</string>
<string>mac-studio</string>
```

The agent renews the Lease with the same name as the MetalNode;
`--metal-node-lease <name>` picks a different one. It only ever gets
and updates that Lease, and gets the MetalNode: for its UID, at start
and every 5 minutes after, and without `--host-ip` also at every
renewal, for the address to register. If the Lease does not exist, the agent logs
`Lease <namespace>/<name> not found; run llmkube metal token create`
at most once a minute and keeps serving. Without `--metal-node` the
agent makes no Lease call at all.

The candidate addresses follow the route to the API server (the
`server:` in the agent's kubeconfig): `--host-ip` first; for a
cluster on the same Mac (API server on loopback, as with kind on
Docker Desktop), `192.168.65.254`, then LAN, then tailnet; for a
cluster reached over a tailnet or VPN (`100.64.0.0/10`, or a route
through a `utun` interface), the Mac's address on that interface
first; otherwise the address the Mac's kernel sends from toward the
API server, then its other private addresses. The list is
recomputed at every renewal, so a network change reaches the cluster
within 15 seconds and triggers a new dial-back.

#### The objects for each Mac

`llmkube metal token create <mac>` creates the MetalNode, its Lease,
the Mac's ServiceAccount (`llmkube-metal-<mac>`), the per-Mac Role and
RoleBinding (`llmkube-metal-<mac>`), the binding to the shared agent
Role (`llmkube-metal-<mac>-shared`) and a single-use join Secret
(`llmkube-metal-<mac>-join`). Every one carries the label
`inference.llmkube.dev/metal-node: <mac>`, and the command refuses to
take over an existing object of the same name without it. The
MetalNode and Lease it creates for `mac-studio` are equivalent to:

```yaml
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
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: mac-studio
  labels:
    # The controller watches only Leases with this label. An unlabeled
    # Lease is still read, but only on the controller's 60 s resync.
    inference.llmkube.dev/metal-node: mac-studio
spec:
  holderIdentity: mac-studio
  leaseDurationSeconds: 45
```

The per-Mac Role grants `get` and `update` on the Mac's own Lease and
`get` on its own MetalNode, by `resourceNames`; the full set of
objects is in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#--metal-node-flag-metalnode-reporting-and-the-per-mac-role).

#### The dial-back

`Reachable` means a pod inside the cluster completed a TLS handshake
with this Mac's ingress: a server presenting the certificate the agent
reported, which names this MetalNode, with the key the agent reported.
The controller runs it as a short-lived verifier pod in the
MetalNode's namespace, owned by the MetalNode, placed by
`spec.verifier`:

- It uses the operator's `--init-container-image` (curl), runs as a
  non-root user with a read-only root filesystem (its only writable
  path is a 64 KiB in-memory `emptyDir` for the certificate), no
  capabilities and no service account token, is killed after 60
  seconds, and stops at once when deleted (no termination grace
  period). It opts out of Istio and Linkerd sidecar injection
  (`sidecar.istio.io/inject: "false"` as label and annotation,
  `linkerd.io/inject: disabled`): a sidecar would keep the pod from
  finishing and send the probe through the mesh instead of straight to
  the Mac.
- The agent's ingress certificate names the MetalNode:
  `<metadata.uid>.metal-node.llmkube.invalid` (`.invalid` never
  resolves and no public CA issues for it). The agent reads the UID
  from its MetalNode and re-issues the certificate for the same key,
  so the SPKI pin, which the relay and the EndpointSlices carry, does
  not change. The certificate is saved beside the key in the agent's
  state directory.
- For each candidate it runs curl against
  `https://<uid>.metal-node.llmkube.invalid:<ingressPort>/health`,
  with `--resolve` sending that name to the candidate address,
  `--cacert` set to the certificate the agent reported, `--capath` set
  to an empty directory (so that certificate is the only trust anchor,
  even with a curl built with a CA directory) and
  `--pinnedpubkey sha256//<pin>`, ignoring any
  `.curlrc` and any proxy settings, so it tests the direct path. The
  ingress answers 400 to a request without its relay headers, which is
  enough: curl completes the handshake only with a server whose
  certificate is the reported one, names this MetalNode and carries
  the pinned key. Another server at a reported address (an API server,
  whose address and public key any client sees) presents its own
  certificate, which does not name the MetalNode, so it fails
  (`CertificateMismatch`) whatever key and address the agent reports.
- The controller checks the reported certificate before it dials: it
  must be for the pinned key and signed by that key (otherwise the
  report is invalid), and it must name this MetalNode (otherwise
  `Reachable=False`, reason `CertificateUnbound`, until the agent binds
  it). An agent that reports a pin but no certificate (any agent from
  before this check) is never dialed: `Reachable=False`, reason
  `AgentUpgradeRequired`. Upgrade the agent; until then the MetalNode
  is not Reachable.
- The certificate is checked for its validity period on the verifier
  node's clock. The agent backdates it by a day, so only a node clock
  more than a day behind the Mac's fails a freshly re-issued one. The
  agent also re-issues it (same key) whenever it is not valid on the
  Mac's own clock, so one issued while that clock ran fast is replaced
  within 5 minutes of the clock being fixed. Deleting
  `ingress-cert.pem` (keeping `ingress-key.pem`) makes the agent issue
  a fresh one at its next start, with the same pin.
- The image's curl must be 7.57 or later and accept a self-signed
  certificate as its own trust anchor (OpenSSL and LibreSSL do; see the
  [air-gapped guide](./air-gapped) for a custom image). Any other curl
  fails closed: `Reachable` stays False.
- What the check does not cover: a server that answers any requested
  name with a certificate it self-signs on the spot with one stable
  key (some TLS-intercepting proxies and development servers do) would
  pass. The agent could ask that server for the MetalNode's name and
  report the certificate it gets back, with its key. A challenge the
  ingress has to answer with its key would close this; the check does
  not have one yet.
- Per-candidate results are `Reachable`, `Timeout`, `Refused`,
  `PinMismatch`, `CertificateMismatch`, `NotAllowed` or `Error`. The
  first `Reachable` address in order becomes `status.hostIP`, which is
  set only while `Reachable` is True.

The check reruns when the pin, the certificate, the port, the
addresses it dials or the placement change, every 10 minutes, and
after a failure with a
backoff of 30 seconds growing to 5 minutes. Two rules hold whatever
the Mac reports: at most one verifier pod exists per MetalNode at a
time (a pod being deleted still counts), and two verifier starts are
at least 30 seconds apart. The one exception is a verifier pod that
has been terminating for more than 5 minutes: it has no grace period,
so it is on a node that went away, and it stays until that node object
is removed. One such pod stops blocking new checks; two or more block
again, so a `spec.verifier` placement that keeps landing on lost nodes
cannot pile pods up, and once the last success expires the
`Reachable` message names one and says to remove the node or
force-delete the pods. Rewording a candidate's source or changing
an address that is never dialed is not a change. Only the verifier
pod the controller created (recorded by UID in
`status.verification.podUID`) can report a result. When the pin or
port changes, or the verified address drops out of the list, the old
`Reachable=True` and `hostIP` are withdrawn at once (`Reachable` is
`Unknown`, reason `VerificationPending`) until the new check lands; a
new address offered beside the verified one leaves the result
standing. A spec edit that changes the check (`spec.verifier` moves it
to other nodes, `spec.hostIP` changes what it dials) withdraws the last
result the same way, success or failure, since it was for the old
check; an edit that leaves the check as it was (the ServiceAccount,
for example) keeps it. Either way `Reachable` carries the new
`metadata.generation` once `status.observedGeneration` does, which is
what `llmkube metal join` waits for. No success stands for more than
13 minutes (the 10-minute recheck, the 2-minute verifier timeout and a minute of
slack) without a newer check finishing. A Mac whose report keeps
changing restarts the check each time, so without that bound an old
success, and its `hostIP`, would outlive the address it proved; past
it `Reachable` is `Unknown`, reason `VerificationPending`, and the
message names a verifier pod stuck terminating if one is what keeps
the check from starting. Times a verifier's node reports are kept
between the verifier's start and the moment the controller reads it,
so a node clock that is off does not change when a result expires or
when the next check runs. Put the verifier
where the relay pods for this Mac run, so it tests the network path the
relay will use: the relay dials the address the agent registers, which is
the verified one unless the agent has `--host-ip` (see
[Which address the cluster uses](#which-address-the-cluster-uses)).

While `AgentConnected` is not True, nothing is dialed: `Reachable`
is `Unknown` (reason `ReportInvalid` when the report is what is wrong,
`AgentNotConnected` otherwise), `status.hostIP` is cleared, and any
verifier pod is deleted. When the Lease is missing or
its report is invalid, the agent's reported facts (`agentVersion`,
`capacity`, `ingressPort`, `ingressSPKI`) are cleared too; a stale but
valid report keeps them as the last known facts. `status.lastHeartbeatTime`
is accurate to about a minute: it advances when `AgentConnected`
changes or when it is more than 60 seconds behind the Lease, which
holds the exact time. A Lease update that only advances `renewTime`
on schedule does not wake the controller; its 60 s resync does.

#### Security notes

- **The agent cannot mark itself reachable.** It has no write access
  to the MetalNode; only the controller sets `Reachable` and
  `hostIP`. Everything in the Lease report is treated as a claim:
  bounded, validated (a malformed report or pin, an ingress port below
  1024, or text with non-printable characters such as bidi overrides
  is `AgentConnected=False`, reason `ReportInvalid`), and verified
  where it can be. A `renewTime` more than 30 seconds in the future
  counts as stale.
- **The dial-back is bound to the Mac.** It completes TLS only with a
  server whose certificate names this MetalNode's UID and carries the
  SPKI pin the agent reported (checked with the same rule the relay
  uses), so the agent cannot get `Reachable=True` for another
  server's address by reporting that server's key. It copies no
  response body or curl error
  into status, and never passes an agent value through a shell:
  addresses must be IP literals and reach the pod as arguments.
- **Allowed CIDRs.** The verifier only dials candidates inside the
  controller's `--metal-node-allowed-cidrs` (default
  `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10`, which
  covers Docker Desktop's `192.168.65.254`). Anything else is marked
  `NotAllowed`. Loopback, link-local (where cloud metadata services
  live) and multicast addresses are never dialed, whatever the list
  says. A Mac still chooses which allowed addresses and which port
  (1024 or above) get a single TLS probe, at most once every 30
  seconds. Narrow the list to your Mac networks, and keep your pod
  and Service CIDRs out of it, if that matters in your cluster.
- **The dial-back results are visible to the Mac.** The Mac can read
  its own MetalNode, including the per-candidate results. They stay
  coarse on purpose (`Reachable`, `Timeout`, `Refused`, `PinMismatch`,
  `CertificateMismatch`, `NotAllowed`, `Error`), with no response
  bodies, curl errors or timings.
- **No Kubernetes version floor.** The MetalNode CRD uses no CEL
  rule that needs a newer Kubernetes than the rest of LLMKube;
  `spec.hostIP` is checked by the controller (`Reachable=False`,
  reason `InvalidSpec`, when it is not an IP address).

#### Conditions

| Condition | Reason | Meaning |
|---|---|---|
| `AgentConnected` | `HeartbeatFresh` | The Lease was renewed within three lease durations. |
| `AgentConnected` | `HeartbeatStale` | The agent stopped renewing (or never started), or `renewTime` is more than 30 s in the future. |
| `AgentConnected` | `LeaseNotFound` | The Lease in `spec.leaseName` does not exist. |
| `AgentConnected` | `ReportInvalid` | The report annotation is missing, malformed, or carries an invalid pin, a port below 1024, non-printable text, or an ingress certificate that is not for the pinned key or not signed by it. |
| `Reachable` | `DialBackSucceeded` | A verifier reached `status.hostIP` and the server presented the agent's certificate for this MetalNode with the pinned key. |
| `Reachable` | `NoneReachable` | No candidate answered with the agent's certificate and pinned key; see `status.candidates`. |
| `Reachable` | `NoCandidates` | No reported address is inside the allowed CIDRs. |
| `Reachable` | `PinMissing` | The agent reports no ingress pin (legacy mode). |
| `Reachable` | `AgentUpgradeRequired` | The agent reports a pin but no ingress certificate, as agents from before the certificate check do; nothing is dialed until the agent is upgraded. |
| `Reachable` | `CertificateUnbound` | The agent's ingress certificate does not name this MetalNode's UID; the agent binds it once it can read its MetalNode (see the agent log). |
| `Reachable` | `VerifierFailed` | The verifier pod could not be created, failed, or never finished (check the controller log, `spec.verifier` placement and the image pull), or a pod this MetalNode does not own holds the verifier's name. |
| `Reachable` | `VerificationPending` | `Unknown`: the pin or port changed, the verified address is no longer offered, a spec edit changed the check (`spec.verifier` or `spec.hostIP`), or the last success is more than 13 minutes old with no newer check finished, so the last result no longer applies; `hostIP` is empty until the new check lands. |
| `Reachable` | `AgentNotConnected` | `Unknown`: the agent is not connected, so nothing is dialed and `hostIP` is empty. |
| `Reachable` | `ReportInvalid` | `Unknown`: the agent's report on its Lease is invalid (see `AgentConnected`), so nothing is dialed and `hostIP` is empty. Fix the report, not the Mac's network. |
| `Reachable` | `InvalidSpec` | `spec.hostIP` is not an IP address; nothing is dialed until it is fixed. |

### Engine environment

Engines the agent starts (llama-server, mlx-server, vllm-swift,
TensorFold, oMLX) do not inherit the agent's whole environment. They get
`PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`, `LANG`, `TZ`, every `LC_*`,
`HF_HOME` and `HF_HUB_CACHE`, plus the variables the agent sets for the
runtime (`GGML_METAL_ENABLE` and `GGML_METAL_PATH_RESOURCES` for
llama-server, `TENSORFOLD_NO_UPDATE_CHECK` for TensorFold). Credentials,
`KUBECONFIG`, proxies and other overrides in the agent's launchd
environment stay with the agent.

To pass a variable through, list its exact name in
`--engine-env-passthrough` (in the plist, as in
[Changing the agent's flags](#changing-the-agents-flags)):

```xml
    <string>--engine-env-passthrough</string>
    <string>HTTPS_PROXY,NO_PROXY,MLX_MAX_OPS_PER_BUFFER</string>
```

An invalid name stops the agent at startup. See
["Engine environment" in `deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#engine-environment---engine-env-passthrough)
for the details.

### Apple Silicon power metrics (optional)

For [InferCost](https://github.com/defilantech/infercost) (LLMKube's
companion FinOps project) per-token cost attribution on Apple
Silicon, the agent can publish CPU / GPU / ANE / Combined power
gauges sourced from `powermetrics`. This is **disabled by default**
because `powermetrics` requires root.

Enable in three steps:

1. Install the bundled NOPASSWD sudoers fragment, which pins both
   the binary path and the arguments (no wildcards), so it matches
   only the one command line the agent runs. From a source checkout:

   ```bash
   make install-powermetrics-sudo
   ```

   The sampling interval is part of the pinned arguments. If you
   run the agent with a non-default `--apple-power-interval`, pass
   the same value in whole milliseconds (minimum 100), for example
   `make install-powermetrics-sudo APPLE_POWER_INTERVAL_MS=2000`
   for `--apple-power-interval=2s`. When the installed rule does
   not match, sudo refuses the agent's command and the agent logs
   `apple-power: powermetrics exited unexpectedly` with a `remedy`
   field naming the command to run. Upgrading the agent does not
   change an installed rule; re-run the install after upgrading.

2. Add `--apple-power-enabled` to the plist's `ProgramArguments`
   array (see [Changing the agent's flags](#changing-the-agents-flags)).

3. Reload the agent.

The four gauges exposed:
`llmkube_metal_agent_apple_power_combined_watts`,
`llmkube_metal_agent_apple_power_gpu_watts`,
`llmkube_metal_agent_apple_power_cpu_watts`,
`llmkube_metal_agent_apple_power_ane_watts`.
They are served on the agent's `127.0.0.1:9090/metrics`, on the Mac only.

See the
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#apple-silicon-power-metrics-for-infercost)
for the full sudoers setup and a manual install path that lets you
inspect each step before running it.

### Further reading

- [`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md): the full agent reference, including every flag, the security model, runtimes other than llama.cpp (mlx-server, oMLX, Ollama, vllm-swift, TensorFold) and the Prometheus metrics.
- [`Memory-pressure protection`](../memory-pressure-protection): eviction tuning and the InferenceService priority field.
- [`Model Router`](../concepts/model-router): policy-aware routing above Metal-served InferenceServices.
- [`Air-gapped install`](./air-gapped): Metal serving with offline or private-registry installs.

## Appendix: build from source

The Homebrew install and `join` above are the supported path. To build and
run the agent yourself instead, with a LaunchAgent you manage, use a checkout
of the release that matches the operator Helm installed:

```bash
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
git checkout v$(helm show chart llmkube/llmkube | awk '/^appVersion:/{print $2}')
```

`helm show chart` reads the same local chart index `helm install`
used, so this builds the agent from the controller's release tag
instead of from `main`. You need the Xcode Command Line Tools
(`xcode-select --install`, for `git` and `make`) and Go 1.27.3 or newer (the
version `go.mod` pins).

The agent does not need your admin credentials. From the checkout, run the
recipe in
["Dedicated kubeconfig with least-privilege RBAC"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#dedicated-kubeconfig-with-least-privilege-rbac).
It creates a least-privilege ServiceAccount, writes a kubeconfig for
it to `~/.kube/llmkube-metal-agent.yaml`, and ends by running the
bundled installer with that kubeconfig:

```bash
make install-metal-agent METAL_KUBECONFIG="$KC"   # KC=$HOME/.kube/llmkube-metal-agent.yaml
```

If you installed LLMKube with Helm, the chart can create that ServiceAccount
and its Role instead (`metalAgent.rbac`), so a `helm upgrade` keeps the agent's
permissions in step with the release. The kubeconfig is still minted by hand:
the chart never creates a token Secret. See the same README section for the
values block and the migration from the hand-applied manifest.

The recipe mints a 30-day token into a file that the kubeconfig reads through
`tokenFile`. To rotate it, mint a new token into the same file (the README
gives the one-line command); client-go re-reads the file about once a minute,
so the running agent picks it up without a restart or a plist change. This
path has no MetalNode and no self-renewing token unless you set them up as
`join` does.

Without `METAL_KUBECONFIG` the agent connects with the current context
of `~/.kube/config`, usually an admin identity. Under launchd it does
not see your shell's `$KUBECONFIG`.

This builds the agent binary, installs to
`/usr/local/bin/llmkube-metal-agent` (`sudo` asks for your password),
drops a launchd plist into `~/Library/LaunchAgents/`, and starts the
service. The build also runs `go fmt ./...` and `go vet ./...` over
the whole repository, and the first build downloads every Go module
dependency, so a first install takes noticeably longer than later
ones. The bundled plist points `--llama-server` at the Apple Silicon
Homebrew path (`/opt/homebrew/bin/llama-server`); on an Intel Mac, change
it to `/usr/local/bin/llama-server`. Each rebuild is a new binary to macOS,
so [Local Network access](#local-network-access) has to be granted again
after every reinstall.

`make install-metal-agent` rewrites the plist from the repo's template every
time, so re-add flags you set by hand after a reinstall. For a different
binary path or launch system, see
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md)
for the full plist and launchctl commands.

Check that it runs:

```bash
launchctl print gui/$(id -u)/com.llmkube.metal-agent | grep 'state = running'
curl -s http://localhost:9090/healthz
# expected: ok
```

### Upgrading a source build

Check out the matching release, unload the agent, and reinstall it
(`make install-metal-agent` alone keeps running the plist launchd already
loaded):

```bash
cd /path/to/LLMKube-checkout
git fetch --tags
git checkout v$(helm show chart llmkube/llmkube | awk '/^appVersion:/{print $2}')
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
make install-metal-agent METAL_KUBECONFIG=$HOME/.kube/llmkube-metal-agent.yaml
launchctl print gui/$(id -u)/com.llmkube.metal-agent | grep 'state = running'
```

If you installed the agent without a dedicated kubeconfig, drop the
`METAL_KUBECONFIG=...` argument. The reinstall rewrites the plist from the
template, so re-add any flags you set by hand (`--host-ip`,
`--memory-fraction` and so on).

### Uninstalling a source build

```bash
cd /path/to/LLMKube-checkout
make uninstall-metal-agent
```

That stops the agent, runs `stop-engines`, removes the binary from
`/usr/local/bin`, and deletes the plist. Model weights downloaded
into the agent's `--model-store` path stay on disk; remove them by hand if
you need the space.
