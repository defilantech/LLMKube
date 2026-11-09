---
title: APISIX in front of LLMKube
description: Put Apache APISIX's ai-proxy-multi plugin in front of an LLMKube InferenceService that requires an API key. Covers the key Secret, the APISIX route, timeouts sized from measured time to first token, health checks, fallback, client keys, one gateway for several InferenceServices, metrics, and the known gaps.
---

# APISIX in front of LLMKube

LLMKube runs the inference: it deploys vLLM or SGLang, probes it, and exports
its metrics. [Apache APISIX](https://apisix.apache.org/ai-gateway/) can sit in
front of it as the API gateway, handling what clients see: routes, client
authentication, token quotas, and failover between backends. This page shows
how to connect the two so the InferenceService accepts requests only when they
carry an API key, and APISIX holds that key and adds it to every request it
forwards.

> **Status.** LLMKube's end-to-end suite runs this setup on kind against
> APISIX 3.19.0 (Helm chart `apisix` 2.18.0), with a stub OpenAI-compatible
> backend in place of a GPU model (`test/e2e/apisix_e2e_test.go`). A lab run
> then exercised it on real hardware: vLLM serving Qwen3-0.6B on one NVIDIA
> GB10, with APISIX 3.19.0 on another node of the same cluster, and then
> SGLang with a key on the same GPU. The lab run used every snippet on this
> page except the two `rollout restart` commands and the snippets taken from
> the daily gateway described next, adapted only for names, route paths,
> namespace, node placement and resource limits; with one GPU, the second
> backend on each route was a second Service in front of the same vLLM pod
> rather than a second InferenceService.
>
> The ApisixRoute patterns in "The upstream key from a Secret", "One gateway
> for many InferenceServices" and "Client authentication", and the
> "Operating notes", come from a lab's daily inference gateway (APISIX
> 3.19.0, ingress controller 2.2.0), which serves a coding agent and a
> LiteLLM proxy. Those snippets are adapted from its configuration only for
> names, route paths, namespace and port. Its verification matrix checks
> each live route for 401 on a request without a client key, for chat with
> the key, and for a streamed chat that ends with `data: [DONE]`; it checks
> `/v1/models` through the passthrough rule, and, on one route, streamed tool
> calls, streamed reasoning deltas, and a cold prompt of about 100K tokens.
> Run with a wrong client key, every check that needs the key fails.
> Behavior described below as verified comes from the suite, the lab run or
> the daily gateway. The two endpoint fields this page uses,
> `spec.endpoint.apiKeySecretRef` and
> `spec.endpoint.externalTrafficPolicy`, were added after LLMKube 0.10.3.

## What you get

- **The InferenceService refuses requests without the key.** A direct call
  with no key gets HTTP 401. Callers go through APISIX, or hold the key.
- **APISIX adds the key.** The key is configured once per backend in APISIX's
  `ai-proxy-multi` plugin, and clients never see it.
- **Streaming passes through as it is generated.** In the e2e suite, chunks the
  backend sent one second apart arrived at the client one second apart. In
  the lab run, a 400-token answer from vLLM arrived through APISIX token by
  token, about 10 ms apart, as vLLM generated it.
- **Token counts at the gateway.** APISIX asks the backend for a usage report
  on every streamed chat request and exports prompt and completion token
  counters, which its `ai-rate-limiting` plugin can also enforce limits on.
- **Health checks and failover across InferenceServices**, when a route has
  more than one backend.

## The LLMKube side

Store the key in a Secret. Use a long random string, for example:

```bash
kubectl create namespace llm
UPSTREAM_KEY=$(openssl rand -hex 32)
kubectl create secret generic upstream-key -n llm --from-literal=key="$UPSTREAM_KEY"
```

Point the InferenceService at it with `spec.endpoint.apiKeySecretRef`. This
example serves Qwen3.8-27B-FP8 with vLLM on one GPU:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen38-27b-fp8
  namespace: llm
spec:
  source: Qwen/Qwen3.8-27B-FP8
  format: safetensors
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 1
      vendor: nvidia
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen38
  namespace: llm
spec:
  modelRef: qwen38-27b-fp8
  runtime: vllm
  image: vllm/vllm-openai:v0.31.0
  skipModelInit: true
  extraArgs: ["--served-model-name", "qwen38-27b"]
  resources:
    gpu: 1
  endpoint:
    port: 8000
    apiKeySecretRef:
      name: upstream-key
      key: key
