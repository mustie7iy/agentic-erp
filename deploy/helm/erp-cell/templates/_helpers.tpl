{{/* An image by digest; both parts required. */}}
{{- define "erp-cell.image" -}}
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

{{/* The SPIFFE identity of a service of this cell. */}}
{{- define "erp-cell.spiffe" -}}
{{- printf "spiffe://%s/%s/%s" (required "trustDomain is required" .root.Values.trustDomain) (required "cell is required" .root.Values.cell) .service -}}
{{- end -}}

{{/* Labels of a component. */}}
{{- define "erp-cell.labels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/part-of: erp
app.kubernetes.io/instance: {{ .root.Release.Name }}
erp.platform/cell: {{ required "cell is required" .root.Values.cell }}
{{- end -}}

{{- define "erp-cell.selector" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{/* The restricted pod security context of every Pod. */}}
{{- define "erp-cell.podSecurity" -}}
runAsNonRoot: true
runAsUser: {{ .uid }}
runAsGroup: {{ .uid }}
fsGroup: {{ .uid }}
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/* The restricted security context of every container. */}}
{{- define "erp-cell.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
privileged: false
capabilities:
  drop: [ALL]
{{- end -}}

{{/* A service's Certificate: a day long, its SPIFFE identity and its own
service names, from the environment's subordinate CA. */}}
{{- define "erp-cell.certificate" -}}
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ .name }}
  labels:
    {{- include "erp-cell.labels" (dict "root" .root "component" .component) | nindent 4 }}
spec:
  secretName: {{ .name }}
  duration: {{ .root.Values.certificates.duration }}
  renewBefore: {{ .root.Values.certificates.renewBefore }}
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  usages: {{ .usages | toJson }}
  uris:
    - {{ include "erp-cell.spiffe" (dict "root" .root "service" .service) }}
  dnsNames:
    - {{ .component }}.{{ .root.Release.Namespace }}.svc
    - {{ .component }}.{{ .root.Release.Namespace }}.svc.cluster.local
  issuerRef:
    group: awspca.cert-manager.io
    kind: AWSPCAClusterIssuer
    name: erp-subordinate
{{- end -}}
