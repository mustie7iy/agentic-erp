#!/usr/bin/env bash
# Checks the erp-cell chart's NATS authorization (ADR-0033, ADR-0035) on a
# real NATS server, the version go.mod pins and the cells run
# (deploy/images/upstream.txt): the chart's nats.conf, rendered, with only
# its file paths, store and ports pointed at a scratch directory, must
#   - admit the cell's server by its SPIFFE identity, let it create its
#     stream and publish its own events, and nothing else;
#   - refuse another identity of the trust domain, a certificate of another
#     authority, and a client without a certificate.
# HELM and CONFTEST name the binaries; the server is built from the module.
set -euo pipefail
cd "$(dirname "$0")/.."
HELM="${HELM:-helm}"
CONFTEST="${CONFTEST:-conftest}"
root="$PWD"
work="$(mktemp -d)"
server_pid=""
cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit cleanup
cd "$work"

(cd "$root" && go build -o "$work/nats-server" github.com/nats-io/nats-server/v2)

# The chart's configuration, as the cell dev1 of the trust domain
# dev.erp.example (tests/values.yaml).
"$HELM" template erp-cell "$root/deploy/helm/erp-cell" --namespace erp -f "$root/deploy/helm/erp-cell/tests/values.yaml" \
  --show-only templates/nats.yaml >rendered.yaml
"$CONFTEST" parse --parser yaml rendered.yaml |
  python3 -c 'import json, sys; print(next(d for d in json.load(sys.stdin) if d and d["kind"] == "ConfigMap")["data"]["nats.conf"])' >chart.conf

# Certificates: the environment's CA, the node's, the cell's server, another
# identity of the domain, and the server's identity from another CA.
issue() { # issue NAME CA SAN...
  local name="$1" ca="$2" san
  shift 2
  san="$(printf '%s,' "$@")"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$name.key" -out "$name.csr" -subj / >/dev/null 2>&1
  printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth,clientAuth\n' "${san%,}" >"$name.ext"
  openssl x509 -req -in "$name.csr" -CA "$ca.crt" -CAkey "$ca.key" -CAcreateserial -days 1 -out "$name.crt" -extfile "$name.ext" >/dev/null 2>&1
}
authority() {
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$1.key" -out "$1.crt" -days 1 -subj "/CN=$1" >/dev/null 2>&1
}
authority ca
authority other-ca
issue node ca DNS:localhost URI:spiffe://dev.erp.example/dev1/erp-nats
issue server ca URI:spiffe://dev.erp.example/dev1/erp-server DNS:erp-server.erp.svc
issue router ca URI:spiffe://dev.erp.example/global/erp-router
issue impostor other-ca URI:spiffe://dev.erp.example/dev1/erp-server

free() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
client="$(free)" cluster="$(free)" monitor="$(free)"
sed -e "s|/etc/nats/tls/tls.crt|$work/node.crt|g" -e "s|/etc/nats/tls/tls.key|$work/node.key|g" \
  -e "s|/etc/nats/trust/ca.crt|$work/ca.crt|g" -e "s|/data/jetstream|$work/jetstream|" \
  -e "s|0.0.0.0:4222|127.0.0.1:$client|" -e "s|0.0.0.0:6222|127.0.0.1:$cluster|" -e "s|0.0.0.0:8222|127.0.0.1:$monitor|" \
  chart.conf >nats.conf
if diff <(sed -e 's|/etc/nats.*||' -e 's|/data/jetstream||' -e 's|0.0.0.0:[0-9]*||' chart.conf) \
  <(sed -e "s|$work.*||" -e 's|127.0.0.1:[0-9]*||' nats.conf) >/dev/null; then :; else
  echo "nats-checks: the configuration under test differs from the chart's beyond paths and ports" >&2
  exit 1
fi
POD_NAME=erp-nats-0 ./nats-server --config nats.conf >server.log 2>&1 &
server_pid=$!
up=0
for _ in $(seq 1 100); do
  kill -0 "$server_pid" 2>/dev/null || break
  if curl -sf "http://127.0.0.1:$monitor/healthz?js-enabled-only=true" >/dev/null; then up=1; break; fi
  sleep 0.1