```

The APISIX route below balances across two InferenceServices, so apply this
manifest a second time with the InferenceService named `qwen38-b`. APISIX
balances and health-checks per Service, so a second backend for it is a second
InferenceService, not a second replica. If you run only one, delete the second
instance from the route.

Check that the key is enforced. A request without it gets 401:

```bash
kubectl -n llm port-forward svc/qwen38 8000:8000 &
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8000/v1/chat/completions -d '{}'
```

`llmkube benchmark` reads the same key from `apiKeySecretRef` through your
kubeconfig and sends `Authorization: Bearer <key>`, so a keyed service needs no
extra flags:

```bash
llmkube benchmark qwen38 -n llm --report-dir ./reports
```

Reading the Secret needs `get secrets` in the service's namespace. Pass
`--api-key-file`, or set `LLMKUBE_API_KEY`, to benchmark a service whose key
LLMKube cannot read, such as one that passes `--api-key` in `spec.extraArgs`, or
a service that is not in the cluster at all. `--endpoint` runs take the same
two sources and read nothing from the cluster, so an exported
`LLMKUBE_API_KEY` is sent to whatever URL you point `--endpoint` at; prefer
`--api-key-file` there, or scope the variable to the command. The key is never
printed and never written to the report.

How LLMKube wires the key:

- vLLM reads it from the `VLLM_API_KEY` environment variable. SGLang's launcher
  writes `LLMKUBE_API_KEY` to a private config file on a memory-backed volume,
  removes the variable from the server's environment, and starts SGLang with
  `--config /run/llmkube-endpoint-key/config.yaml`. The file is readable only
  by the serving user; no other container mounts the volume. The variable
  remains in the container spec as a Secret reference, so `kubectl exec`
  sessions still receive `LLMKUBE_API_KEY`. The key itself
  appears in neither the pod spec nor the OS process arguments. An existing
  `--config` in `extraArgs` is merged, with this Secret supplying `api-key`
  and explicit CLI flags retaining precedence. Images must support SGLang's
  YAML `--config` option (checked in the pinned v0.5.15.post1 and v0.5.22).
  **The startup log remains sensitive:** SGLang 0.5.19 prints the parsed
  `api_key` among its server arguments even when loaded from a config file.
  Treat pod logs and downstream copies (Loki, SIEM, backups) as credentials.
  Upstream must redact both `api_key` and `admin_api_key` to close that
  exposure; remaining work is tracked in
  [LLMKube #2054](https://github.com/defilantech/LLMKube/issues/2054).
  vLLM does not have this command-line/log exposure. After upgrading LLMKube,
  keyed SGLang pods restart to adopt the config-file launch path; rotate keys
  previously passed on the command line or shipped to log storage.
- Both runtimes leave `/health` and `/metrics` open, so the pod's probes and
  Prometheus scraping keep working without the key. The lab run checked this
  on vLLM and on SGLang 0.5.19 (`lmsysorg/sglang:v0.5.19-cu130-runtime`,
  arm64): 401 without the key, 200 with it, `/health` and `/metrics` 200
  without it, and `/v1/models` 401 without it on both.
- The Secret reference is always required, even if you set `optional: true`.
  A missing Secret or key keeps the container from starting rather than
  starting it without a key.
- An `--api-key` you put in `extraArgs` yourself wins, and LLMKube then adds
  neither the variable nor the flag.
- The key is read when the container starts. After changing the Secret, run
  `kubectl -n llm rollout restart deployment/qwen38` and update the key in
  APISIX at the same time: requests carrying the old key get 401 once the new
  pods are up.

### APISIX outside the cluster

APISIX running in the same cluster reaches the InferenceService through its
ClusterIP Service, as above. An APISIX outside the cluster needs a NodePort or
LoadBalancer Service instead. If you also restrict which source addresses may
reach the pods, for example with a NetworkPolicy `ipBlock` that admits only
APISIX, set `externalTrafficPolicy: Local` so the pod sees APISIX's address
rather than a node's:

```yaml
  endpoint:
    port: 8000
    type: NodePort
    nodePort: 30880
    externalTrafficPolicy: Local
    apiKeySecretRef:
      name: upstream-key
      key: key
```

`externalTrafficPolicy` requires `type: NodePort` or `type: LoadBalancer`, and
leaving it unset keeps the Kubernetes default, `Cluster`. With `Local`, a node
forwards NodePort traffic only to pods on that node, so point APISIX at a node
that runs the pod, or use `type: LoadBalancer`, whose load balancer sends
traffic only to such nodes. In APISIX, the instance's `override.endpoint` then
names the node or load balancer address and port instead of the cluster DNS
name.

In the lab run, with this fragment applied, a client outside the cluster
called the NodePort on the node running the pod, and vLLM's access log showed
the client's own address. The NodePort on the other nodes did not answer.
With `externalTrafficPolicy` unset, the other nodes answered too, and the pod
saw the address of whichever node forwarded the request instead of the
client's.
Changing the field updates only the Service; the pods keep running.

## Install APISIX and route to the InferenceService

APISIX can run in standalone mode, reading its routes from a YAML file instead
of from etcd. Save this as `apisix.yaml`, replacing `<upstream-key>` (twice)
with the key you stored in the Secret (`$UPSTREAM_KEY` above):

```yaml
routes:
  - id: qwen38
    uri: /v1/chat/completions
    plugins:
      prometheus: {}
      ai-proxy-multi:
        timeout: 60000
        instances:
          - name: qwen38-a
            provider: openai-compatible
            weight: 1
            auth:
              header:
                Authorization: "Bearer <upstream-key>"
            options:
              model: qwen38-27b
            override:
              endpoint: "http://qwen38.llm.svc.cluster.local:8000/v1/chat/completions"
            checks:
              active:
                type: http
                http_path: /health
                healthy: {interval: 1, successes: 1}
                unhealthy: {interval: 1, http_failures: 2}
          # A second instance, on a second InferenceService: APISIX 3.19.0 runs
          # checks.active only when a route has two or more instances.
          - name: qwen38-b
            provider: openai-compatible
            weight: 1
            auth:
              header:
                Authorization: "Bearer <upstream-key>"
            options:
              model: qwen38-27b
            override:
              endpoint: "http://qwen38-b.llm.svc.cluster.local:8000/v1/chat/completions"
            checks:
              active:
                type: http
                http_path: /health
                healthy: {interval: 1, successes: 1}
                unhealthy: {interval: 1, http_failures: 2}
#END
```

What each part does:

- `provider: openai-compatible` with `override.endpoint` sends requests to a
  self-hosted OpenAI-compatible server at that URL.
- `auth.header` is added to every request forwarded to the instance, and to
  its health checks.
- `options.model` replaces the `model` field of every forwarded request (see
  the `options.model` section below).
- `timeout` is in milliseconds, and the 60000 here is the e2e suite's value,
  not a recommendation (see the `timeout` section below).
- `prometheus: {}` turns on APISIX's metrics for this route.
- The file must end with the `#END` line, or standalone APISIX does not load
  it.

Put the file in a ConfigMap under the key `apisix.yaml`, which is the key the
chart mounts, and install the chart in standalone mode:

```bash
kubectl -n llm create configmap apisix-standalone --from-file=apisix.yaml=apisix.yaml

helm install apisix apisix \
  --repo https://charts.apiseven.com --version 2.18.0 \
  -n llm \
  --set apisix.deployment.mode=standalone \
  --set apisix.deployment.role=data_plane \
  --set apisix.deployment.standalone.existingConfigMap=apisix-standalone \
  --set etcd.enabled=false \
  --set apisix.prometheus.enabled=true \
  --wait --timeout 5m
```

