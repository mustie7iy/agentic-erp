{{/* An image by digest, images.<name>; both parts required. */}}
{{- define "erp-global.image" -}}
{{- $image := index .root.Values.images .name -}}
{{- $repository := required (printf "images.%s.repository is required" .name) $image.repository -}}
{{- $digest := required (printf "images.%s.digest is required" .name) $image.digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $digest) -}}
{{- fail (printf "images.%s.digest must be a sha256 digest, not %q" .name $digest) -}}
{{- end -}}
{{- if contains "@" $repository -}}
{{- fail (printf "images.%s.repository names the repository, the digest goes in images.%s.digest" .name .name) -}}
{{- end -}}
{{- printf "%s@%s" $repository $digest -}}
{{- end -}}

{{/* Labels of a component. */}}
{{- define "erp-global.labels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/part-of: erp
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{- define "erp-global.selector" -}}
app.kubernetes.io/name: erp-router
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "erp-global.webSelector" -}}
app.kubernetes.io/name: erp-web
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
