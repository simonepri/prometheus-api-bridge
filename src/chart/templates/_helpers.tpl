{{- define "prometheus-api-bridge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "prometheus-api-bridge.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := include "prometheus-api-bridge.name" . }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "prometheus-api-bridge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "prometheus-api-bridge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prometheus-api-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: bridge
{{- end }}

{{- define "prometheus-api-bridge.labels" -}}
helm.sh/chart: {{ include "prometheus-api-bridge.chart" . }}
{{ include "prometheus-api-bridge.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "prometheus-api-bridge.collectorSelectorLabels" -}}
app.kubernetes.io/name: {{ include "prometheus-api-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: collector
{{- end }}

{{- define "prometheus-api-bridge.collectorLabels" -}}
helm.sh/chart: {{ include "prometheus-api-bridge.chart" . }}
{{ include "prometheus-api-bridge.collectorSelectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "prometheus-api-bridge.image" -}}
{{- printf "%s:%s" .Values.server.image.repository (default .Chart.AppVersion .Values.server.image.tag) }}
{{- end }}

{{- define "prometheus-api-bridge.collectorImage" -}}
{{- printf "%s:%s" .Values.collection.image.repository .Values.collection.image.tag }}
{{- end }}

{{- /*
collectorGoMemLimit renders 80% of the Collector memory limit in bytes, or nothing when
useGOMEMLIMIT is off or no memory limit is set.
*/ -}}
{{- define "prometheus-api-bridge.collectorGoMemLimit" -}}
{{- $limit := dig "limits" "memory" "" .Values.collection.resources | toString -}}
{{- if and .Values.collection.useGOMEMLIMIT $limit -}}
{{- $number := regexFind "^[0-9]+([.][0-9]+)?" $limit -}}
{{- $suffix := trimPrefix $number $limit -}}
{{- $multipliers := dict "" 1 "k" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 -}}
{{- if or (not $number) (not (hasKey $multipliers $suffix)) -}}
{{- fail (printf "collection.resources.limits.memory %q must be a number with an optional k, M, G, T, Ki, Mi, Gi, or Ti suffix" $limit) -}}
{{- end -}}
{{- mulf (float64 $number) (index $multipliers $suffix) 0.8 | floor | int64 -}}
{{- end -}}
{{- end }}

{{- define "prometheus-api-bridge.collectorServiceAccountName" -}}
{{- printf "%s-collector" (include "prometheus-api-bridge.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "prometheus-api-bridge.rbacServiceAccountName" -}}
{{- if eq .Values.collection.mode "existing" -}}
{{- required "collection.existing.serviceAccount.name is required in existing mode when collection.rbac.create=true" .Values.collection.existing.serviceAccount.name -}}
{{- else -}}
{{- include "prometheus-api-bridge.collectorServiceAccountName" . -}}
{{- end -}}
{{- end }}

{{- define "prometheus-api-bridge.rbacServiceAccountNamespace" -}}
{{- if eq .Values.collection.mode "existing" -}}
{{- required "collection.existing.serviceAccount.namespace is required in existing mode when collection.rbac.create=true" .Values.collection.existing.serviceAccount.namespace -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end }}

{{- define "prometheus-api-bridge.kubeStateMetricsTarget" -}}
{{- if index .Values "kube-state-metrics" "enabled" -}}
{{- $override := default "" (index .Values "kube-state-metrics" "fullnameOverride") -}}
{{- $name := default "kube-state-metrics" (index .Values "kube-state-metrics" "nameOverride") -}}
{{- $generatedName := ternary .Release.Name (printf "%s-%s" .Release.Name $name) (contains $name .Release.Name) -}}
{{- $serviceName := default $generatedName $override | trunc 63 | trimSuffix "-" -}}
{{- printf "%s.%s.svc:8080" $serviceName .Release.Namespace -}}
{{- else -}}
{{- required "collection.sources.kubeStateMetrics.target is required when kube-state-metrics.enabled=false" .Values.collection.sources.kubeStateMetrics.target -}}
{{- end -}}
{{- end }}