Keep `apisix.deployment.role=data_plane`. With `mode=standalone` alone, chart
2.18.0 keeps the `traditional` role, which reads its configuration from etcd,
and APISIX crash-loops trying to reach etcd at `127.0.0.1:2379`.

Chart 2.18.0 creates `apisix-gateway` as a NodePort Service by default (chart
value `service.type`), so it is reachable on every node's address. The route
above does no client authentication: anyone who reaches the gateway can use
the model with the key APISIX adds. Add one of APISIX's authentication plugins
(such as `key-auth`, `jwt-auth` or `openid-connect`) to the route, or set
`service.type` to `ClusterIP`, before anything untrusted can reach a node
("Client authentication" below shows `key-auth` on ApisixRoutes).
APISIX forwards client headers to the backend, but an instance's `auth.header`
replaces a client header of the same name, so a client's own `Authorization`
header does not reach the InferenceService (in the lab run, a request through
APISIX carrying a wrong key still got 200).

The ConfigMap holds the upstream key, so restrict who can read it as you would
a Secret. To change the routes later, recreate the ConfigMap and restart
APISIX with `kubectl -n llm rollout restart deployment/apisix`.

Clients connect to the `apisix-gateway` Service on port 80. Send a request
through it:

```bash
kubectl -n llm port-forward svc/apisix-gateway 9080:80 &
curl -sS http://localhost:9080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen38-27b","messages":[{"role":"user","content":"hello"}]}'
```

And a streamed one:

```bash
curl -sN http://localhost:9080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen38-27b","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

### The same route through the Admin API

If your APISIX keeps its configuration in etcd (the chart's default
`traditional` mode) and you manage routes through the Admin API, this is the
same route as JSON, field for field. Save it as `route.json`:

```json
{
  "id": "qwen38",
  "uri": "/v1/chat/completions",
  "plugins": {
    "prometheus": {},
    "ai-proxy-multi": {
      "timeout": 60000,
      "instances": [
        {
          "name": "qwen38-a",
          "provider": "openai-compatible",
          "weight": 1,
          "auth": {
            "header": {
              "Authorization": "Bearer <upstream-key>"
            }
          },
          "options": {
            "model": "qwen38-27b"
          },
          "override": {
            "endpoint": "http://qwen38.llm.svc.cluster.local:8000/v1/chat/completions"
          },
          "checks": {
            "active": {
              "type": "http",
              "http_path": "/health",
              "healthy": {
                "interval": 1,
                "successes": 1
              },
              "unhealthy": {
                "interval": 1,
                "http_failures": 2
              }
            }
          }
        },
        {
          "name": "qwen38-b",
          "provider": "openai-compatible",
          "weight": 1,
          "auth": {
            "header": {
              "Authorization": "Bearer <upstream-key>"
            }
          },
          "options": {
            "model": "qwen38-27b"
          },
          "override": {
            "endpoint": "http://qwen38-b.llm.svc.cluster.local:8000/v1/chat/completions"
          },
          "checks": {
            "active": {
              "type": "http",
              "http_path": "/health",
              "healthy": {
                "interval": 1,
                "successes": 1
              },
              "unhealthy": {
                "interval": 1,
                "http_failures": 2
              }
            }
          }
        }
      ]
    }
  }
}
```

Then send it to the Admin API, which the chart serves through the
`apisix-admin` Service on port 9180, with your Admin API key:

```bash
kubectl -n llm port-forward svc/apisix-admin 9180:9180 &
curl "http://127.0.0.1:9180/apisix/admin/routes" -X PUT \
  -H "X-API-KEY: ${admin_key}" \
  -d @route.json
```

The lab run applied this JSON with this command to APISIX 3.19.0 in
traditional mode (Admin API answered 201) and sent requests through the
resulting route. Two chart defaults to know about:

- `admin_key` is the chart value `apisix.admin.credentials.admin`, whose
  default is a published value. Set your own, or keep it in a Secret named by
  `apisix.admin.credentials.secretName` (keys `admin` and `viewer`).
- The Admin API accepts connections only from `127.0.0.1/24` (chart value
  `apisix.admin.allow.ipList`). `kubectl port-forward` arrives from
  localhost, so the command above works; a client in another pod, such as the
  ingress controller below, needs its address range added.

### The same route as an ApisixRoute

The APISIX Ingress Controller turns `ApisixRoute` resources
(`apisix.apache.org/v2`) into APISIX routes. The lab run used controller
2.2.0, from Helm chart `apisix-ingress-controller` 1.3.0 (the chart that
`apisix` 2.18.0 bundles as its `ingress-controller` subchart), with provider
type `apisix`: the controller writes routes through the Admin API of an
APISIX in traditional mode, like the one in the previous section. It reads
the Admin API address and key from a GatewayProxy resource that the chart
creates.

Keep the admin key in a Secret that both releases read, and let the
controller's pod reach the Admin API. On the APISIX release:

```bash
kubectl -n llm create secret generic apisix-admin-key \
  --from-literal=admin="$(openssl rand -hex 16)" \
  --from-literal=viewer="$(openssl rand -hex 16)"

# add to the APISIX helm install (traditional mode):
#   --set apisix.admin.credentials.secretName=apisix-admin-key
#   --set 'apisix.admin.allow.ipList={127.0.0.1/24,<pod CIDR>}'
```

The controller chart's `crds/` directory holds the APISIX CRDs and a copy of
the Gateway API CRDs, and `helm install` installs both unless you pass
`--skip-crds`. On a cluster whose Gateway API CRDs belong to another
controller, the lab run skipped them and created only the APISIX CRDs:

```bash
helm pull apisix-ingress-controller --repo https://apache.github.io/apisix-helm-chart \
  --version 1.3.0 --untar
kubectl create -f apisix-ingress-controller/crds/apisixic-crds.yaml
```

Save as `ingress-values.yaml`:

```yaml
config:
  provider:
    type: apisix
  kubernetes:
    ingressClass: apisix
  disableGatewayAPI: true
webhook:
  enabled: false
