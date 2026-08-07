{{/*
Expand the name of the chart.
*/}}
{{- define "llmkube.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "llmkube.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "llmkube.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "llmkube.labels" -}}
helm.sh/chart: {{ include "llmkube.chart" . }}
{{ include "llmkube.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "llmkube.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llmkube.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "llmkube.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-controller-manager" (include "llmkube.fullname" .)) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the controller manager image
Supports registry prefix, tag, and digest. Digest takes precedence when set.
*/}}
{{- define "llmkube.controllerImage" -}}
{{- $repo := .Values.controllerManager.image.repository -}}
{{- if .Values.controllerManager.image.registry -}}
{{- $repo = printf "%s/%s" .Values.controllerManager.image.registry .Values.controllerManager.image.repository -}}
{{- end -}}
{{- if .Values.controllerManager.image.digest -}}
{{- printf "%s@%s" $repo .Values.controllerManager.image.digest -}}
{{- else -}}
{{- $tag := .Values.controllerManager.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Create the init container image
Supports optional registry prefix.
*/}}
{{- define "llmkube.initContainerImage" -}}
{{- $repo := .Values.controllerManager.initContainer.repository -}}
{{- if .Values.controllerManager.initContainer.registry -}}
{{- $repo = printf "%s/%s" .Values.controllerManager.initContainer.registry .Values.controllerManager.initContainer.repository -}}
{{- end -}}
{{- printf "%s:%s" $repo .Values.controllerManager.initContainer.tag -}}
{{- end }}

{{/*
Create the router-proxy image used as the default for ModelRouter-managed
proxy pods. Per-ModelRouter spec.proxy.image overrides this. Same digest /
tag resolution rules as the controller image.
*/}}
{{- define "llmkube.routerProxyImage" -}}
{{- $repo := .Values.controllerManager.routerProxy.repository -}}
{{- if .Values.controllerManager.routerProxy.registry -}}
{{- $repo = printf "%s/%s" .Values.controllerManager.routerProxy.registry .Values.controllerManager.routerProxy.repository -}}
{{- end -}}
{{- if .Values.controllerManager.routerProxy.digest -}}
{{- printf "%s@%s" $repo .Values.controllerManager.routerProxy.digest -}}
{{- else -}}
{{- $tag := .Values.controllerManager.routerProxy.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Create the namespace
*/}}
{{- define "llmkube.namespace" -}}
{{- default .Values.namespace .Release.Namespace }}
{{- end }}

{{/*
Prometheus ServiceMonitor namespace
*/}}
{{- define "llmkube.prometheus.serviceMonitor.namespace" -}}
{{- if .Values.prometheus.serviceMonitor.namespace }}
{{- .Values.prometheus.serviceMonitor.namespace }}
{{- else }}
{{- include "llmkube.namespace" . }}
{{- end }}
{{- end }}

{{/*
Prometheus PrometheusRule namespace
*/}}
{{- define "llmkube.prometheus.prometheusRule.namespace" -}}
{{- default "monitoring" .Values.prometheus.prometheusRule.namespace }}
{{- end }}

{{/*
GPU alert metric contract (#1356). One OR-fallback expression per role, so the
exporter a cluster actually runs is the one that evaluates and absent families
contribute nothing. Adding an exporter is one branch, not another rule body.

temp pins the edge sensor: drm reports sensor=edge/junction/mem and node_hwmon
the same split as temp1/temp2/temp3, measured 69/82/79 C on one GPU.
node_hwmon is scoped through node_hwmon_chip_names{chip_name="amdgpu"} -
unscoped it is every sensor on the node, and fired at 89 C off a chipset.
The * on(chip, instance) group_left() join multiplies by that series' value
to filter: the conventional idiom, correct only while the value is always 1.
If an exporter ever emits something else there, switch to a bool match.
mem pins pool="vram" on both sides; the denominator needs the numerator's
selector, not implicit label matching.
*/}}
{{- define "llmkube.gpuMetricNames" -}}
util: DCGM_FI_DEV_GPU_UTIL or amdgpu_gpu_busy_percent or drm_engine_utilization_ratio{engine="gpu"} * 100
temp: DCGM_FI_DEV_GPU_TEMP or node_hwmon_temp_celsius{sensor="temp1"} * on(chip, instance) group_left() node_hwmon_chip_names{chip_name="amdgpu"} or drm_temperature_celsius{sensor="edge"}
mem: (DCGM_FI_DEV_FB_USED / DCGM_FI_DEV_FB_TOTAL) * 100 or (amdgpu_vram_used_bytes / amdgpu_vram_total_bytes) * 100 or (drm_memory_used_bytes{pool="vram"} / drm_memory_total_bytes{pool="vram"}) * 100
power: DCGM_FI_DEV_POWER_USAGE or node_hwmon_power_watt * on(chip, instance) group_left() node_hwmon_chip_names{chip_name="amdgpu"} or drm_power_watts
{{- end }}

{{/*
Webhook Service name. The validating webhook's clientConfig targets this
Service; the controller-manager pod labels are the Service selector.
*/}}
{{- define "llmkube.webhook.serviceName" -}}
{{- printf "%s-webhook" (include "llmkube.fullname" .) -}}
{{- end }}

{{/*
Webhook serving-cert Secret name. Holds tls.crt + tls.key (+ ca.crt for
reference). Mounted into the controller pod at the controller-runtime default
cert dir and reused across upgrades via lookup.
*/}}
{{- define "llmkube.webhook.secretName" -}}
{{- printf "%s-webhook-cert" (include "llmkube.fullname" .) -}}
{{- end }}

{{/*
ValidatingWebhookConfiguration name.
*/}}
{{- define "llmkube.webhook.configName" -}}
{{- printf "%s-validating-webhook" (include "llmkube.fullname" .) -}}
{{- end }}

{{/*
llmkube.webhook.certs resolves the serving cert + CA bundle for the webhook,
reusing the existing Secret's material when present so the cert and the injected
caBundle stay STABLE across `helm upgrade`. Returns a dict with keys "ca",
"cert", "key" (all base64-encoded PEM).

Lookup-reuse: if the serving Secret already exists AND carries tls.crt /
tls.key / ca.crt, reuse them verbatim. Otherwise generate a fresh self-signed
CA + serving cert whose SANs cover the in-cluster Service DNS names. The
caBundle in the ValidatingWebhookConfiguration is injected from the SAME dict,
so they always match.

Note: `lookup` returns an empty dict during `helm template` / dry-run, so those
always render freshly-generated material (fine: template output is not
applied). On a real install/upgrade against an API server the existing Secret
is found and reused.
*/}}
{{- define "llmkube.webhook.certs" -}}
{{- $svc := include "llmkube.webhook.serviceName" . -}}
{{- $ns := include "llmkube.namespace" . -}}
{{- $altNames := list (printf "%s.%s.svc" $svc $ns) (printf "%s.%s.svc.cluster.local" $svc $ns) -}}
{{- $secretName := include "llmkube.webhook.secretName" . -}}
{{- $existing := lookup "v1" "Secret" $ns $secretName -}}
{{- if and $existing $existing.data (index $existing.data "tls.crt") (index $existing.data "tls.key") (index $existing.data "ca.crt") -}}
{{- dict "ca" (index $existing.data "ca.crt") "cert" (index $existing.data "tls.crt") "key" (index $existing.data "tls.key") | toYaml -}}
{{- else -}}
{{- $ca := genCA (printf "%s-webhook-ca" (include "llmkube.fullname" .)) (int .Values.webhook.certValidityDays) -}}
{{- $cert := genSignedCert $svc nil $altNames (int .Values.webhook.certValidityDays) $ca -}}
{{- dict "ca" ($ca.Cert | b64enc) "cert" ($cert.Cert | b64enc) "key" ($cert.Key | b64enc) | toYaml -}}
{{- end -}}
{{- end }}
