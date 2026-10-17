#!/usr/bin/env bash
# Checks the erp-cluster chart's admission policies (ADR-0035) with
# Kyverno's CLI against real signatures, as the cluster would:
#   - a local OCI registry (zot, with the referrers API, as ECR has) holds
#     images signed and attested with cosign v3 exactly as
#     scripts/sign-images.sh signs releases, and images missing one part or
#     signed with another key;
#   - the chart, rendered with that registry and the test key, must admit
#     only the Pods and controllers whose every image is signed and carries
#     both attestations, by digest, from the release registry;
#   - its certificate policy must admit only day-long certificates from the
#     subordinate CA with server or client auth and one SPIFFE identity.
# ZOT, CRANE, COSIGN, KYVERNO and HELM name the binaries; CRDS is a file of
# the add-ons' CRDs (cert-manager's Certificate), which scripts/helm-checks.sh
# renders from the pinned charts.
set -euo pipefail
cd "$(dirname "$0")/.."
ZOT="${ZOT:-zot}"
CRANE="${CRANE:-crane}"
COSIGN="${COSIGN:-cosign}"
KYVERNO="${KYVERNO:-kyverno}"
HELM="${HELM:-helm}"
CRDS="$(cd "$(dirname "${CRDS:?CRDS: a file of the add-ons CRDs, from scripts/helm-checks.sh}")" && pwd)/$(basename "$CRDS")"
root="$PWD"
work="$(mktemp -d)"
zot_pid=""
cleanup() {
  if [ -n "$zot_pid" ]; then kill "$zot_pid" 2>/dev/null || true; fi
  # KEEP=1 keeps the registry's content, keys and reports for a look.
  if [ -n "${KEEP:-}" ]; then echo "admission-checks: kept $work" >&2; else rm -rf "$work"; fi
}
trap cleanup EXIT
cd "$work"

port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
registry="localhost:$port"
printf '{"distSpecVersion":"1.1.1","storage":{"rootDirectory":"%s/zot"},"http":{"address":"127.0.0.1","port":"%s"},"log":{"level":"error"}}\n' "$work" "$port" >zot.json
"$ZOT" serve zot.json >zot.log 2>&1 &
zot_pid=$!
for _ in $(seq 1 100); do
  if curl -sf "http://$registry/v2/" >/dev/null; then break; fi
  sleep 0.1
done
curl -sf "http://$registry/v2/" >/dev/null || { cat zot.log; echo "admission-checks: the registry did not start" >&2; exit 1; }

# Two keys: the release's, and another that must not be trusted.
export COSIGN_PASSWORD=""
mkdir release other
(cd release && "$COSIGN" generate-key-pair >/dev/null 2>&1)
(cd other && "$COSIGN" generate-key-pair >/dev/null 2>&1)
# As release/signing-config.json: no transparency log.
echo '{"mediaType":"application/vnd.dev.sigstore.signingconfig.v0.2+json","rekorTlogConfig":{},"tsaConfig":{}}' >signing-config.json
echo '{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","name":"test","dataLicense":"CC0-1.0","documentNamespace":"https://example.com/test","creationInfo":{"created":"2026-01-01T00:00:00Z","creators":["Tool: test"]},"packages":[]}' >sbom.json
echo '{"buildDefinition":{"buildType":"https://example.com/build","externalParameters":{},"resolvedDependencies":[{"uri":"git+https://example.com/erp","digest":{"gitCommit":"0000000000000000000000000000000000000000"}}]},"runDetails":{"builder":{"id":"https://example.com/builder"}}}' >provenance.json

# image NAME: an image of its own content, by digest.
image() {
  printf '%s' "$1" >"layer-$1"
  tar -cf "layer-$1.tar" "layer-$1"
  "$CRANE" append --insecure --oci-empty-base -f "layer-$1.tar" -t "$registry/erp/$1:v1" >/dev/null 2>&1
  echo "$registry/erp/$1@$("$CRANE" digest --insecure "$registry/erp/$1:v1")"
}
sign() { "$COSIGN" sign --yes --key "$2/cosign.key" --signing-config signing-config.json "$1" >/dev/null 2>&1; }
attest() { "$COSIGN" attest --yes --key "$2/cosign.key" --signing-config signing-config.json --type "$3" --predicate "$4" "$1" >/dev/null 2>&1; }
release() { sign "$1" release; attest "$1" release spdxjson sbom.json; attest "$1" release slsaprovenance1 provenance.json; }

