#!/usr/bin/env bash
# Checks the erp-cluster chart's admission policies (ADR-0035) with
# Kyverno's CLI against real signatures, as the cluster would:
#   - a local OCI registry (zot, with the referrers API, as ECR has) holds
#     images signed and attested with cosign v3 as the release signs them
#     (ADR-0032: a key, no transparency log), and images missing one part
#     or signed with another key;
#   - the chart, rendered with that registry and the test key, must admit
#     only the Pods and controllers whose every image is signed and carries
#     both attestations, by digest, from the release registry;
#   - its certificate policies must admit only day-long certificates from
#     the subordinate CA in the platform's namespaces, with server or client
#     auth, one SPIFFE identity and cluster names only, and certificate
#     requests only from cert-manager's controller;
#   - the erp-cell chart, rendered with those images, is admitted whole.
# What the CLI cannot model is checked on the rendered policies instead:
# ephemeral containers are added through a subresource, which both image
# policies must match.
# ZOT, CRANE, COSIGN, KYVERNO, HELM and CONFTEST name the binaries; CRDS is a file of
# the add-ons' CRDs (cert-manager's), which scripts/helm-checks.sh renders
# from the pinned charts. KEEP=1 keeps the work directory for a look.
set -euo pipefail
cd "$(dirname "$0")/.."
CRANE="${CRANE:-crane}"
COSIGN="${COSIGN:-cosign}"
KYVERNO="${KYVERNO:-kyverno}"
HELM="${HELM:-helm}"
CONFTEST="${CONFTEST:-conftest}"
CRDS="$(cd "$(dirname "${CRDS:?CRDS: a file of the add-ons CRDs, from scripts/helm-checks.sh}")" && pwd)/$(basename "$CRDS")"
root="$PWD"
tests="$root/deploy/helm/erp-cluster/tests"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/registry.sh
. "$root/scripts/lib/registry.sh"
cleanup() {
  stop_registry
  if [ -n "${KEEP:-}" ]; then echo "admission-checks: kept $work" >&2; else rm -rf "$work"; fi
}
trap cleanup EXIT
cd "$work"
start_registry "$work"

# Two keys: the release's, and another that must not be trusted.
export COSIGN_PASSWORD=""
mkdir release other
(cd release && "$COSIGN" generate-key-pair >/dev/null 2>&1)
(cd other && "$COSIGN" generate-key-pair >/dev/null 2>&1)
# As the release's signing config: no transparency log.
echo '{"mediaType":"application/vnd.dev.sigstore.signingconfig.v0.2+json","rekorTlogConfig":{},"tsaConfig":{}}' >signing-config.json
echo '{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","name":"test","dataLicense":"CC0-1.0","documentNamespace":"https://example.com/test","creationInfo":{"created":"2026-01-01T00:00:00Z","creators":["Tool: test"]},"packages":[]}' >sbom.json
echo '{"buildDefinition":{"buildType":"https://example.com/build","externalParameters":{},"resolvedDependencies":[{"uri":"git+https://example.com/erp","digest":{"gitCommit":"0000000000000000000000000000000000000000"}}]},"runDetails":{"builder":{"id":"https://example.com/builder"}}}' >provenance.json

# run LOG CMD...: CMD with its output in LOG, shown when it fails.
run() {
  local log="$1"
  shift
  if ! "$@" >"$log" 2>&1; then
    cat "$log" >&2
    echo "admission-checks: failed: $*" >&2
    exit 1
  fi
}
# image NAME: pushes an image of its own content and sets $ref to it, by
# digest. (A function setting a variable, not a command substitution,
# so that a failure stops the script.)
image() {
  local digest
  digest="$(push_image "erp/$1" v1)"
  ref="$registry/erp/$1@$digest"
  case "$ref" in *@sha256:?*) ;; *) echo "admission-checks: no digest for $1" >&2; exit 1 ;; esac
}
sign() { run sign.log "$COSIGN" sign --yes --key "$2/cosign.key" --signing-config signing-config.json "$1"; }
attest() { run attest.log "$COSIGN" attest --yes --key "$2/cosign.key" --signing-config signing-config.json --type "$3" --predicate "$4" "$1"; }

image full
full="$ref"
sign "$full" release
attest "$full" release spdxjson sbom.json
attest "$full" release slsaprovenance1 provenance.json
image unsigned
unsigned="$ref"
image attested
attested="$ref"
attest "$attested" release spdxjson sbom.json
attest "$attested" release slsaprovenance1 provenance.json
image signed
signed="$ref"
sign "$signed" release
image nosbom
nosbom="$ref"
sign "$nosbom" release
attest "$nosbom" release slsaprovenance1 provenance.json
image noprovenance
noprovenance="$ref"
sign "$noprovenance" release
attest "$noprovenance" release spdxjson sbom.json
image otherkey
otherkey="$ref"
sign "$otherkey" other
attest "$otherkey" other spdxjson sbom.json
attest "$otherkey" other slsaprovenance1 provenance.json

run render.log "$HELM" template erp-cluster "$root/deploy/helm/erp-cluster" -f "$tests/values.yaml" \
  --set "release.registries[0]=$registry" --set release.allowInsecureRegistry=true \
  --set-file release.publicKey=release/cosign.pub \
  --show-only templates/image-policy.yaml --show-only templates/certificate-policy.yaml
cp render.log policies.yaml

# Each image policy has a twin on the subresource ephemeral containers
# come in by, the same but for its name and what it matches.
python3 - policies.yaml <<'PY'
import re, sys
docs = open(sys.argv[1]).read().split("\n---\n")
def policy(name):
    found = [d for d in docs if re.search(r"^  name: %s$" % name, d, re.M)]
    if not found:
        sys.exit("admission-checks: no policy %s" % name)
    return found[0]
