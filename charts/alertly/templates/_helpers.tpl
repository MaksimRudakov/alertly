{{/*
Expand the name of the chart.
*/}}
{{- define "alertly.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. Truncated to 63 chars to comply with DNS limits.
*/}}
{{- define "alertly.fullname" -}}
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
Chart name and version as used by the chart label.
*/}}
{{- define "alertly.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "alertly.labels" -}}
helm.sh/chart: {{ include "alertly.chart" . }}
{{ include "alertly.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "alertly.selectorLabels" -}}
app.kubernetes.io/name: {{ include "alertly.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "alertly.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "alertly.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Secret name — either created by this chart or an existing one referenced from values.
*/}}
{{- define "alertly.secretName" -}}
{{- if .Values.secret.create -}}
{{- include "alertly.fullname" . -}}
{{- else -}}
{{- default (include "alertly.fullname" .) .Values.secret.existingSecret -}}
{{- end -}}
{{- end -}}
