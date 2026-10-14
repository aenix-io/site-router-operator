{{/*
The name of the chart.
*/}}
{{- define "site-router-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The full name of the release, which the objects of the chart are named after.
*/}}
{{- define "site-router-operator.fullname" -}}
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

{{- define "site-router-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "site-router-operator.labels" -}}
helm.sh/chart: {{ include "site-router-operator.chart" . }}
{{ include "site-router-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
The labels the operator pods are selected by. They also say which pods may call
the management API of a gateway under the cilium isolation.
*/}}
{{- define "site-router-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "site-router-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "site-router-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "site-router-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "site-router-operator.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- if .Values.image.digest }}
{{- printf "%s%s@%s" .Values.image.repository (ternary (printf ":%s" $tag) "" (ne $tag "")) .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
{{- end }}

{{/*
Where the operator reads its configuration from: the directory the ConfigMap is
mounted at and the key of the file in it.
*/}}
{{- define "site-router-operator.configDir" -}}
/etc/site-router-operator
{{- end }}
{{- define "site-router-operator.configKey" -}}
config.yaml
{{- end }}

{{/*
The configuration of the operator: .Values.config, with the identity of the
operator filled in for the cilium isolation when it is left out. Under cilium the
management API of a gateway admits only the pods that carry these labels in this
namespace, and the chart is what knows them.
*/}}
{{- define "site-router-operator.config" -}}
{{- $cfg := deepCopy (.Values.config | default dict) }}
{{- $gateway := get $cfg "gateway" | default dict }}
{{- $isolation := get $gateway "isolation" | default dict }}
{{- if eq (toString (get $isolation "mode")) "cilium" }}
{{- $controller := get $isolation "controller" | default dict }}
{{- if not (get $controller "namespace") }}
{{- $_ := set $controller "namespace" .Release.Namespace }}
{{- end }}
{{- if not (get $controller "labels") }}
{{- $_ := set $controller "labels" (include "site-router-operator.selectorLabels" . | fromYaml) }}
{{- end }}
{{- $_ := set $isolation "controller" $controller }}
{{- $_ := set $gateway "isolation" $isolation }}
{{- $_ := set $cfg "gateway" $gateway }}
{{- end }}
{{- toYaml $cfg }}
{{- end }}
