{{/* The workflow engine's configuration (Temporal's YAML, erp-temporal
serve): its two databases on the writer over verified TLS as its own
login, whose password erp-temporal adds from its secret file; every
listener and client TLS 1.3 with certificates of the environment's CA,
required of every client; this node's address from POD_IP. */}}
{{- define "erp-cell.temporal.conf" -}}
{{- $ns := .Release.Namespace }}
{{- $writer := required "database.writerHost is required: cell.database_endpoint" .Values.database.writerHost }}
{{- $name := printf "erp-temporal.%s.svc" $ns }}
{{- $t := .Values.temporal }}
log:
  stdout: true
  level: {{ $t.logLevel }}
  format: json
persistence:
  # Fixed once the database exists: Temporal cannot change it.
  numHistoryShards: {{ int $t.historyShards }}
  defaultStore: default
  visibilityStore: visibility
  datastores:
    {{- range $store, $database := dict "default" $t.database.name "visibility" $t.database.visibility }}
    {{ $store }}:
      sql:
        pluginName: postgres12_pgx
        databaseName: {{ $database }}
        connectAddr: "{{ $writer }}:5432"
        connectProtocol: tcp
        user: {{ $t.database.user }}
        maxConns: {{ int (index $t.database.maxConns $store) }}
        maxIdleConns: {{ int (index $t.database.maxConns $store) }}
        maxConnLifetime: 1h
        tls:
          enabled: true
          enableHostVerification: true
          caFile: /etc/temporal/rds/ca.pem
    {{- end }}
global:
  membership:
    maxJoinDuration: 30s
  tls:
    refreshInterval: 1m
    {{- range $listener := list "internode" "frontend" }}
    {{ $listener }}:
      server:
        certFile: /etc/temporal/tls/tls.crt
        keyFile: /etc/temporal/tls/tls.key
        requireClientAuth: true
        clientCaFiles: [/etc/temporal/trust/ca.crt]
      client:
        serverName: {{ $name }}
        rootCaFiles: [/etc/temporal/trust/ca.crt]
    {{- end }}
    systemWorker:
      certFile: /etc/temporal/tls/tls.crt
      keyFile: /etc/temporal/tls/tls.key
      client:
        serverName: {{ $name }}
        rootCaFiles: [/etc/temporal/trust/ca.crt]
services:
  {{- range $service, $ports := dict "frontend" (list 7233 6933) "history" (list 7234 6934) "matching" (list 7235 6935) "worker" (list 7239 6939) }}
  {{ $service }}:
    rpc:
      grpcPort: {{ index $ports 0 }}
      membershipPort: {{ index $ports 1 }}
      bindOnIP: 0.0.0.0
  {{- end }}
clusterMetadata:
  enableGlobalNamespace: false
  failoverVersionIncrement: 10
  masterClusterName: {{ include "erp-cell.cell" . }}
  currentClusterName: {{ include "erp-cell.cell" . }}
  clusterInformation:
    {{ include "erp-cell.cell" . }}:
      enabled: true
      initialFailoverVersion: 1
      rpcName: frontend
      rpcAddress: "{{ $name }}:7233"
dcRedirectionPolicy:
  policy: noop
archival:
  history: {state: disabled}
  visibility: {state: disabled}
namespaceDefaults:
  archival:
    history: {state: disabled}
    visibility: {state: disabled}
publicClient:
  hostPort: "{{ $name }}:7233"
{{- end -}}

{{/* The ports the engine's nodes reach each other on: each service's gRPC
and membership port. */}}
{{- define "erp-cell.temporal.ports" -}}
{{- range $port := list 7233 6933 7234 6934 7235 6935 7239 6939 }}
- {protocol: TCP, port: {{ $port }}}
{{- end }}
{{- end -}}
