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
ServiceAccount name for the foreman-agent.
*/}}
{{- define "foreman.agent.serviceAccountName" -}}
{{- if .Values.agent.serviceAccount.create }}
{{- default (printf "%s-agent" (include "foreman.fullname" .)) .Values.agent.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.agent.serviceAccount.name }}
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
Foreman agent image (registry/repo:tag or registry/repo@digest).
*/}}
{{- define "foreman.agent.image" -}}
{{- $img := .Values.agent.image -}}
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
Gate cache PVC name.
*/}}
{{- define "foreman.gateCache.pvcName" -}}
{{- default (printf "%s-gate-cache" (include "foreman.fullname" .)) .Values.agent.gateCache.name }}
{{- end }}
