{{/* The repository Argo CD reads deploy/ from, with the cell's deploy key,
and the branch it runs: fixed here, so that only a reviewed change to the
chart moves them, never a cell's values. */}}
{{- define "erp-delivery.repository" -}}git@github.com:mustie7iy/agentic-erp.git{{- end -}}
{{- define "erp-delivery.revision" -}}main{{- end -}}

{{/* The cell's directory under deploy/environments, checked. */}}
{{- define "erp-delivery.cellPath" -}}
{{- range $k := list "repository" "revision" -}}
{{- if hasKey $.Values $k -}}
{{- fail (printf "%s is the chart's own, not a cell's value" $k) -}}
{{- end -}}
{{- end -}}
{{- $env := required "environment is required: dev, test, staging or production" .Values.environment -}}
{{- $cell := required "cell is required: the cell's id (ERP_CELL)" .Values.cell -}}
{{- if not (has $env (list "dev" "test" "staging" "production")) -}}
{{- fail (printf "environment %q is not dev, test, staging or production" $env) -}}
{{- end -}}
{{- if not (regexMatch "^[a-z][a-z0-9]{1,19}$" $cell) -}}
{{- fail (printf "cell %q is not a cell id: a letter, then lower-case letters or digits, at most 20" $cell) -}}
{{- end -}}
deploy/environments/{{ $env }}/{{ $cell }}
{{- end -}}

{{/* The add-ons of addons.txt, in order: name, namespace, repository, chart, version. */}}
{{- define "erp-delivery.addons" -}}
{{- $addons := list -}}
{{- range .Files.Lines "addons.txt" -}}
{{- $line := trim . -}}
{{- if and $line (not (hasPrefix "#" $line)) -}}
{{- $f := regexSplit "\\s+" $line -1 -}}
{{- $addons = append $addons (dict "name" (index $f 0) "namespace" (index $f 1) "repository" (index $f 2) "chart" (index $f 3) "version" (index $f 4)) -}}
{{- end -}}
{{- end -}}
{{- toJson $addons -}}
{{- end -}}

{{/* What every Application of the cell shares: the project, the cell's
own cluster, automated sync with pruning and self-healing, a bounded retry. */}}
{{- define "erp-delivery.sync" -}}
project: erp
syncPolicy:
  automated:
    prune: true
    selfHeal: true
  syncOptions:
    - ServerSideApply=true
    {{- range .options }}
    - {{ . }}
    {{- end }}
  retry:
    limit: 2
    backoff:
      duration: 1m
      factor: 2
      maxDuration: 10m
{{- end -}}
