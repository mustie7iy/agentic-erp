{{/* NATS's configuration (templates/nats.yaml), one file for the ConfigMap
and the hash that restarts the nodes when it changes. */}}
{{- define "erp-cell.nats.conf" -}}
{{- $cell := include "erp-cell.cell" . }}
{{- $ns := .Release.Namespace }}
{{- $replicas := int .Values.nats.replicas }}
{{- $server := include "erp-cell.spiffe" (dict "root" . "service" "erp-server") }}
server_name: $POD_NAME
listen: 0.0.0.0:4222
# Probes only (kubelet): health and JetStream readiness, no data.
http: 0.0.0.0:8222
max_payload: 1MB

tls {
  cert_file: /etc/nats/tls/tls.crt
  key_file: /etc/nats/tls/tls.key
  ca_file: /etc/nats/trust/ca.crt
  min_version: "1.3"
  verify_and_map: true
  timeout: 5
}

authorization {
  users = [
    {
      user: {{ $server | quote }}
      permissions: {
        publish: {
          allow: [{{ printf "erp.%s.>" $cell | quote }}, "$JS.API.INFO", "$JS.API.STREAM.CREATE.>", "$JS.API.STREAM.UPDATE.>", "$JS.API.STREAM.INFO.>"]
        }
        subscribe: {
          allow: ["_INBOX.>"]
        }
      }
    }
  ]
}

jetstream {
  store_dir: /data/jetstream
  max_file_store: {{ .Values.nats.maxFileStore }}
  max_memory_store: 0
}

cluster {
  name: {{ printf "erp-%s" $cell }}
  listen: 0.0.0.0:6222
  routes: [
    {{- range $i := until $replicas }}
    {{ printf "nats://erp-nats-%d.erp-nats.%s.svc:6222" $i $ns }}
    {{- end }}
  ]
  tls {
    cert_file: /etc/nats/tls/tls.crt
    key_file: /etc/nats/tls/tls.key
    ca_file: /etc/nats/trust/ca.crt
    min_version: "1.3"
    verify: true
    timeout: 5
  }
}
{{- end -}}
