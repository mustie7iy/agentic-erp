{{/* Each required value, failing the render when it is missing. */}}
{{- define "erp-cluster.region" -}}
{{- required "region is required" .Values.region -}}
{{- end -}}

{{/* The namespaces the policies govern, as a CEL list. */}}
{{- define "erp-cluster.namespaceSelector" -}}
namespaceSelector:
  matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: {{ required "namespaces is required" .Values.namespaces | toJson }}
{{- end -}}

{{/* The release registries, each checked to be a host name. */}}
{{- define "erp-cluster.registries" -}}
{{- if not .Values.release.registries -}}
{{- fail "release.registries is required: the registries the release images come from" -}}
{{- end -}}
{{- range .Values.release.registries -}}
{{- if or (contains "/" .) (contains "*" .) -}}
{{- fail (printf "release.registries: %q must be a registry host, without a path or wildcard" .) -}}
{{- end -}}
{{- end -}}
{{- toJson .Values.release.registries -}}
{{- end -}}
