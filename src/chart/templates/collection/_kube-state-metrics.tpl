{{- define "prometheus-api-bridge.collection.kubeStateMetrics" -}}
{{- if .Values.collection.sources.kubeStateMetrics.enabled -}}
- job_name: prometheus-api-bridge-kube-state-metrics
  scrape_interval: {{ .Values.collection.interval }}
  static_configs:
    - targets: [{{ include "prometheus-api-bridge.kubeStateMetricsTarget" . | quote }}]
  metric_relabel_configs:
    - source_labels: [__name__]
      regex: {{ join "|" .Values.collection.sources.kubeStateMetrics.metrics | quote }}
      action: keep
    - target_label: cluster
      replacement: {{ .Values.clusterName | quote }}
{{- end -}}
{{- end -}}
