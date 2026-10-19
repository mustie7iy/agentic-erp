{{/* The router's image by digest; both parts required. */}}
{{- define "erp-global.image" -}}
{{- $image := .Values.images.router -}}
{{- $repository := required "images.router.repository is required" $image.repository -}}
{{- $digest := required "images.router.digest is required" $image.digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $digest) -}}
{{- fail (printf "images.router.digest must be a sha256 digest, not %q" $digest) -}}
{{- end -}}
{{- if contains "@" $repository -}}
{{- fail "images.router.repository names the repository, the digest goes in images.router.digest" -}}
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
