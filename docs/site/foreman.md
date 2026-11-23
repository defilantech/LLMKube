---
title: "Foreman moved to Meitheal"
description: "Foreman, the agentic coding harness that shipped beside LLMKube, now lives in its own repository as Meitheal. The old and new names, and how to move a running install."
---

# Foreman moved to Meitheal

Foreman, the coder, verifier and reviewer harness that used to ship beside
LLMKube, now lives in its own repository under a new name, **Meitheal**:
[github.com/defilantech/meitheal](https://github.com/defilantech/meitheal).
Its documentation is in that repository's
[docs directory](https://github.com/defilantech/meitheal/tree/main/docs), and
its issues are tracked there. LLMKube is the inference operator only.

Nothing changes for LLMKube itself. `Model`, `InferenceService` and the other
`inference.llmkube.dev` resources work as before, and Meitheal reads
InferenceServices as an LLMKube client, so an Agent can keep using the
InferenceService it uses today.

Published Foreman artifacts stay where they are. Every tag of the
`ghcr.io/defilantech/llmkube-foreman-*` images, every `foreman` chart release
(at `oci://ghcr.io/defilantech/charts/foreman` and in the chart repository
index), and every GitHub release and tag remain published, so an existing
install keeps running on its current images. The last Foreman images and chart
are the ones from the last LLMKube release before the move. New releases come
from the Meitheal repository.

Meitheal's first release, 0.1.0, is still pending: until it ships, stay on
the last Foreman release LLMKube published.

## Old names and new names

| Foreman | Meitheal |
| --- | --- |
| API group `foreman.llmkube.dev` | `meitheal.dev` |
| Label and annotation prefix `foreman.llmkube.dev/` | `meitheal.dev/` |
| Image `ghcr.io/defilantech/llmkube-foreman-operator` | `ghcr.io/defilantech/meitheal-operator` |
| Image `ghcr.io/defilantech/llmkube-foreman-agent` | `ghcr.io/defilantech/meitheal-agent` |
| Image `ghcr.io/defilantech/llmkube-foreman-agent-coder` | `ghcr.io/defilantech/meitheal-agent-coder` |
| Chart `oci://ghcr.io/defilantech/charts/foreman` | `oci://ghcr.io/defilantech/charts/meitheal`, with versions starting at 0.1.0 |
| CLI `llmkube foreman dispatch`, `slice`, `run` and `decisions` | `meitheal dispatch`, `slice`, `run` and `decisions` |
| CLI `llmkube audit export` | `meitheal audit export` |
| Metrics `foreman_*` | `meitheal_*` |
| macOS signing identifier `dev.llmkube.foreman-agent` | `dev.meitheal.agent` |
| macOS launchd label `com.llmkube.foreman-agent` | `dev.meitheal.agent` |

The renamed metrics are `foreman_task_completed_total`,
`foreman_task_duration_seconds`, `foreman_task_turns`,
`foreman_workload_completed_total` and `foreman_archive_failures_total`, each
now with the `meitheal_` prefix. The chart's Grafana dashboard and
PrometheusRule moved with them; dashboards or alerts outside the chart that
query the old names need the new ones.

## Moving a running install

The new API group means new CRDs, so Meitheal installs beside an existing
Foreman release instead of upgrading it. The `foreman` chart templates its six
CRDs without `helm.sh/resource-policy: keep`, so `helm uninstall` of that
release deletes the CRDs and every Agent, AgenticTask, AgentRelease, FleetNode,
ModelProfile and Workload with them. The full guide, with every command and
the edge cases, is in Meitheal's docs:
[Migrating from Foreman](https://github.com/defilantech/meitheal/blob/main/docs/migrating-from-foreman.md).
In short, find your release first, since the commands below assume the
release `foreman` in namespace `foreman-system`:

```sh
helm list -A | grep foreman
```

The steps below need foreman chart 0.9.1 or later, the first version that
templates its CRDs behind `crds.install`. On an older chart, setting
`crds.install=false` changes nothing, so upgrade the release to a current
foreman chart (the last one LLMKube published) the way you normally upgrade it, then
start here. Move in this order:

1. Annotate the six `foreman.llmkube.dev` CRDs with
   `helm.sh/resource-policy: keep`, then take them out of the release.
   `helm uninstall` reads that annotation from the release's stored manifest,
   not from the live CRD, so the annotation alone does not keep them.
   `helm upgrade` does read it from the live CRD, so an upgrade that stops
   templating the CRDs leaves them in place. Use your release name, namespace
   and installed chart version:

   ```sh
   for crd in agents agentictasks agentreleases fleetnodes modelprofiles workloads; do
     kubectl annotate crd "$crd.foreman.llmkube.dev" helm.sh/resource-policy=keep
   done
   # FOREMAN_CHART_VERSION is the chart version helm list shows for the release.
   helm upgrade foreman oci://ghcr.io/defilantech/charts/foreman \
     --namespace foreman-system --version "$FOREMAN_CHART_VERSION" \
     --reuse-values --set crds.install=false
   ```

   Check both of these before going on. The release must no longer hold the
   CRDs, and the CRDs must still exist:

   ```sh
   helm get manifest foreman --namespace foreman-system | grep -c 'kind: CustomResourceDefinition' || true   # expect 0
   kubectl get crd | grep -c foreman.llmkube.dev                                                    # expect 6
   ```

   If the first command prints anything but 0, stop: the chart did not drop the
   CRDs, and uninstalling the release would delete them.

2. Install the `meitheal` chart as a new release, following the
   [Meitheal README](https://github.com/defilantech/meitheal#install).
3. Export the durable resources (Agent, AgentRelease, FleetNode,
   ModelProfile), change their `apiVersion` to `meitheal.dev/v1alpha1` and any
   `foreman.llmkube.dev/` label or annotation keys to `meitheal.dev/`, and
   apply them.
4. Let in-flight AgenticTasks and Workloads finish.
5. Uninstall the `foreman` release, then delete the old CRDs:

   ```sh
   helm uninstall foreman --namespace foreman-system
   for crd in agents agentictasks agentreleases fleetnodes modelprofiles workloads; do
     kubectl delete crd "$crd.foreman.llmkube.dev"
   done
   ```

## The macOS agent

The new signing identifier and launchd label mean a native Mac agent has to be
reinstalled. Stop the Foreman agent first, either with
`make uninstall-foreman-agent` from a checkout of an LLMKube release that still
ships Foreman, or by hand:

```sh
launchctl bootout "gui/$(id -u)/com.llmkube.foreman-agent"
rm -f ~/Library/LaunchAgents/com.llmkube.foreman-agent.plist
```

Then install the Meitheal agent with `make install-agent` from a Meitheal
checkout, as its README describes.