gatewayProxy:
  createDefault: true
  provider:
    type: ControlPlane
    controlPlane:
      service:
        name: apisix-admin
        port: 9180
      auth:
        type: AdminKey
        adminKey:
          valueFrom:
            secretKeyRef:
              name: apisix-admin-key
              key: admin
```

`disableGatewayAPI` and `webhook.enabled: false` turn off the controller's
Gateway API support and its admission webhook, which this route does not
need; leave them out if you use either. Install:

```bash
helm install apisix-ingress apisix-ingress-controller \
  --repo https://apache.github.io/apisix-helm-chart --version 1.3.0 \
  -n llm --skip-crds -f ingress-values.yaml --wait
```

Then the route, with `<upstream-key>` replaced as before:

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixRoute
metadata:
  name: qwen38
  namespace: llm
spec:
  ingressClassName: apisix
  http:
    - name: chat
      match:
        paths:
          - /v1/chat/completions
      plugins:
        - name: prometheus
          enable: true
        - name: ai-proxy-multi
          enable: true
          config:
            timeout: 60000
            instances:
              - name: qwen38-a
                provider: openai-compatible
                weight: 1
                auth:
                  header:
                    Authorization: "Bearer <upstream-key>"
                options:
                  model: qwen38-27b
                override:
                  endpoint: "http://qwen38.llm.svc.cluster.local:8000/v1/chat/completions"
                checks:
                  active:
                    type: http
                    http_path: /health
                    healthy: {interval: 1, successes: 1}
                    unhealthy: {interval: 1, http_failures: 2}
              - name: qwen38-b
                provider: openai-compatible
                weight: 1
                auth:
                  header:
                    Authorization: "Bearer <upstream-key>"
                options:
                  model: qwen38-27b
                override:
                  endpoint: "http://qwen38-b.llm.svc.cluster.local:8000/v1/chat/completions"
                checks:
                  active:
                    type: http
                    http_path: /health
                    healthy: {interval: 1, successes: 1}
                    unhealthy: {interval: 1, http_failures: 2}
```

In the lab run the controller reported the ApisixRoute `Accepted`, created
the APISIX route with both instances and their health checks, and streamed
and plain requests through APISIX's gateway answered 200. Notes:

- `ingressClassName` names the IngressClass the chart creates
  (`config.kubernetes.ingressClass`).
- The rule has no `backends`: `ai-proxy-multi` sends the request itself.
- In this example the key sits in the ApisixRoute, as it sits in the
  standalone ConfigMap. The plugin's `secretRef` field cannot hold it:
  controller 2.2.0 merges each key of that Secret into the plugin
  configuration by dot-separated path, which reaches nested objects but not
  an entry of the `instances` list. APISIX can still read the key from a
  Secret through an environment variable; see the next section.
- The chart creates cluster-scoped objects: ClusterRoles, ClusterRoleBindings
  and the IngressClass.

### The upstream key from a Secret

APISIX 3.19.0 resolves `$env://VAR` references in plugin configuration,
including inside an `ai-proxy-multi` instance and in `proxy-rewrite` header
values, so an ApisixRoute can name an environment variable of the APISIX pod
instead of holding the key. The lab's daily gateway keeps its upstream key
in a Secret this way, with no key in any ApisixRoute or values file. Two
rules shape the setup:

- APISIX's Lua code can read only the environment variables declared in its
  nginx configuration, chart value `apisix.nginx.envs`.
- `$env://` replaces a whole value; it does not interpolate inside a string.
  The variable therefore holds the whole header value, `Bearer <key>`, which
  Kubernetes builds from the Secret's raw key with `$(VAR)` expansion.

Add to the APISIX release's values (traditional mode, as above). The Secret
has to be in APISIX's namespace; here that is `llm`, so it is the
`upstream-key` Secret from "The LLMKube side":

```yaml
apisix:
  nginx:
    # Lua can read only env vars declared here; $env:// references need them.
    envs:
      - QWEN38_UPSTREAM_AUTH
extraEnvVars:
  - name: QWEN38_UPSTREAM_KEY
    valueFrom:
      secretKeyRef: {name: upstream-key, key: key}
  - name: QWEN38_UPSTREAM_AUTH
    value: "Bearer $(QWEN38_UPSTREAM_KEY)"
```

Then give each instance the variable instead of the key:

```yaml
                auth:
                  header:
                    Authorization: "$env://QWEN38_UPSTREAM_AUTH"
```

The same value works in `proxy-rewrite`'s `headers.set` (see "One gateway
for many InferenceServices" below). In the lab, a backend that echoed the
header it received got `Bearer ` followed by a value of the key's length, on
an `ai-proxy-multi` rule and on a `proxy-rewrite` rule alike. The variables
are set when the APISIX container starts, so after changing the Secret,
restart the APISIX Deployment along with the InferenceService.

If `$env://` is not available to you, a literal key in the ApisixRoute, as
in the example above, is the fallback. Restrict who can read ApisixRoutes as
you would a Secret.

## `options.model`: send the name the runtime serves

`ai-proxy-multi` writes the instance's `options.model` into every request it
forwards, replacing whatever `model` the client sent. Without it, the client's
`model` passes through unchanged, and vLLM rejects a request for any name it
does not serve.

Set `options.model` to the runtime's served model name. vLLM and SGLang get
`--served-model-name` set to `spec.modelRef` unless `extraArgs` sets it, as the
manifest above does. To check the name, ask the runtime with the key:

```bash
curl -sS http://localhost:8000/v1/models -H "Authorization: Bearer $UPSTREAM_KEY"
```

Because the name is overwritten, one route serves one model, whatever the
client asks for. To route by the name the client sends, see "Serving more
than one model" below.

## Size `timeout` from measured time to first token

The plugin's `timeout`, in milliseconds, bounds each socket operation APISIX
performs against an instance: connecting, sending, and each read. It does not
cap the length of a streamed response, but the first read waits for the first
byte of the answer, so the time to first token (TTFT) has to fit inside it.
The default is 30000 (30 seconds) and the maximum is 600000 (10 minutes).
When the timeout elapses, APISIX returns HTTP 504.