full="$(image full)"
release "$full"
unsigned="$(image unsigned)"
attested="$(image attested)"
attest "$attested" release spdxjson sbom.json
attest "$attested" release slsaprovenance1 provenance.json
signed="$(image signed)"
sign "$signed" release
nosbom="$(image nosbom)"
sign "$nosbom" release
attest "$nosbom" release slsaprovenance1 provenance.json
noprovenance="$(image noprovenance)"
sign "$noprovenance" release
attest "$noprovenance" release spdxjson sbom.json
otherkey="$(image otherkey)"
sign "$otherkey" other
attest "$otherkey" other spdxjson sbom.json
attest "$otherkey" other slsaprovenance1 provenance.json

"$HELM" template erp-cluster "$root/deploy/helm/erp-cluster" -f "$root/deploy/helm/erp-cluster/tests/values.yaml" \
  --set "release.registries[0]=$registry" --set release.allowInsecureRegistry=true \
  --set-file release.publicKey=release/cosign.pub \
  --show-only templates/image-policy.yaml --show-only templates/certificate-policy.yaml >policies.yaml

# The workloads to admit or refuse, as JSON documents (YAML to Kyverno):
# NAME KIND IMAGE [INIT_IMAGE] per line.
resources() {
  python3 -c '
import json, sys
docs = []
for line in sys.stdin.read().split("\n"):
    if not line:
        continue
    name, kind, image, *init = line.split()
    spec = {"containers": [{"name": "c", "image": image}]}
    if init:
        spec["initContainers"] = [{"name": "i", "image": init[0]}]
    meta = {"name": name, "namespace": "erp"}
    if kind == "Pod":
        docs.append({"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec})
    else:
        labels = {"app": name}
        docs.append({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta,
                     "spec": {"selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels}, "spec": spec}}})
print("\n---\n".join(json.dumps(d) for d in docs))
'
}
resources >resources.yaml <<EOF
admitted Pod $full
unsigned Pod $unsigned
attestations-only Pod $attested
signature-only Pod $signed
no-sbom Pod $nosbom
no-provenance Pod $noprovenance
other-key Pod $otherkey
by-tag Pod $registry/erp/full:v1
foreign Pod docker.io/library/busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000
unsigned-init Pod $full $unsigned
admitted-deployment Deployment $full
unsigned-deployment Deployment $unsigned
foreign-deployment Deployment docker.io/library/busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000
EOF
echo '{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "erp", "labels": {"kubernetes.io/metadata.name": "erp"}}}' >namespace.yaml

# expect FILE RESOURCES NAME:WANT...: each named resource passes or fails
# every policy that applies to it.
expect() {
  local report="$1" ok=1 entry name want got
  shift
  for entry in "$@"; do
    name="${entry%%:*}" want="${entry##*:}"
    got="$(python3 - "$report" "$name" <<'PY'
import json, sys
report, name = sys.argv[1], sys.argv[2]
try:
    loaded = json.load(open(report))
except ValueError:
    print("no-report")
    sys.exit()
results = [r for r in loaded.get("results", [])
           if any(x.get("name") == name for x in r.get("resources", []))]
if not results:
    print("none")
elif any(r.get("result") in ("fail", "error") for r in results):
    print("refused")
else:
    print("admitted")
PY
)"
    if [ "$got" != "$want" ]; then
      echo "admission-checks: $name: got $got, want $want" >&2
      ok=0
    fi
  done
  [ "$ok" -eq 1 ]
}

"$KYVERNO" apply policies.yaml --resource resources.yaml --resource namespace.yaml --registry \
  --policy-report --output-format json --remove-color >images-report.json 2>images.log || true
grep -v '^Applying\|^$' images.log >&2 || true
expect images-report.json \
  admitted:admitted unsigned:refused attestations-only:refused signature-only:refused \
  no-sbom:refused no-provenance:refused other-key:refused by-tag:refused foreign:refused \
  unsigned-init:refused admitted-deployment:admitted unsigned-deployment:refused \
  foreign-deployment:refused
echo "admission-checks: images: only release-signed, attested images by digest from the release registry are admitted"

cp "$root/deploy/helm/erp-cluster/tests/certificates.yaml" certificates.yaml
"$KYVERNO" apply policies.yaml --resource certificates.yaml --resource namespace.yaml --crd-paths "$CRDS" \
  --policy-report --output-format json --remove-color >certificates-report.json 2>certificates.log || true
want=()
while IFS= read -r line; do want+=("$line"); done < <(sed -n 's/^# want: //p' certificates.yaml)
expect certificates-report.json "${want[@]}"
echo "admission-checks: certificates: only day-long subordinate-CA certificates with one SPIFFE identity are admitted"
