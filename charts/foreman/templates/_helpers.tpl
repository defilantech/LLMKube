{{/*
Expand the name of the chart.
*/}}
{{- define "foreman.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "foreman.fullname" -}}
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
Chart label, used by app.kubernetes.io/version etc.
*/}}
{{- define "foreman.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resolved namespace. Defaults to .Values.namespace; falls back to Release.Namespace.
*/}}
{{- define "foreman.namespace" -}}
{{- default .Release.Namespace .Values.namespace }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "foreman.labels" -}}
helm.sh/chart: {{ include "foreman.chart" . }}
{{ include "foreman.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (shared across operator + agent so a single Service or
NetworkPolicy can target either). Components stamp an additional
app.kubernetes.io/component label downstream.
*/}}
{{- define "foreman.selectorLabels" -}}
app.kubernetes.io/name: {{ include "foreman.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: foreman
{{- end }}

{{/*
Operator pod labels = base selector + component=operator.
*/}}
{{- define "foreman.operator.labels" -}}
{{ include "foreman.selectorLabels" . }}
app.kubernetes.io/component: operator
{{- end }}

{{/*
Agent pod labels = base selector + component=agent.
*/}}
{{- define "foreman.agent.labels" -}}
{{ include "foreman.selectorLabels" . }}
app.kubernetes.io/component: agent
{{- end }}

{{/*
ServiceAccount name for the foreman-operator.
*/}}
{{- define "foreman.operator.serviceAccountName" -}}
{{- if .Values.operator.serviceAccount.create }}
{{- default (printf "%s-operator" (include "foreman.fullname" .)) .Values.operator.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.operator.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name for the foreman-agent. Expects a context dict with
"agentName" (the entry from .Values.agents, or the legacy sentinel
"_implicit_" when .Values.agent: is used unchanged) and "agentConfig"
(the per-agent values dict). When the sentinel is passed, the legacy
"<fullname>-agent" name is preserved so an existing install's SA is not
renamed on upgrade; an explicit agents.<name> entry always suffixes the
agent key so multiple agents never collide.
*/}}
{{- define "foreman.agent.serviceAccountName" -}}
{{- if .agentConfig.serviceAccount.create }}
{{- if eq .agentName "_implicit_" }}
{{- default (printf "%s-agent" (include "foreman.fullname" .)) .agentConfig.serviceAccount.name }}
{{- else }}
{{- default (printf "%s-%s-agent" (include "foreman.fullname" .) .agentName | trunc 63 | trimSuffix "-") .agentConfig.serviceAccount.name }}
{{- end }}
{{- else }}
{{- default "default" .agentConfig.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Default resource name for one foreman-agent (Deployment / ClusterRole /
ClusterRoleBinding). Same shape as the SA default — "<fullname>-agent"
for the legacy sentinel and "<fullname>-<agentName>-agent" for an
explicit agents.<name> entry — but with no per-agent override since the
SA already carries that escape hatch.
*/}}
{{- define "foreman.agent.resourceName" -}}
{{- if eq .agentName "_implicit_" }}
{{- printf "%s-agent" (include "foreman.fullname" .) }}
{{- else }}
{{- printf "%s-%s-agent" (include "foreman.fullname" .) .agentName | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
The agents map to render. When the user opts into the multi-fleet form
(.Values.agents is set) the map is returned verbatim. Otherwise the
legacy top-level .Values.agent: block is wrapped under a sentinel key
("_implicit_") so a single template path can render both shapes and the
legacy install's resource names are preserved on upgrade (#994). Callers
must not pass "_implicit_" as an explicit agents.* key.
*/}}
{{- define "foreman.agents" -}}
{{- if .Values.agents }}
{{- .Values.agents | toYaml }}
{{- else }}
{{- dict "_implicit_" .Values.agent | toYaml }}
{{- end }}
{{- end }}

{{/*
Coder Job pod labels = base selector + component=coder.
*/}}
{{- define "foreman.coder.labels" -}}
{{ include "foreman.selectorLabels" . }}
app.kubernetes.io/component: coder
{{- end }}

{{/*
ServiceAccount name for the coder Job pods. Defaults to "foreman-coder"
so the sample Agent's spec.execution.serviceAccountName lines up out of
the box; override via coder.serviceAccount.name.
*/}}
{{- define "foreman.coder.serviceAccountName" -}}
{{- if .Values.coder.serviceAccount.create }}
{{- default "foreman-coder" .Values.coder.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.coder.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Foreman operator image (registry/repo:tag or registry/repo@digest).
*/}}
{{- define "foreman.operator.image" -}}
{{- $img := .Values.operator.image -}}
{{- $repo := $img.repository -}}
{{- if $img.registry -}}
{{- $repo = printf "%s/%s" $img.registry $img.repository -}}
{{- end -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest -}}
{{- else -}}
{{- $tag := $img.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Foreman agent image (registry/repo:tag or registry/repo@digest). Expects
the per-agent context dict with "agentConfig" (.Values.agents.<name> or
.Values.agent) and the chart context (Release/Values/Chart) at the root.
*/}}
{{- define "foreman.agent.image" -}}
{{- $img := .agentConfig.image -}}
{{- $repo := $img.repository -}}
{{- if $img.registry -}}
{{- $repo = printf "%s/%s" $img.registry $img.repository -}}
{{- end -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest -}}
{{- else -}}
{{- $tag := $img.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Gate cache PVC name for a single agent. Expects the per-agent context
dict; the legacy install (sentinel "_implicit_") keeps "<fullname>-gate-cache"
so an upgrade does not rename an in-use PVC. Explicit agents.<name>
entries get "<fullname>-<agentName>-gate-cache" so multiple agents each
own a distinct PVC. Users can still override via .agentConfig.gateCache.name.
*/}}
{{- define "foreman.gateCache.pvcName" -}}
{{- $suffix := "" -}}
{{- if ne .agentName "_implicit_" -}}
{{- $suffix = printf "-%s" .agentName -}}
{{- end -}}
{{- default (printf "%s%s-gate-cache" (include "foreman.fullname" .) $suffix) .agentConfig.gateCache.name }}
{{- end }}

{{/*
Webhook Service name. The validating webhook's clientConfig targets this
Service; the operator Deployment's pod labels are the Service selector.
*/}}
{{- define "foreman.webhook.serviceName" -}}
{{- printf "%s-webhook" (include "foreman.fullname" .) -}}
{{- end }}

{{/*
Webhook serving-cert Secret name. Holds tls.crt + tls.key (+ ca.crt for
reference). Mounted into the operator pod at the controller-runtime
default cert dir and reused across upgrades via lookup.
*/}}
{{- define "foreman.webhook.secretName" -}}
{{- printf "%s-webhook-cert" (include "foreman.fullname" .) -}}
{{- end }}

{{/*
ValidatingWebhookConfiguration name.
*/}}
{{- define "foreman.webhook.configName" -}}
{{- printf "%s-validating-webhook" (include "foreman.fullname" .) -}}
{{- end }}

{{/*
foreman.webhook.certs resolves the serving cert + CA bundle for the
webhook, reusing the existing Secret's material when present so the cert
and the injected caBundle stay STABLE across `helm upgrade`. Returns a
dict with keys "ca", "cert", "key" (all base64-encoded PEM).

Lookup-reuse: if the serving Secret already exists AND carries tls.crt /
tls.key / ca.crt, reuse them verbatim. Otherwise generate a fresh
self-signed CA + serving cert whose SANs cover the in-cluster Service DNS
names. The caBundle in the ValidatingWebhookConfiguration is injected from
the SAME dict, so they always match.

Note: `lookup` returns an empty dict during `helm template` / dry-run, so
those always render freshly-generated material (fine: template output is
not applied). On a real install/upgrade against an API server the existing
Secret is found and reused.
*/}}
{{- define "foreman.webhook.certs" -}}
{{- $svc := include "foreman.webhook.serviceName" . -}}
{{- $ns := include "foreman.namespace" . -}}
{{- $altNames := list (printf "%s.%s.svc" $svc $ns) (printf "%s.%s.svc.cluster.local" $svc $ns) -}}
{{- $secretName := include "foreman.webhook.secretName" . -}}
{{- $existing := lookup "v1" "Secret" $ns $secretName -}}
{{- if and $existing $existing.data (index $existing.data "tls.crt") (index $existing.data "tls.key") (index $existing.data "ca.crt") -}}
{{- dict "ca" (index $existing.data "ca.crt") "cert" (index $existing.data "tls.crt") "key" (index $existing.data "tls.key") | toYaml -}}
{{- else -}}
{{- $ca := genCA (printf "%s-webhook-ca" (include "foreman.fullname" .)) (int .Values.webhook.certValidityDays) -}}
{{- $cert := genSignedCert $svc nil $altNames (int .Values.webhook.certValidityDays) $ca -}}
{{- dict "ca" ($ca.Cert | b64enc) "cert" ($cert.Cert | b64enc) "key" ($cert.Key | b64enc) | toYaml -}}
{{- end -}}
{{- end }}