The e2e suite verifies this with a backend that takes 40 seconds to answer: a
route with `timeout: 60000` returns its answer, and a route left at the default
returns 504 after 30.0 seconds. The lab run showed the same on vLLM with a
route set to `timeout: 200`: a 256-token request that was not streamed got 504
after 0.2 seconds, while the same request streamed completed in 1.95 seconds,
because each chunk arrived well inside 200 ms.

For a request that is not streamed, the backend sends nothing until
generation ends, so the whole generation has to fit inside `timeout`, and one
that runs longer than 10 minutes always fails. Stream long generations.

Prefill time grows with prompt length and depends heavily on the runtime and
its settings. Measured on one B200 serving Qwen3.8-27B-FP8, single request,
fresh prompt (details in [SGLang on Blackwell](./sglang-blackwell)):

| Configuration | TTFT, 200K-token prompt |
|---|---|
| vLLM v0.31.0, FP8 KV cache, MTP | 13.7 s |
| SGLang, LLMKube defaults | 75.8 s |
| SGLang, `attentionBackend: flashinfer` | 29.2 s |

At the 30-second default, SGLang with LLMKube defaults returns 504 on a
200K-token prompt, and SGLang with FlashInfer has less than a second to spare.
Size `timeout` from the TTFT you measure on your own hardware, at your longest
prompt and the concurrency you expect: requests that queue behind other
prefills wait longer than a single request does.

## Health checks on `/health`

`checks.active` has APISIX probe each instance's `/health` and stop sending
requests to an instance that fails `http_failures` checks in a row, until it
passes `successes` checks again. Verified on APISIX 3.19.0:

- **The checks run only when the route has two or more instances.** With one
  instance APISIX never starts a health checker, so the block has no effect
  until you add a second instance.
- **The checks carry the instance's `auth.header`**, so they send the key.
  vLLM and SGLang leave `/health` open, so it answers 200 with or without it.
  The lab run confirmed this on vLLM by pointing `http_path` at `/v1/models`,
  which vLLM answers only with the key: the instance stayed healthy, and
  vLLM logged the checks as 200.
- **Health status takes effect gradually after APISIX loads a route.** In
  the lab run, the checks marked an instance whose Service had no endpoints
  unhealthy, yet 9 of the first 14 requests, spread over about a minute, were
  still sent to it first and succeeded only because `fallback_strategy`
  retried them on the other instance. The 80 requests after that never
  touched it. The requests came in two batches about 50 seconds apart, and
  the 4 such requests in the second batch all came from the same nginx
  worker, so the status appears to take effect per worker as each one
  handles its first requests for the route. Set `fallback_strategy` (next
  section) along with `checks`.

The example checks every second (`interval: 1`), which keeps the e2e suite
fast. Choose intervals that suit your backends. APISIX's control API reports
each checker's view (`GET /v1/healthcheck` on port 9090, which listens on
the pod's loopback address, so reach it with `kubectl port-forward` to the
pod), and the `apisix_upstream_status` metric is 1 for each healthy checked
instance.

## Fallback and retries

With more than one instance, `ai-proxy-multi` balances by `weight` within a
`priority` group, and sends requests to a lower-priority group only when every
instance in the higher one is unavailable. To retry a failed request on
another instance, set `fallback_strategy` (`http_5xx`, `http_429`,
`instance_health_and_rate_limiting`, or a list of `rate_limiting`, `http_429`
and `http_5xx`), and bound the retries with `max_retries` and
`retry_on_failure_within_ms`. A stream that has already sent bytes to the
client is never retried. The fields are described in the
[ai-proxy-multi documentation](https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/).

The e2e suite does not exercise fallback; the lab run did, with two routes
of this shape:

```yaml
      ai-proxy-multi:
        timeout: 60000
        fallback_strategy: ["http_5xx"]
        max_retries: 1        # first route only
        instances:
          - name: primary
            priority: 1
            # ... provider, weight, auth, options, override,
            # endpoint on a Service with no ready pods
          - name: secondary
            priority: 0
            # ... endpoint on the live InferenceService
```

On the first route both instances also had a `checks` block (as above, but
with `http_path: /v1/models`, as described under health checks). The second
route had no `checks` at all, so every request on it picked the primary
first. While the secondary was up, every request on both routes succeeded
(94 on the first, 4 on the second). Whenever the primary was
picked, APISIX logged `connect: Connection refused`, counted it as status
500, and retried on the secondary, so a connection failure matches
`http_5xx`. A Service with no endpoints refuses the connection at once: on
the route without checks, where every request went through that retry,
short requests still completed in 0.07 to 0.09 seconds.

The lab run also scaled the live InferenceService to 0 in the middle of a
stream on the route without checks. APISIX logged `upstream closed before
complete response` and did not retry. The client had already received
status 200 and 381 chunks; the stream stopped shortly after the scale-down,
stayed silent until the pod's 30-second termination grace period ended, and
then closed without the final `data: [DONE]` line.
Clients should treat a stream that ends without `[DONE]` as failed, and set
a read timeout so a silent stream does not hang them.

## Serving more than one model

