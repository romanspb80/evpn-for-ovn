{{- define "evpnOVN.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "evpnOVN.agent.defaultName" -}}
{{- printf "agent-%s" .Release.Name -}}
{{- end -}}

{{- define "evpnOVN.agent.DaemonSet.name" -}}
{{- default (include "evpnOVN.agent.defaultName" .) .Values.agent.DaemonSet.name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "evpnOVN.api.defaultName" -}}
{{- printf "agent-%s" .Release.Name -}}
{{- end -}}

{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "evpnOVN.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "evpnOVN.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "evpnOVN.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "evpnOVN.labels" -}}
app.kubernetes.io/name: {{ include "evpnOVN.name" . }}
helm.sh/chart: {{ include "evpnOVN.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Create the name of the service account to use
*/}}
{{- define "evpnOVN.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
    {{ default (include "evpnOVN.fullname" .) .Values.serviceAccount.name }}
{{- else -}}
    {{ default "default" .Values.serviceAccount.name }}
{{- end -}}
{{- end -}}
