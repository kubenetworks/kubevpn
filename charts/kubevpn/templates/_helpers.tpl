{{/*
Expand the name of the chart.
*/}}
{{- define "kubevpn.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "kubevpn.fullname" -}}
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
{{- define "kubevpn.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kubevpn.labels" -}}
helm.sh/chart: {{ include "kubevpn.chart" . }}
app: kubevpn-traffic-manager
{{ include "kubevpn.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kubevpn.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubevpn.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kubevpn.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kubevpn.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Namespace
1. special by -n
2. use default namespace kubevpn
*/}}
{{- define "kubevpn.namespace" -}}
{{- if .Release.Namespace }}
  {{- if eq .Release.Namespace "default" }}
{{- .Values.namespace }}
  {{- else }}
{{- .Release.Namespace }}
  {{- end }}
{{- else if .Values.namespace }}
{{- .Values.namespace }}
{{- else }}
{{- .Values.namespace }}
{{- end }}
{{- end }}