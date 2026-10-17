# shellcheck shell=bash
# A local OCI registry for the checks (zot, with the referrers API, as ECR
# has), sourced by scripts/admission-checks.sh and test-mirror-images.sh.
#
# start_registry DIR: serves a registry from DIR on a free port of
# 127.0.0.1, and sets registry (its host:port) and registry_pid. Another
# process taking the port between the probe and zot's bind makes zot exit,
# which the wait sees. stop_registry stops it. ZOT names the binary.
start_registry() {
  local dir="$1" port up=0
  port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
  registry="localhost:$port"
  printf '{"distSpecVersion":"1.1.1","storage":{"rootDirectory":"%s/zot"},"http":{"address":"127.0.0.1","port":"%s"},"log":{"level":"error"}}\n' "$dir" "$port" >"$dir/zot.json"
  "${ZOT:-zot}" serve "$dir/zot.json" >"$dir/zot.log" 2>&1 &
  registry_pid=$!
  for _ in $(seq 1 100); do
    kill -0 "$registry_pid" 2>/dev/null || break
    if curl -sf "http://$registry/v2/" >/dev/null; then up=1; break; fi
    sleep 0.1
  done
  if [ "$up" -ne 1 ]; then
    cat "$dir/zot.log" >&2
    echo "registry: zot did not start" >&2
    return 1
  fi
}

stop_registry() {
  if [ -n "${registry_pid:-}" ]; then
    kill "$registry_pid" 2>/dev/null || true
    wait "$registry_pid" 2>/dev/null || true
    registry_pid=""
  fi
}

# push_image NAME TAG: pushes an OCI image of its own content to
# $registry/NAME:TAG from the current directory and prints its digest.
push_image() {
  local layer="layer-${1//\//-}-$2"
  printf '%s:%s' "$1" "$2" >"$layer"
  tar -cf "$layer.tar" "$layer"
  "${CRANE:-crane}" append --insecure --oci-empty-base -f "$layer.tar" -t "$registry/$1:$2" >/dev/null 2>"$layer.log" || {
    cat "$layer.log" >&2
    return 1
  }
  "${CRANE:-crane}" digest --insecure "$registry/$1:$2"
}
