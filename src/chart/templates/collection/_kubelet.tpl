{{- define "prometheus-api-bridge.collection.kubelet" -}}
{{- if .Values.collection.sources.kubelet.enabled -}}
- job_name: {{ printf "prometheus-api-bridge-kubelet-%s" .Values.clusterName | quote }}
  scrape_interval: {{ .Values.collection.interval }}
  scheme: https
  bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
  tls_config:
    ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  kubernetes_sd_configs:
    - role: node
  relabel_configs:
    - source_labels: [__meta_kubernetes_node_name]
      regex: (.+)
      target_label: __metrics_path__
      replacement: /api/v1/nodes/$1/proxy/metrics
    - target_label: __address__
      replacement: kubernetes.default.svc:443
    - target_label: cluster
      replacement: {{ .Values.clusterName | quote }}
  metric_relabel_configs:
    - source_labels: [__name__]
      regex: {{ join "|" .Values.collection.sources.kubelet.metrics | quote }}
      action: keep
{{- end -}}
{{- end -}}
