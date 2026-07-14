{{- define "caa-csi.validateValues" -}}
{{- if and (eq .Values.provider "aws") .Values.aws.irsa.enabled .Values.aws.staticCredentials.enabled }}
{{- fail "aws.irsa.enabled and aws.staticCredentials.enabled are mutually exclusive — choose one authentication method" }}
{{- end }}
{{- if and (eq .Values.provider "aws") .Values.aws.irsa.enabled (not .Values.aws.irsa.roleArn) }}
{{- fail "aws.irsa.roleArn is required when aws.irsa.enabled=true" }}
{{- end }}
{{- end }}

{{- define "caa-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "caa-csi.fullname" -}}
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

{{- define "caa-csi.labels" -}}
app.kubernetes.io/name: {{ include "caa-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "caa-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "caa-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "caa-csi.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else if .Values.serviceAccount.create }}
{{- include "caa-csi.fullname" . }}
{{- else }}
{{- "default" }}
{{- end }}
{{- end }}

{{- define "caa-csi.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace }}
{{- end }}

{{- define "caa-csi.storageClassName" -}}
{{- if .Values.storageClass.name }}
{{- .Values.storageClass.name }}
{{- else }}
{{- printf "%s-%s" (include "caa-csi.fullname" .) .Values.provider }}
{{- end }}
{{- end }}