def judged(doc):
    return re.sub(r"^  name: .*$|^        (operations|resources): .*$", "", doc, flags=re.M)
for name in ("erp-release-signed", "erp-release-images"):
    pods, twin = policy(name), policy(name + "-ephemeral")
    if "resources: [pods/ephemeralcontainers]" not in twin:
        sys.exit("admission-checks: %s-ephemeral does not judge ephemeral containers" % name)
    if judged(pods) != judged(twin):
        sys.exit("admission-checks: %s and its ephemeral twin differ beyond what they match" % name)
PY

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
foreign="docker.io/library/busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000"
resources >resources.yaml <<EOF
admitted Pod $full
unsigned Pod $unsigned
attestations-only Pod $attested
signature-only Pod $signed
no-sbom Pod $nosbom
no-provenance Pod $noprovenance
other-key Pod $otherkey
by-tag Pod $registry/erp/full:v1
foreign Pod $foreign
unsigned-init Pod $full $unsigned
admitted-deployment Deployment $full
unsigned-deployment Deployment $unsigned
foreign-deployment Deployment $foreign
EOF
for ns in erp default; do
  echo "{\"apiVersion\": \"v1\", \"kind\": \"Namespace\", \"metadata\": {\"name\": \"$ns\", \"labels\": {\"kubernetes.io/metadata.name\": \"$ns\"}}}"
  echo "---"
done >namespaces.yaml

# expect REPORT NAME:WANT...: each named resource is admitted (passes
# every policy that judges it) or refused (fails one); a resource no
# policy judged fails the check.
expect() {
  local report="$1" ok=1 entry name want got
  shift
  if [ "$#" -eq 0 ]; then
    echo "admission-checks: nothing to expect of $report" >&2
    exit 1
  fi
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
    print("unjudged")
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
# wants FILE PREFIX: the NAME:WANT entries of FILE's "# PREFIX: " lines.
wants() { sed -n "s/^# $2: //p" "$1"; }
# apply REPORT ARGS...: Kyverno's CLI on the policies, its report in REPORT.
apply() {
  local report="$1"
  shift
  "$KYVERNO" apply policies.yaml --resource namespaces.yaml --crd-paths "$CRDS" "$@" \
    --policy-report --output-format json --remove-color >"$report" 2>"$report.log" || true
}
# entries FILE PREFIX: the entries as words.
entries() {
  local -a list=()
  while IFS= read -r line; do list+=("$line"); done < <(wants "$1" "$2")
  echo "${list[@]}"
}

apply images.json --resource resources.yaml --registry
expect images.json \
  admitted:admitted unsigned:refused attestations-only:refused signature-only:refused \
  no-sbom:refused no-provenance:refused other-key:refused by-tag:refused foreign:refused \
  unsigned-init:refused admitted-deployment:admitted unsigned-deployment:refused \
  foreign-deployment:refused
echo "admission-checks: images: only release-signed, attested images by digest from the release registry are admitted"

cp "$tests/certificates.yaml" "$tests/certificate-requests.yaml" .
apply certificates.json --resource certificates.yaml
# shellcheck disable=SC2046 # the entries are words without spaces
expect certificates.json $(entries certificates.yaml want)
# The requests, made by cert-manager's controller and by anyone else.
cat >cert-manager.yaml <<'EOF'
apiVersion: cli.kyverno.io/v1alpha1
kind: UserInfo
metadata: {name: cert-manager}
userInfo: {username: "system:serviceaccount:cert-manager:cert-manager"}
EOF
cat >someone.yaml <<'EOF'
apiVersion: cli.kyverno.io/v1alpha1
kind: UserInfo
metadata: {name: someone}
userInfo: {username: "someone@example.com", groups: ["system:authenticated"]}
EOF
apply requests.json --resource certificate-requests.yaml --userinfo cert-manager.yaml
# shellcheck disable=SC2046 # the entries are words without spaces
expect requests.json $(entries certificate-requests.yaml want)
apply requests-other.json --resource certificate-requests.yaml --userinfo someone.yaml
# shellcheck disable=SC2046 # the entries are words without spaces
expect requests-other.json $(entries certificate-requests.yaml want-other)
echo "admission-checks: certificates: only day-long subordinate-CA certificates of the platform's namespaces, requested by cert-manager, are admitted"

# The erp-cell chart, with images signed as the release signs them: the
# policies admit its controllers (and the hook Job) and its certificates.
"$HELM" template erp-cell "$root/deploy/helm/erp-cell" --namespace erp -f "$root/deploy/helm/erp-cell/tests/values.yaml" \
  --set "images.server.repository=${full%@*}" --set "images.server.digest=${full#*@}" \
  --set "images.pgbouncer.repository=${full%@*}" --set "images.pgbouncer.digest=${full#*@}" >cell-rendered.yaml
# As Helm installs them: in the release's namespace, which the template
# leaves out of the manifests.
"$CONFTEST" parse --parser yaml cell-rendered.yaml | python3 -c '
import json, sys
docs = [d for d in json.load(sys.stdin) if d]
for d in docs:
    d["metadata"].setdefault("namespace", "erp")
print("\n---\n".join(json.dumps(d) for d in docs))
' >cell.yaml
apply cell.json --resource cell.yaml --registry
expect cell.json erp-server:admitted erp-pgbouncer:admitted erp-migrate:admitted \
  erp-server-tls:admitted erp-pgbouncer-tls:admitted
echo "admission-checks: the erp-cell chart's workloads and certificates are admitted"