done
if [ "$up" -ne 1 ]; then
  cat server.log >&2
  echo "nats-checks: the server did not start on the chart's configuration" >&2
  exit 1
fi

# session CERT LINE...: connects over TLS as CERT ("none": no certificate),
# sends CONNECT, each protocol LINE and a PING, and prints what the server
# answered, one line each.
session() {
  python3 - "$client" "$work" "$@" <<'PY'
import socket, ssl, sys, time
port, work, cert, *lines = sys.argv[1:]
ctx = ssl.create_default_context(ssl.Purpose.SERVER_AUTH, cafile=work + "/ca.crt")
ctx.minimum_version = ssl.TLSVersion.TLSv1_3
# The test authority is made by openssl for the run; Python's strict X.509
# checks (3.13+) would refuse its leaves for reasons beside the point here.
ctx.verify_flags &= ~getattr(ssl, "VERIFY_X509_STRICT", 0)
if cert != "none":
    ctx.load_cert_chain(work + "/" + cert + ".crt", work + "/" + cert + ".key")
raw = socket.create_connection(("127.0.0.1", int(port)), timeout=5)
info = raw.recv(65536)
if not info.startswith(b"INFO "):
    sys.exit("no INFO: %r" % info)
out = b""
try:
    s = ctx.wrap_socket(raw, server_hostname="localhost")
    s.sendall(b'CONNECT {"verbose":true,"pedantic":false,"tls_required":true}\r\n' +
              b"".join(l.encode() + b"\r\n" for l in lines) + b"PING\r\n")
    # Read to the PONG, which the server sends after answering every line
    # before it, or until it closes the connection (an identity it refuses):
    # stopping at the first -ERR would miss the refusals that follow it.
    deadline = time.time() + 5
    while b"PONG" not in out and time.time() < deadline:
        chunk = s.recv(65536)
        if not chunk:
            break
        out += chunk
    print(out.decode(errors="replace").replace("\r\n", "\n").strip())
except (ssl.SSLError, ConnectionError, socket.timeout) as e:
    # A refusal read before the connection broke is still the answer.
    if out:
        print(out.decode(errors="replace").replace("\r\n", "\n").strip())
    print("TLS refused: %s" % type(e).__name__)
PY
}
fail() {
  echo "nats-checks: $*" >&2
  cat server.log >&2
  exit 1
}

out="$(session server "SUB _INBOX.check 1" "PUB erp.dev1.ledger.posted _INBOX.check 2" "hi" \
  'PUB $JS.API.STREAM.CREATE.ERP_DEV1_LEDGER _INBOX.check 2' "{}")"
grep -q PONG <<<"$out" || fail "the cell's server was not admitted: $out"
if grep -q "Violation" <<<"$out"; then fail "the cell's server was refused what it does: $out"; fi
echo "nats-checks: the cell's server is admitted by its SPIFFE identity and publishes its events"

out="$(session server "PUB erp.dev2.ledger.posted 2" "hi" "SUB erp.dev1.> 1" 'PUB $JS.API.STREAM.DELETE.ERP_DEV1_LEDGER 0' "")"
for want in "Publish to \"erp.dev2.ledger.posted\"" "Subscription to \"erp.dev1.>\"" "Publish to \"\$JS.API.STREAM.DELETE.ERP_DEV1_LEDGER\""; do
  grep -qF "$want" <<<"$out" || fail "the cell's server was allowed more than it does ($want): $out"
done
echo "nats-checks: it may not publish another cell's events, read events or delete streams"

out="$(session router)"
grep -q "Authorization Violation" <<<"$out" || fail "another identity was admitted: $out"
out="$(session impostor)"
if grep -q PONG <<<"$out"; then fail "a certificate of another authority was admitted: $out"; fi
out="$(session none)"
if grep -q PONG <<<"$out"; then fail "a client without a certificate was admitted: $out"; fi
echo "nats-checks: another identity, another authority and no certificate are refused"
finished
