{{/* InferMesh chart shared name/selector helpers. */}}

{{- define "infermesh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "infermesh.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version -}}
{{- end -}}

{{- define "infermesh.labels" -}}
helm.sh/chart: {{ include "infermesh.chart" . }}
{{- if .Values.appVersion }}
app.kubernetes.io/version: {{ .Values.appVersion | quote }}
{{- end }}
app.kubernetes.io/name: {{ include "infermesh.name" . }}
app.kubernetes.io/managed-by: Helm
{{- end -}}

{{- define "infermesh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "infermesh.name" . }}
{{- end -}}

{{- define "infermesh.fullname" -}}
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

{{- define "infermesh.serviceName" -}}
{{- include "infermesh.fullname" . -}}
{{- end -}}

{{- define "infermesh.routerLabels" -}}
{{- include "infermesh.labels" . }}
app.kubernetes.io/component: router
{{- end -}}

{{- define "infermesh.workerLabels" -}}
{{- include "infermesh.labels" . }}
app.kubernetes.io/component: worker
{{- end -}}

{{- define "infermesh.routerSelectorLabels" -}}
{{- include "infermesh.selectorLabels" . }}
app.kubernetes.io/component: router
{{- end -}}

{{- define "infermesh.workerSelectorLabels" -}}
{{- include "infermesh.selectorLabels" . }}
app.kubernetes.io/component: worker
{{- end -}}

{{/* Resolve the image pull spec: registry/name:tag */}}
{{- define "infermesh.routerImage" -}}
{{- printf "%s/%s:%s" .Values.image.registry "router" .Values.image.tag -}}
{{- end -}}

{{- define "infermesh.workerImage" -}}
{{- printf "%s/%s:%s" .Values.image.registry "worker" .Values.image.tag -}}
{{- end -}}