`ai-proxy-multi` does not choose an instance by the requested model: every
instance on a route is a balancing target, and `options.model` overwrites the
name. To serve several models behind one URL, give each model its own route
and match on the request body with the route's `vars`, using the
`post_arg.model` variable (APISIX 3.14.0 and later). The
[ai-proxy documentation](https://apisix.apache.org/docs/apisix/plugins/ai-proxy/)
shows this in its "Proxy to Selected Model using Request Body Parameter"
example. Each route then carries its own `ai-proxy-multi` with that model's
InferenceServices. The other way, a path prefix per model, is in "One
gateway for many InferenceServices" below.

`GET /v1/models` cannot go through `ai-proxy-multi`, which accepts only
requests with an `application/json` body. Serve it with a plain APISIX route
whose upstream is one of the InferenceService Services. The runtime requires
the key on `/v1/models` too, so that route has to add the `Authorization`
header itself, for example with APISIX's `proxy-rewrite` plugin. The list it
returns names the served model, so setting `--served-model-name` to the name
your clients send keeps the two consistent.

The lab run verified both, with routes like these in the standalone file
(the `ai-proxy-multi` blocks are the same shape as above, one per model):

```yaml
  - id: qwen38
    uri: /v1/chat/completions
    vars: [["post_arg.model", "==", "qwen38-27b"]]
    plugins:
      ai-proxy-multi:
        # ... instances for the qwen38 InferenceServices
  - id: other-model
    uri: /v1/chat/completions
    vars: [["post_arg.model", "==", "other-model"]]
    plugins:
      ai-proxy-multi:
        # ... instances for that model's InferenceServices
  - id: models
    uri: /v1/models
    methods: ["GET"]
    plugins:
      proxy-rewrite:
        headers:
          set:
            Authorization: "Bearer <upstream-key>"
    upstream:
      type: roundrobin
      nodes:
        "qwen38.llm.svc.cluster.local:8000": 1
```

A request naming the first model reached its route and answered 200. A
request naming `other-model`, on a route left without `options.model`,
reached vLLM unchanged and got vLLM's 404 ("The model `other-model` does not
exist"). A name that matches no route got APISIX's own 404
(`{"error_msg":"404 Route Not Found"}`). `GET /v1/models` answered 200 with
the served model. Clients must send `Content-Type: application/json`: without
it the `post_arg` routes did not match (404), and `ai-proxy-multi` itself
answers 400 ("only application/json is supported").

## One gateway for many InferenceServices

The lab's daily gateway puts several InferenceServices, each serving its own
model, behind one APISIX, with a path prefix per model: a client sets its
base URL to `http://<gateway>/qwen38/v1` for one model and to another prefix
for the next. Each InferenceService gets one ApisixRoute:

- **The ApisixRoute lives in the backend Service's namespace.** A rule's
  `backends` name a Service in the ApisixRoute's own namespace and cannot
  reach another, so InferenceServices in different namespaces get their
  ApisixRoutes in those namespaces. APISIX and its consumers can stay in a
  namespace of their own; the lab's routes span two namespaces besides
  APISIX's.
- **A chat rule.** `POST <prefix>/v1/chat/completions` goes through
  `ai-proxy-multi`, as in the routes above, for streaming, token metrics and
  a long `timeout`.
- **A passthrough rule.** Every other path under the prefix, such as
  `<prefix>/v1/models`, which `ai-proxy-multi` cannot serve (see "Serving
  more than one model"), goes straight to the Service. `proxy-rewrite`
  strips the prefix, so `/qwen38/v1/models` reaches the runtime as
  `/v1/models`.
- **`priority` picks the chat rule.** A chat request matches both rules, and
  the rule with the higher `priority` wins: 10 for the chat rule, 1 for the
  passthrough rule.

The route for the InferenceService from "The LLMKube side", adapted from the
lab's (one instance, the upstream key from a Secret as in "The upstream key
from a Secret", client keys as in "Client authentication" below):

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixRoute
metadata:
  name: qwen38
  namespace: llm
spec:
  ingressClassName: apisix
  http:
    - name: chat
      priority: 10
      match:
        paths: [/qwen38/v1/chat/completions]
        methods: [POST]
      plugins:
        - name: key-auth
          enable: true
          config: {header: apikey, hide_credentials: true}
        - name: prometheus
          enable: true
        - name: ai-proxy-multi
          enable: true
          config:
            timeout: 600000
            instances:
              - name: qwen38
                provider: openai-compatible
                weight: 1
                auth:
                  header:
                    Authorization: "$env://QWEN38_UPSTREAM_AUTH"
                override:
                  endpoint: "http://qwen38.llm.svc.cluster.local:8000/v1/chat/completions"
    - name: passthrough
      priority: 1
      # Same upstream budget as the chat rule (APISIX default is 60s).
      timeout:
        connect: 10s
        send: 600s
        read: 600s
      match:
        paths: [/qwen38/*]
      backends:
        - serviceName: qwen38
          servicePort: 8000
      plugins:
        - name: key-auth
          enable: true
          config: {header: apikey, hide_credentials: true}
        - name: prometheus
          enable: true
        - name: proxy-rewrite
          enable: true
          config:
            regex_uri: ["^/qwen38/(.*)", "/$1"]
            headers:
              set:
                Authorization: "$env://QWEN38_UPSTREAM_AUTH"
```

Notes:

- The passthrough rule's `timeout` matches the chat rule's 600000 ms. Without
  it, the passthrough rule uses APISIX's default upstream timeout of 60
  seconds. The lab's model routes use the plugin's maximum, 600000; size
  yours as in the `timeout` section.
- The upstream key goes on both rules: in the instance's `auth.header` and
  in `proxy-rewrite`'s `headers.set`. For an InferenceService without a key,
  drop the `headers` block and give the instance `auth: {}` (see "Operating
  notes" below).
- The route has no `options.model`, so the client's `model` passes through
  and has to be the served name (`qwen38-27b` here). Clients can read it
  from `<prefix>/v1/models`. Add `options.model` to the instance to
  overwrite it instead, as in the earlier routes.
- In the lab, `<prefix>/v1/models` answered 200 through the passthrough rule,
  and chat and streamed chat through the chat rule, with a client key.

## Client authentication

The lab's daily gateway puts APISIX's `key-auth` plugin on every rule of
every route, as in the ApisixRoute above:

```yaml
        - name: key-auth
          enable: true
          config: {header: apikey, hide_credentials: true}
```

- **Clients send their key in an `apikey` header, not in `Authorization`.**
  OpenAI-compatible clients put `Authorization: Bearer <their key>` on every
  request. On a route with an upstream key, `ai-proxy-multi` and
  `proxy-rewrite` overwrite `Authorization` with it, and `key-auth`
  compares the header's whole value with a consumer's key, `Bearer `
  included. A header of its own keeps the client key and the upstream key
  apart.
- **`hide_credentials: true`** removes the `apikey` header before the
  request is forwarded, so the InferenceService never sees the client key.
- **Every rule needs it.** Each rule of an ApisixRoute becomes a separate
  APISIX route (the lab's six ApisixRoutes of two rules each became 12), so
  a rule without `key-auth` is open even when the other rule has it.

Create one consumer per client, each with its own Secret, so you can revoke
one key alone and tell clients apart in the metrics:

```bash
kubectl -n llm create secret generic client-key-opencode \
  --from-literal=key="$(openssl rand -hex 24)"
```

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixConsumer
metadata:
  name: opencode
  namespace: llm
spec:
  ingressClassName: apisix
  authParameter:
    keyAuth:
      secretRef:
        name: client-key-opencode
```

Consumers do not have to share the routes' namespace: the lab keeps its
consumers in APISIX's namespace, and they authenticate requests on routes in
two other namespaces. APISIX labels consumers `<namespace>_<name>` in its
metrics (the `consumer` label on `apisix_http_status` and the
`apisix_llm_*` series), not by the bare name, so filter on the prefixed
form. The default access log does not show the consumer at all.

In the lab, a request without a key got 401 from `key-auth` in 11 to 25 ms,
before its body reached the backend, and a run of the verification matrix
with a wrong key failed every check that needs the key. Two clients run
against the gateway, each verified through the consumer label:

- **opencode** (2.0.28): add the key under `options.headers` in the provider
  that points at the gateway. The provider's other fields stay as they are.

  ```json
  {
    "provider": {
      "qwen38": {
        "options": {
          "baseURL": "http://<gateway>/qwen38/v1",
          "headers": {
            "apikey": "<client key>"
          }
        }
      }
    }
  }
  ```

- **LiteLLM** (1.104.2): add `extra_headers` to the model entry. LiteLLM
  resolves `os.environ/` inside `extra_headers`, so the key stays in an
  environment variable:

  ```yaml
  model_list:
    - model_name: qwen38
      litellm_params:
        # model and the entry's other fields stay as they are
        api_base: http://apisix-gateway.llm.svc.cluster.local/qwen38/v1
        extra_headers:
          apikey: os.environ/APISIX_CLIENT_KEY
  ```

  Fill the variable from a copy of the client's Secret in LiteLLM's
  namespace, for example with
  `kubectl -n <litellm-namespace> set env deploy/litellm --from=secret/client-key-litellm --prefix=APISIX_CLIENT_`,
  which turns the Secret's `key` into `APISIX_CLIENT_KEY`.

## Observability

APISIX's metrics need two settings, both in the steps above:
`apisix.prometheus.enabled=true` in the Helm install, which serves them on
port 9091 at `/apisix/prometheus/metrics` behind the
`apisix-prometheus-metrics` Service, and `prometheus: {}` in each route's
plugins. With the Prometheus Operator, the chart's
`metrics.serviceMonitor.enabled` value creates a ServiceMonitor. The
[prometheus plugin documentation](https://apisix.apache.org/docs/apisix/plugins/prometheus/)
lists every metric. The ones for model traffic:

| Metric | Type | Meaning |
|---|---|---|
| `apisix_llm_latency` | histogram | Milliseconds. Label `type`: `ttft` (streamed requests: first chunk from the backend, measured from the start of the request) or `total` |
| `apisix_llm_prompt_tokens` | counter | Prompt tokens the backend reported |
| `apisix_llm_completion_tokens` | counter | Completion tokens the backend reported |

They are labeled with `route_id`, `request_type` (`ai_chat` or `ai_stream`),
`node` (the instance name), `request_llm_model` (the name the client sent)
and `llm_model` (the name forwarded, from `options.model`). The counters
have no `_total` suffix. In the e2e suite,
`apisix_llm_completion_tokens{route_id="fast",request_type="ai_stream"}`
counted the tokens of a streamed request; for the route above, `route_id` is
`qwen38`. In the lab run, a streamed 400-token answer from vLLM added 400 to
that counter. APISIX 3.19.0 also exports `apisix_llm_active_connections` and
per-request histograms `apisix_llm_prompt_tokens_dist` and
`apisix_llm_completion_tokens_dist`.

Token counts on streams depend on the backend reporting usage.
`ai-proxy-multi` sets `stream_options.include_usage: true` on every streamed
chat request, so an OpenAI-compatible backend sends usage in its final chunk.
The e2e suite verifies that the field reaches the backend and that the counts
reach the metrics. With vLLM in the lab run, a streamed request that did not
ask for usage still ended with vLLM's usage chunk, which the client receives
too. The
[ai-rate-limiting plugin](https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/)
limits on the same reported usage, counted after each response: the request
that crosses a limit completes, and the next one is rejected. A backend that
reports no usage counts as zero tokens.

Read the gateway's numbers alongside the runtime's own. LLMKube's inference
PodMonitor (chart value `prometheus.inferencePodMonitor.enabled`) scrapes
`vllm:time_to_first_token_seconds` or `sglang:time_to_first_token_seconds`,
and its PrometheusRule (`prometheus.prometheusRule.enabled`) records
`llmkube:inference:ttft_seconds:p95_5m` and `llmkube:inference:ttft_seconds:p50_5m`
from them. APISIX's `apisix_llm_latency{type="ttft"}` measures the same wait
from the gateway, so it also includes the time spent in APISIX and on the
network.

## Gateway overhead, measured

The lab run measured what APISIX adds to time to first token. Setup: vLLM
serving Qwen3-0.6B on one NVIDIA GB10 (`--gpu-memory-utilization=0.25`),
APISIX 3.19.0 in standalone mode on another node (4 nginx workers, 4-CPU
limit), and the client in the cluster on the APISIX node. Every request was
streamed and used a fresh prompt (a unique prefix, so vLLM's prefix cache
could not reuse it), and TTFT was measured at the client up to the first
chunk with content. Direct requests went to the InferenceService's Service,
the others through APISIX's gateway Service. Medians of 5 runs each:

| Prompt | Direct | Through APISIX | Difference |
|---|---|---|---|
| About 1K tokens | 33.6 ms | 38.6 ms | +5.0 ms |
| About 16K tokens | 738.4 ms | 750.7 ms | +12.3 ms |

A repeat with 20 runs each gave +3.6 ms and +6.3 ms.

A 30-minute soak through APISIX at 32 concurrent streams (prompts of about
500 to 1,100 tokens, answers of 128 to 384 tokens) completed 9,612 requests
with no errors and no truncated streams. TTFT at the client was 90 ms at the
median and 117 ms at p95; APISIX's own `apisix_llm_latency{type="ttft"}`
averaged 82.8 ms and vLLM's `vllm:time_to_first_token_seconds` 77.4 ms over
the soak. APISIX used 0.07 of a CPU core on average (0.08 at the
busiest 30 seconds) and under 190 MiB of memory, and its two instances each
took 4,806 requests. These figures come from a small model on one GPU, so
measure with your own model, prompt sizes and concurrency before relying on
them.

## Operating notes

From running the ApisixRoute setup as the lab's daily gateway, on APISIX
3.19.0 and ingress controller 2.2.0:

- **Every `ai-proxy-multi` instance needs an `auth` block.** APISIX 3.19.0's
  Admin API answers 400 to a route with an instance that has none. For an
  InferenceService without a key, write `auth: {}`.
- **A route whose Service has no endpoints fails cleanly.** With the
  InferenceService scaled to zero, the chat rule answers 500 and the
  passthrough rule 503; neither hangs.
- **The APISIX image has no `curl`**, so `kubectl exec` cannot call the
  Admin API from inside the pod. Reach it with
  `kubectl -n llm port-forward svc/apisix-admin 9180:9180`, as in "The same
  route through the Admin API".
- **An etcd restart without persistence leaves the controller unable to
  sync.** In traditional mode with the chart's etcd persistence off
  (`etcd.persistence.enabled: false`), a `helm upgrade` of the APISIX
  release that changed only the gateway's NodePort also restarted etcd, and
  etcd came back empty. APISIX kept serving its
  last configuration from memory, but the ingress controller's sync failed on
  every cycle, so no ApisixRoute or ApisixConsumer change reached APISIX. It
  recovered when the APISIX Deployment was restarted: APISIX initializes
  etcd when it starts, and the controller's next sync succeeded. Turn on etcd
  persistence, or restart the APISIX Deployment after any etcd restart.

## Requirements and gaps

APISIX:

- **APISIX 3.19.0 or later.** It is the version the e2e suite runs, and its
  [release](https://github.com/apache/apisix/releases/tag/3.19.0) fixed
  several `ai-proxy` streaming bugs, including retries and 5xx responses after
  part of a stream had already been sent.
- **Requests that are not streamed cannot run longer than 10 minutes**, the
  maximum `timeout`.
- **Balancing and health checks are per instance.** An instance is one URL,
  here one Service, and Kubernetes picks the pod behind it, so there is no
  prompt-cache affinity across an InferenceService's replicas.
- **No Gateway API Inference Extension.** APISIX has no InferencePool backend
  or endpoint picker; the ingress controller's tracking issue,
  [apache/apisix-ingress-controller#2498](https://github.com/apache/apisix-ingress-controller/issues/2498),
  was closed without an implementation.
- **Open issue
  [apache/apisix#13256](https://github.com/apache/apisix/issues/13256):** the
  `ai-proxy` streaming loop can starve a worker's CPU when the backend sends in
  bursts. The lab soak (see "Gateway overhead, measured") did not show it
  with vLLM streaming token by token, but soak-test streaming on your own
  hardware and backends before production.
- **The plugin's `secretRef` cannot set the upstream key.** With the APISIX
  Ingress Controller 2.2.0, it cannot set a value inside `ai-proxy-multi`'s
  `instances` list. Keep the key in a Secret through an `$env://` variable
  instead (see "The upstream key from a Secret").

LLMKube:

- `spec.endpoint.apiKeySecretRef` and `spec.endpoint.externalTrafficPolicy`
  were added after LLMKube 0.10.3.
- `apiKeySecretRef` is supported for runtime `vllm` and `sglang` only;
  admission rejects it on other runtimes.
- SGLang with `spec.command` set and a key is rejected, because LLMKube cannot
  install its config-file launcher around a custom command. Drop
  `spec.command`, or remove `apiKeySecretRef` and configure authentication in
  your custom command. Passing the key as `--api-key $(VAR)` in `spec.args`
  exposes it in the OS process arguments as well as the startup log.
- `apiKeySecretRef` cannot be combined with `spec.endpoint.gateway.enabled`;
  admission rejects it. The Envoy AI Gateway resources LLMKube generates do
  not forward the key, so every request through the gateway would get 401.
- **LoRAAdapter refuses keyed backends.** SGLang guards its adapter load and
  unload endpoints with the key, and LLMKube does not send it. A LoRAAdapter
  that targets a keyed InferenceService reports `Loaded=False` with reason
  `APIKeyUnsupported` and never calls SGLang. Load adapters at startup instead
  (SGLang `--lora-paths` in `spec.extraArgs`), or remove the key.
- **ModelRouter does not forward the key yet.** A
  [ModelRouter](../concepts/model-router) with a backend whose InferenceService
  requires an API key reports reason `BackendAPIKeyUnsupported`, naming the
  backends, on `BackendsReady` (Proxy data plane), `GatewayReady` (Gateway) or
  `AgentGatewayReady` (AgentGateway). An InferenceService requires a key when
  it sets `apiKeySecretRef`, passes `--api-key` in `spec.extraArgs`, passes
  `--api-key` in `spec.args` with `spec.command` set, or sets `VLLM_API_KEY`
  in `spec.env` (vLLM reads the variable directly). A backend whose own
  InferenceService is open but whose ModelPool has a keyed member is reported
  the same way on `BackendsReady` (pool siblings are reachable only through
  the Proxy data plane). Route to keyed InferenceServices from APISIX
  directly.
- On an InferenceService relayed to a metal agent, `externalTrafficPolicy`
  applies to the relay's Service: a NetworkPolicy `ipBlock` on the relay pod
  sees APISIX's address. The engine on the host connects from the agent's
  loopback ingress and gets APISIX's address as the rightmost
  `X-Forwarded-For` entry.
