#!/usr/bin/env bash
# Checks the cluster's Helm content without a cluster (ADR-0035), as CI's
# deploy workflow runs it:
#   - each add-on in deploy/helm/addons/charts.txt is pulled at its pinned
#     version and refused unless its archive has the pinned SHA-256, then
#     rendered with its values and an environment's (addons/testdata) and
#     validated with kubeconform; the service account its IRSA role trusts
#     (deploy/terraform/modules/cell/irsa.tf) must be the one it runs as,
#     annotated with the role;
#   - deploy/helm/erp-cluster is linted, rendered and validated against the
#     pinned add-ons' own CRDs, and must refuse to render without each value
#     it requires;
#   - scripts/admission-checks.sh then runs its policies against signed
#     images and certificates.
# HELM, KUBECONFORM and CONFTEST (which turns YAML into JSON here) name the
# binaries; admission-checks.sh takes its own.
set -euo pipefail
cd "$(dirname "$0")/.."
HELM="${HELM:-helm}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"
CONFTEST="${CONFTEST:-conftest}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/schemas"

# Kubernetes' own kinds from kubeconform's schemas; the add-ons' kinds from
# the schemas of their CRDs, written below. CRDs themselves are upstream's.
validate() {
  "$KUBECONFORM" -strict -summary -kubernetes-version 1.34.0 -skip CustomResourceDefinition \
    -schema-location default -schema-location "$work/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" "$@"
}

# The service account each add-on's IRSA role trusts.
service_account() {
  case "$1" in
    aws-privateca-issuer) echo cert-manager/aws-privateca-issuer ;;
    external-secrets) echo external-secrets/external-secrets ;;
    aws-load-balancer-controller) echo kube-system/aws-load-balancer-controller ;;
    kyverno) echo kyverno/kyverno-admission-controller ;;
    *) echo "" ;;
  esac
}

# schemas JSON: JSON schemas, strict as kubeconform's own, from the CRDs
# among the documents of JSON (a list), into $work/schemas; the CRDs
# themselves are appended to $work/crds.yaml for the admission checks.
schemas() {
  python3 - "$1" "$work/schemas" "$work/crds.yaml" <<'PY'
import json, os, sys
docs, out, crds = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3]
def strict(s):
    if isinstance(s, dict):
        if s.get("type") == "object" and "properties" in s and "additionalProperties" not in s \
                and not s.get("x-kubernetes-preserve-unknown-fields"):
            s["additionalProperties"] = False
        for v in s.values():
            strict(v)
    elif isinstance(s, list):
        for v in s:
            strict(v)
with open(crds, "a") as f:
    for d in docs:
        if not d or d.get("kind") != "CustomResourceDefinition":
            continue
        f.write(json.dumps(d) + "\n---\n")
        spec = d["spec"]
        for v in spec["versions"]:
            schema = v.get("schema", {}).get("openAPIV3Schema")
            if schema is None:
                continue
            strict(schema)
            schema.setdefault("properties", {}).update(
                {"apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"}})
            os.makedirs(os.path.join(out, spec["group"]), exist_ok=True)
            with open(os.path.join(out, spec["group"], "%s_%s.json" % (spec["names"]["kind"].lower(), v["name"])), "w") as s:
                json.dump(schema, s)
PY
}

# runs_as JSON NAMESPACE NAME: the documents of JSON hold that service
# account, annotated with an IRSA role.
runs_as() {
  python3 - "$@" <<'PY'
import json, sys
docs, namespace, name = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3]
for d in docs:
    m = (d or {}).get("metadata", {})
    if d and d.get("kind") == "ServiceAccount" and m.get("name") == name and m.get("namespace") == namespace:
        if not m.get("annotations", {}).get("eks.amazonaws.com/role-arn"):
            sys.exit("helm-checks: %s/%s carries no IRSA role annotation" % (namespace, name))
        break
else:
    sys.exit("helm-checks: no ServiceAccount %s/%s, the one its IRSA role trusts" % (namespace, name))
PY
}

: >"$work/crds.yaml"
while read -r name namespace repository chart version sha256; do
  case "$name" in '' | '#'*) continue ;; esac
  echo "== add-on $name $version"
  "$HELM" pull "$chart" --repo "$repository" --version "$version" --destination "$work" >/dev/null
  archive="$work/$chart-$version.tgz"
  got="$(shasum -a 256 "$archive" | cut -d' ' -f1)"
  if [ "$got" != "$sha256" ]; then
    echo "helm-checks: $name $version: archive SHA-256 $got, pinned $sha256" >&2
    exit 1
  fi
  "$HELM" template "$name" "$archive" --namespace "$namespace" --include-crds \
    -f "deploy/helm/addons/values/$name.yaml" -f "deploy/helm/addons/testdata/$name.yaml" >"$work/$name.yaml"
  "$CONFTEST" parse --parser yaml "$work/$name.yaml" >"$work/$name.json"
  schemas "$work/$name.json"
  validate "$work/$name.yaml"
  sa="$(service_account "$name")"
  if [ -n "$sa" ]; then
    runs_as "$work/$name.json" "${sa%%/*}" "${sa##*/}"
    if ! grep -q "namespace = \"${sa%%/*}\", name = \"${sa##*/}\"" deploy/terraform/modules/cell/irsa.tf; then
      echo "helm-checks: irsa.tf trusts no $sa" >&2
      exit 1
    fi
  fi
done <deploy/helm/addons/charts.txt

echo "== erp-cluster"
chart=deploy/helm/erp-cluster
values="$chart/tests/values.yaml"
"$HELM" lint --strict "$chart" -f "$values"
"$HELM" template erp-cluster "$chart" -f "$values" >"$work/erp-cluster.yaml"
validate "$work/erp-cluster.yaml"
# Each value the chart requires, removed in turn, fails the render and
# names the value.
for value in region subordinateCaArn clusterKmsKeyArn trustDomain release.publicKey release.registries namespaces; do
  if "$HELM" template erp-cluster "$chart" -f "$values" --set "$value=null" >/dev/null 2>"$work/err"; then
    echo "helm-checks: erp-cluster rendered without $value" >&2
    exit 1
  fi
  if ! grep -q "$value" "$work/err"; then
    cat "$work/err" >&2
    echo "helm-checks: rendering without $value failed for another reason" >&2
    exit 1
  fi
done
if "$HELM" template erp-cluster "$chart" -f "$values" --set 'release.registries[0]=registry.example/erp' >/dev/null 2>&1; then
  echo "helm-checks: erp-cluster took a registry with a path" >&2
  exit 1
fi
echo "helm-checks: erp-cluster refuses to render without each value it requires"

CRDS="$work/crds.yaml" HELM="$HELM" ./scripts/admission-checks.sh
