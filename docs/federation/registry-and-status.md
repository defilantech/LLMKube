# Federation Registry and Status Rollup

LLMKube federation lets a datacenter cluster track fleet-wide inference status and capacity from remote edge sites without granting those sites more than minimal, per-site scoped credentials. This guide covers how to register an edge site, configure the edge operator, and read the fleet-wide status.

## Architecture Overview: Spoke Model

Federation uses a spoke model where edge sites initiate outbound connections to the datacenter:

- **Edge initiates**: Edge operators push heartbeats and inference status to the datacenter cluster, never accepting inbound connections.
- **Outbound only**: All communication flows from edge to datacenter. No datacenter-to-edge control plane traffic.
- **NAT and firewall friendly**: Works across network boundaries since the edge cluster always dials outbound.

The datacenter cluster reconciles each edge site's health status (Connected, Stale, Unreachable) based on heartbeat staleness. Edge sites carry a narrowly scoped credential that can ONLY update their own FederatedCluster's status subresource.

## Registering an Edge Site

Registration happens on the datacenter cluster using the `llmkube fleet register` command. This creates:

1. A FederatedCluster object on the datacenter (identity for the site)
2. A least-privilege ServiceAccount, ClusterRole, and ClusterRoleBinding on the datacenter (scoped to the site's own FederatedCluster status subresource)
3. A long-lived bearer token for that ServiceAccount
4. A kubeconfig snippet and operator flags for the site admin to install on the edge

### Register a Site

Run this on the datacenter cluster:

```bash
llmkube fleet register \
  --name acme-floor-3 \
  --residency floor-3 \
  --heartbeat-interval 30 \
  --datacenter-endpoint https://datacenter.example.com:6443
```

Flag meanings:

- `--name` (required): Unique identifier for this edge site's FederatedCluster object. Used as the token's resource scope.
- `--residency`: Free-form data residency tier label (e.g., "eu", "us-west", "floor-3"). Recorded now; enforced later by the federation router (issue #1237).
- `--heartbeat-interval`: Expected edge heartbeat interval in seconds. Staleness thresholds are derived from this (3x=Stale, 10x=Unreachable). Defaults to 30 seconds.
- `--datacenter-endpoint`: Datacenter API server URL to embed in the edge kubeconfig. If omitted, falls back to the current kubeconfig context's server.
- `--namespace`: Kubernetes namespace on the datacenter to create the per-site ServiceAccount. Defaults to llmkube-system.

The command outputs a kubeconfig snippet and operator flags:

```
# Edge site "acme-floor-3" registered on the datacenter as FederatedCluster/acme-floor-3.
# Save the block below as a kubeconfig file on the edge site (for example,
# federation-datacenter.kubeconfig), then start the edge operator with the
# flags shown after it.

apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://datacenter.example.com:6443
    certificate-authority-data: <base64-encoded-ca>
  name: datacenter
contexts:
- context:
    cluster: datacenter
    user: fedcluster-acme-floor-3
  name: datacenter
current-context: datacenter
users:
- name: fedcluster-acme-floor-3
  user:
    token: <long-lived-bearer-token>

# ServiceAccount:  llmkube-system/fedcluster-acme-floor-3
# On the edge site's operator, set:
#   --federation-role=edge --federation-cluster-name=acme-floor-3 --federation-datacenter-kubeconfig=<path-to-saved-file>
```

### Configure the Edge Operator

On the edge site's cluster, start the operator with:

```bash
llmkube-controller-manager \
  --federation-role=edge \
  --federation-cluster-name=acme-floor-3 \
  --federation-datacenter-kubeconfig=/path/to/federation-datacenter.kubeconfig \
  # ... other flags unchanged
```

Operator flags:

- `--federation-role=edge`: Switch from hub (datacenter, default) to edge mode. In edge mode, the operator runs only the FederationEdgeReconciler and skips the FederatedClusterReconciler.
- `--federation-cluster-name`: Name of this edge site's FederatedCluster object on the datacenter (matches the registration --name).
- `--federation-datacenter-kubeconfig`: Path to the kubeconfig file that was printed by `llmkube fleet register`. This file contains the narrowly scoped token and datacenter API endpoint.

In edge mode, the operator reads local Models, InferenceServices, and Nodes, computes the fleet's inference health and capacity, and pushes updates to the datacenter every heartbeat interval.

## Reading Fleet Status

Once edge sites are registered and running, view fleet-wide status on the datacenter cluster:

```bash
llmkube fleet status
```

Example output:

```
NAME               TIER      PHASE       LAST HEARTBEAT  GPUS      SERVICES
acme-floor-3       floor-3   Connected   2 minutes ago   6/8       3/0/5
prod-us-west       us-west   Connected   1 minute ago    16/16     10/0/15
backup-eu          eu        Stale       8 minutes ago   -/-       -/-/-

Fleet-wide: 3 site(s) (2 Connected, 1 Stale, 0 Unreachable)
GPUs: 22/24 allocatable/total
Services: 13/0/20 ready/failed/total
```

Column meanings:

- **NAME**: FederatedCluster name (site identity).
- **TIER**: DataResidencyTier label (or "-" if unset).
- **PHASE**: Connection status:
  - Connected: heartbeat received within the interval.
  - Stale: no heartbeat for 3x the interval.
  - Unreachable: no heartbeat for 10x the interval.
- **LAST HEARTBEAT**: How long ago the last status push was received.
- **GPUS**: Allocatable/total GPU count on the site.
- **SERVICES**: Ready/failed/total inference service count.

The footer aggregates across all sites:

- Count of sites in each phase.
- Total GPU allocatable/total capacity.
- Total inference service counts (ready/failed/total).

## Least-Privilege Token Scope

The bearer token minted for each edge site is scoped to the following permissions (enforced by a ClusterRole with resourceNames):

- **Get** the FederatedCluster object by its own name (no list, no watch, no delete).
- **Get**, **update**, **patch** the FederatedCluster's status subresource by name (no other verbs, no other resources).

This means an edge site cannot:

- Read or modify any other site's FederatedCluster.
- Read or write any other Kubernetes resource on the datacenter.
- List, watch, or delete its own FederatedCluster (only read and update status).

This least-privilege scope is enforced by Kubernetes RBAC at the datacenter cluster, independent of the edge operator's behavior.

## Data Residency: Status-Only Slice

This federation slice (status rollup and registry) is a status-only system. The dataResidencyTier field is recorded in the FederatedCluster spec but NOT enforced by the federation components:

- **Recorded**: Every FederatedCluster stores its tier label for audit and inventory.
- **Not enforced**: Requests are not routed or filtered by tier yet.
- **Future enforcement**: The federation router (issue #1237) will enforce tier constraints in a later phase, selecting edge sites based on residency requirements in request routes.

If you are planning a deployment that requires data residency constraints, set the tier now so the router has the information when it is implemented later.

## Token Rotation and Renewal

The tokens minted by `llmkube fleet register` are long-lived (10-year expiration) bearer credentials embedded in the edge kubeconfig. Token rotation is out of scope for this slice. In a future phase, automation may be added to rotate tokens while keeping the same identity and permissions scoped to the site.

## Next Steps

- Review the sample [FederatedCluster manifest](../../config/samples/federation_v1alpha1_federatedcluster.yaml).
- Check the [federation CRD API reference](../api/federation.md).
- See [federation architecture](./architecture.md) for detailed design rationale.
