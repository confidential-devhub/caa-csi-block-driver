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
{{- else }}
{{- include "caa-csi.fullname" . }}
{{- end }}
{{- end }}

{{- define "caa-csi.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace }}
{{- end }}

{{- define "caa-csi.storageClassName" -}}
{{- if .Values.storageClass.name }}
{{- .Values.storageClass.name }}
{{- else }}
{{- printf "caa-block-%s" .Values.provider }}
{{- end }}
{{- end }}
