#!/usr/bin/env bash
# Checks the cluster's Helm content without a cluster (ADR-0035), as CI's
# deploy workflow runs it:
#   - each add-on in deploy/helm/erp-delivery/addons.txt is pulled at its
#     pinned version and refused unless its archive has the pinned SHA-256, then
#     rendered with its values and an environment's (addons/testdata) and
#     validated with kubeconform; the service account its IRSA role trusts
#     (deploy/terraform/modules/cell/irsa.tf) must be the one it runs as,
#     annotated with the role;
#   - the monitoring add-on (kube-prometheus-stack) keeps 15 days on
#     encrypted gp3 as an HA pair, reads only its own two secrets through
#     its own store and role, and Grafana signs in through OIDC alone;
#   - deploy/helm/erp-cluster and erp-cell are linted, rendered and
#     validated against the pinned add-ons' own CRDs, and must refuse to
#     render without each value they require; erp-cell's workloads must
#     pass deploy/helm/policy (non-root, read-only, digests, budgets,
#     default deny); the workflow engine's configuration it renders must be
#     one erp-temporal accepts (check-config, built from temporal/);
#   - deploy/helm/erp-global likewise, its router reading the PROXY header
#     from, and taking public connections from, the load balancer's
#     subnets alone, ready only once the load balancer finds it healthy;
#   - deploy/helm/erp-delivery likewise: the cell's Argo CD Applications,
#     one per add-on of addons.txt in its order, then erp-cluster, erp-cell
#     and (on the home cell alone) erp-global with the cell's release.json;
#     the hooks erp-cell's hook Jobs mount are hooks of a lower weight that
#     Argo CD keeps until its sync ends, and Argo CD's sync limit is above
#     the Jobs' deadline;
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
# account, annotated with an IRSA role, and a Deployment running as it.
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
if not any(d and d.get("kind") == "Deployment" and d["metadata"].get("namespace") == namespace
           and d["spec"]["template"]["spec"].get("serviceAccountName") == name for d in docs):
    sys.exit("helm-checks: no Deployment in %s runs as %s, the service account its IRSA role trusts" % (namespace, name))
PY
}

: >"$work/crds.yaml"
# The pins on descriptor 3, so that the commands in the loop cannot read them.
while read -r -u 3 name namespace repository chart version sha256; do
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
done 3<deploy/helm/erp-delivery/addons.txt

# The monitoring add-on (ADR-0043): an HA pair keeping 15 days on
# encrypted gp3; Alertmanager's configuration and Grafana's client secret
# from the cell's secrets through a store of its own, as a role that
# irsa.tf trusts for that service account alone; Grafana signing in
# through the identity provider only, with no admin of its own.
monitoring() {
  python3 - "$1" <<'PY'
import json, sys
docs = [d for d in json.load(open(sys.argv[1])) if d]
def fail(msg):
    sys.exit("helm-checks: kube-prometheus-stack: " + msg)
def one(kind, name=None):
    found = [d for d in docs if d["kind"] == kind and (name is None or d["metadata"]["name"] == name)]
    if len(found) != 1:
        fail("renders %d %s %s, want one" % (len(found), kind, name or ""))
    return found[0]
prom, am = one("Prometheus")["spec"], one("Alertmanager")["spec"]
if prom.get("replicas") != 2 or prom.get("retention") != "15d":
    fail("Prometheus runs %s replicas keeping %s, want 2 keeping 15d" % (prom.get("replicas"), prom.get("retention")))
for name, spec in (("Prometheus", prom["storage"]), ("Alertmanager", am["storage"])):
    if spec["volumeClaimTemplate"]["spec"].get("storageClassName") != "erp-monitoring":
        fail("%s's volumes are not of the erp-monitoring class" % name)
sc = one("StorageClass", "erp-monitoring")
if sc["parameters"].get("type") != "gp3" or sc["parameters"].get("encrypted") != "true" or not sc["parameters"].get("kmsKeyId", "").startswith("arn:aws:kms:"):
    fail("the erp-monitoring class is not gp3, encrypted with the cluster key")
if am.get("replicas") != 2 or am.get("configSecret") != "erp-alertmanager":
    fail("Alertmanager runs %s replicas configured from %s, want 2 from erp-alertmanager" % (am.get("replicas"), am.get("configSecret")))
store = one("SecretStore", "erp-monitoring")
if store["spec"]["provider"]["aws"]["auth"]["jwt"]["serviceAccountRef"]["name"] != "erp-monitoring-secrets":
    fail("the SecretStore does not authenticate as erp-monitoring-secrets")
sa = one("ServiceAccount", "erp-monitoring-secrets")
if not sa["metadata"].get("annotations", {}).get("eks.amazonaws.com/role-arn"):
    fail("erp-monitoring-secrets carries no IRSA role annotation")
allowed = {"erp/dev1/alertmanager-receivers", "erp/dev1/grafana-oidc"}
secrets = [d for d in docs if d["kind"] == "ExternalSecret"]
if sorted(d["spec"]["target"]["name"] for d in secrets) != ["erp-alertmanager", "erp-grafana-oidc"]:
    fail("its ExternalSecrets make %s, want erp-alertmanager and erp-grafana-oidc" % [d["spec"]["target"]["name"] for d in secrets])
for d in secrets:
    if d["spec"]["secretStoreRef"] != {"kind": "SecretStore", "name": "erp-monitoring"} or {x["remoteRef"]["key"] for x in d["spec"]["data"]} - allowed:
        fail("ExternalSecret %s reads other than its own store's two secrets" % d["metadata"]["name"])
config = one("ExternalSecret", "erp-alertmanager")["spec"]["target"]["template"]["data"]["alertmanager.yaml"]
if "receiver: erp-alerts" not in config:
    fail("Alertmanager's route does not end at erp-alerts")
if any(d["kind"] == "Secret" and ("admin-password" in d.get("data", {}) or "alertmanager.yaml" in d.get("data", {})) for d in docs):
    fail("the chart makes a Grafana admin password or Alertmanager's configuration itself")
grafana = one("Deployment", "erp-monitoring-grafana")["spec"]["template"]["spec"]["containers"]
env = {e["name"]: e.get("value") for c in grafana for e in c.get("env", [])}
if env.get("GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION") != "true":
    fail("Grafana creates an admin of its own")
ini = one("ConfigMap", "erp-monitoring-grafana")["data"]["grafana.ini"]
for want in ("[auth.generic_oauth]", "disable_login_form = true", "role_attribute_strict = true", "client_secret = $__file{"):
    if want not in ini:
        fail("Grafana's grafana.ini lacks %r: it must sign in through the identity provider alone" % want)
for section in ("[auth.basic]", "[auth.anonymous]"):
    if section + "\nenabled = false" not in ini:
        fail("Grafana's %s is not disabled" % section)
PY
}
monitoring "$work/kube-prometheus-stack.json"
grep -q 'monitoring       = { namespace = "monitoring", name = "erp-monitoring-secrets" }' deploy/terraform/modules/cell/irsa.tf ||
  { echo "helm-checks: irsa.tf trusts no monitoring/erp-monitoring-secrets" >&2; exit 1; }
if grep -qiE '(admin_?password|password[[:space:]]*[:=])' deploy/helm/addons/values/kube-prometheus-stack.yaml deploy/helm/addons/testdata/kube-prometheus-stack.yaml; then
  echo "helm-checks: kube-prometheus-stack's values set a password" >&2
  exit 1
fi
echo "helm-checks: the monitoring add-on keeps 15 days on encrypted gp3, reads its own two secrets, and signs in through the identity provider alone"

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

echo "== erp-cell"
chart=deploy/helm/erp-cell
values="$chart/tests/values.yaml"
"$HELM" lint --strict "$chart" -f "$values"
"$HELM" template erp-cell "$chart" --namespace erp -f "$values" >"$work/erp-cell.yaml"
validate "$work/erp-cell.yaml"
"$CONFTEST" verify --policy deploy/helm/policy --namespace workloads --no-color
"$CONFTEST" test --combine --policy deploy/helm/policy --namespace workloads --no-color "$work/erp-cell.yaml"
# PgBouncer's own quoting (a doubled quote; a backslash is taken as it
# is), as the pinned 1.26.0 was found to parse: a backslash escape would
# make a rotated password with a quote unloadable.
grep -qF "password='{{ .password | replace \"'\" \"''\" }}'" "$work/erp-cell.yaml" ||
  { echo "helm-checks: erp-cell's pgbouncer.ini does not double the quotes of the master password" >&2; exit 1; }
grep -qF '"erp" "{{ .clientPassword | replace "\"" "\"\"" }}"' "$work/erp-cell.yaml" ||
  { echo "helm-checks: erp-cell's user list does not double the quotes of the client password" >&2; exit 1; }
# The engine's configuration, as erp-temporal serve would load it.
"$CONFTEST" parse --parser yaml "$work/erp-cell.yaml" >"$work/erp-cell.json"
python3 - "$work/erp-cell.json" "$work/temporal.yaml" <<'PY'
import json, sys
for d in json.load(open(sys.argv[1])):
    if d and d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "erp-temporal":
        open(sys.argv[2], "w").write(d["data"]["config.yaml"])
        break
else:
    sys.exit("helm-checks: erp-cell renders no erp-temporal ConfigMap")
# The restore drill's pods reach the databases in the VPC and nothing else.
drill = [d for d in json.load(open(sys.argv[1])) if d and d.get("kind") == "NetworkPolicy" and d["metadata"]["name"] == "erp-restore-drill"]
if not drill or drill[0]["spec"]["podSelector"]["matchLabels"].get("app.kubernetes.io/name") != "erp-restore-drill" \
        or [p["port"] for r in drill[0]["spec"]["egress"] for p in r["ports"]] != [5432] or drill[0]["spec"].get("ingress"):
    sys.exit("helm-checks: erp-cell's erp-restore-drill policy must let the drill's pods reach port 5432 in the VPC, and nothing in")
# The cell's Prometheus scrapes the server's admin /metrics over verified
# TLS, and is let in to that port alone.
docs = [d for d in json.load(open(sys.argv[1])) if d]
monitors = [d for d in docs if d["kind"] == "ServiceMonitor" and d["metadata"]["name"] == "erp-server"]
if not monitors or [(e["port"], e["path"], e["scheme"], e["tlsConfig"]["ca"]["configMap"]["name"], e["tlsConfig"]["serverName"]) for e in monitors[0]["spec"]["endpoints"]] \
        != [("admin", "/metrics", "https", "erp-trust", "erp-server.erp.svc")]:
    sys.exit("helm-checks: erp-cell's erp-server ServiceMonitor must scrape admin /metrics over https, verified against erp-trust as erp-server.erp.svc")
scrape = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "erp-server-metrics"]
if not scrape or [(f["namespaceSelector"]["matchLabels"], f["podSelector"]["matchLabels"], [p["port"] for p in r["ports"]])
                  for r in scrape[0]["spec"]["ingress"] for f in r["from"]] \
        != [({"kubernetes.io/metadata.name": "monitoring"}, {"app.kubernetes.io/name": "prometheus"}, [9090])]:
    sys.exit("helm-checks: erp-cell's erp-server-metrics policy must admit the monitoring namespace's Prometheus to 9090 alone")
PY
(cd temporal && go build -o "$work/erp-temporal" ./cmd/erp-temporal)
ERP_TEMPORAL_CONFIG="$work/temporal.yaml" "$work/erp-temporal" check-config
# required VALUE...: each value, removed in turn, fails the render and
# names itself.
required() {
  local value
  for value in "$@"; do
    if "$HELM" template erp-cell "$chart" --namespace erp -f "$values" --set "$value=null" >/dev/null 2>"$work/err"; then
      echo "helm-checks: erp-cell rendered without $value" >&2
      exit 1
    fi
    if ! grep -q "$value" "$work/err"; then
      cat "$work/err" >&2
      echo "helm-checks: rendering erp-cell without $value failed for another reason" >&2
      exit 1
    fi
  done
}
required cell region trustDomain vpcCidr monitoringNamespace trust.caBundle secrets.prefix \
  images.server.repository images.server.digest images.pgbouncer.repository images.pgbouncer.digest \
  images.nats.repository images.nats.digest images.temporal.repository images.temporal.digest \
  server.roleArn server.operatorIssuers server.objectBucket server.fieldKmsKeyArn \
  server.awsEndpoints.sts server.awsEndpoints.kms server.awsEndpoints.s3 \
  database.writerHost database.masterSecretArn database.caBundle directory.masterSecretArn
for bad in 'images.server.digest=latest' 'images.server.repository=registry.example/erp-server@sha256:1' 'cell=dev-1'; do
  if "$HELM" template erp-cell "$chart" --namespace erp -f "$values" --set "$bad" >/dev/null 2>&1; then
    echo "helm-checks: erp-cell took $bad" >&2
    exit 1
  fi
done
echo "helm-checks: erp-cell refuses to render without each value it requires, with an image not by digest, or a cell id ERP_CELL refuses"

# rotating JSON...: every ExternalSecret carrying an RDS-managed master
# password (a password property) refreshes within five minutes, since a
# rotation fails new connections until it does. A hook's lives only while
# the release's hooks run, read afresh when it is made, and is not judged.
rotating() {
  python3 - "$@" <<'PY'
import json, re, sys
def seconds(d):
    return sum(int(n) * {"h": 3600, "m": 60, "s": 1}[u] for n, u in re.findall(r"(\d+)([hms])", d))
found = 0
for path in sys.argv[1:]:
    for d in json.load(open(path)):
        if not d or d.get("kind") != "ExternalSecret" or "helm.sh/hook" in d["metadata"].get("annotations", {}):
            continue
        if any(x.get("remoteRef", {}).get("property") == "password" for x in d["spec"].get("data", [])):
            found += 1
            if seconds(d["spec"]["refreshInterval"]) > 300:
                sys.exit("helm-checks: ExternalSecret %s refreshes a rotating password every %s, want 5m at most" % (d["metadata"]["name"], d["spec"]["refreshInterval"]))
if not found:
    sys.exit("helm-checks: no ExternalSecret carries a rotating password: the check judged nothing")
PY
}

echo "== erp-global"
chart=deploy/helm/erp-global
values="$chart/tests/values.yaml"
"$HELM" lint --strict "$chart" -f "$values"
"$HELM" template erp-global "$chart" --namespace erp-global -f "$values" >"$work/erp-global.yaml"
validate "$work/erp-global.yaml"
"$CONFTEST" test --combine --policy deploy/helm/policy --namespace workloads --no-color "$work/erp-global.yaml"
# The router reads the PROXY header from the load balancer's subnets, and
# takes the public listener's connections from them alone.
"$CONFTEST" parse --parser yaml "$work/erp-global.yaml" >"$work/erp-global.json"
python3 - "$work/erp-global.json" <<'PY'
import json, sys
docs = [d for d in json.load(open(sys.argv[1])) if d]
want = ["10.16.0.0/24", "10.16.1.0/24", "10.16.2.0/24"]
router = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "erp-router")
env = {e["name"]: e.get("value") for e in router["spec"]["template"]["spec"]["containers"][0]["env"]}
if env.get("ERP_ROUTER_PROXY_FROM") != ",".join(want):
    sys.exit("helm-checks: erp-global's ERP_ROUTER_PROXY_FROM is %r, want the load balancer's subnets" % env.get("ERP_ROUTER_PROXY_FROM"))
policy = next(d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "erp-router")
sources = [p["ipBlock"]["cidr"] for rule in policy["spec"]["ingress"] for p in rule["from"]]
if sources != want:
    sys.exit("helm-checks: erp-global's public listener takes %r, want the load balancer's subnets alone" % sources)
binding = next(d for d in docs if d["kind"] == "TargetGroupBinding")
if "networking" in binding["spec"]:
    sys.exit("helm-checks: erp-global's TargetGroupBinding manages security group rules; Terraform owns them")
gates = [g["conditionType"] for g in router["spec"]["template"]["spec"].get("readinessGates", [])]
if gates != ["target-health.elbv2.k8s.aws/" + binding["metadata"]["name"]]:
    sys.exit("helm-checks: erp-global's router is ready before the load balancer finds it healthy (readiness gate %r)" % gates)
PY
for value in trustDomain vpcCidr trust.caBundle secrets.prefix images.router.repository images.router.digest \
  router.targetGroupArn router.proxyFrom router.operatorIssuers directory.endpoint directory.masterSecretArn directory.caBundle; do
  if "$HELM" template erp-global "$chart" --namespace erp-global -f "$values" --set "$value=null" >/dev/null 2>"$work/err"; then
    echo "helm-checks: erp-global rendered without $value" >&2
    exit 1
  fi
  if ! grep -q "$value" "$work/err"; then
    cat "$work/err" >&2
    echo "helm-checks: rendering erp-global without $value failed for another reason" >&2
    exit 1
  fi
done
for bad in 'images.router.digest=latest' 'router.proxyFrom={}' 'router.proxyFrom={10.16.0.0}'; do
  if "$HELM" template erp-global "$chart" --namespace erp-global -f "$values" --set "$bad" >/dev/null 2>&1; then
    echo "helm-checks: erp-global took $bad" >&2
    exit 1
  fi
done
echo "helm-checks: erp-global refuses to render without each value it requires, with an image not by digest, or no load balancer subnets"
rotating "$work/erp-cell.json" "$work/erp-global.json"
echo "helm-checks: every rotating password is refreshed within five minutes"

# hooks JSON: each hook Job's secrets and config maps (mounted or read
# into its environment) come from hooks of a
# lower weight for the same events, deleted at most when the hooks
# succeeded: Helm deletes those after its last hook, Argo CD once its whole
# sync has, so they outlive the Jobs that mount them. Anything else would
# not exist yet on a first install, or be gone when the Job starts.
hooks() {
  python3 - "$1" <<'PY'
import json, sys
docs = [d for d in json.load(open(sys.argv[1])) if d]
def hook(d):
    a = d["metadata"].get("annotations", {})
    return a.get("helm.sh/hook"), int(a.get("helm.sh/hook-weight", "0")), set(filter(None, a.get("helm.sh/hook-delete-policy", "").split(",")))
made = {}
for d in docs:
    events, weight, policy = hook(d)
    if not events or d["kind"] == "Job":
        continue
    if d["kind"] == "ExternalSecret":
        made[("secret", d["spec"]["target"]["name"])] = (events, weight, policy, d["metadata"]["name"])
    elif d["kind"] in ("Secret", "ConfigMap"):
        made[(d["kind"].lower(), d["metadata"]["name"])] = (events, weight, policy, d["metadata"]["name"])
jobs = 0
for d in docs:
    events, weight, _ = hook(d)
    if d["kind"] != "Job" or not events:
        continue
    jobs += 1
    pod = d["spec"]["template"]["spec"]
    refs = []
    for v in pod.get("volumes", []):
        if "secret" in v:
            refs.append(("secret", v["secret"]["secretName"]))
        elif "configMap" in v:
            refs.append(("configmap", v["configMap"]["name"]))
    for c in pod.get("initContainers", []) + pod["containers"]:
        for e in c.get("envFrom", []):
            refs += [(k, e[r]["name"]) for k, r in (("secret", "secretRef"), ("configmap", "configMapRef")) if r in e]
        for e in c.get("env", []):
            ref = e.get("valueFrom", {})
            refs += [(k, ref[r]["name"]) for k, r in (("secret", "secretKeyRef"), ("configmap", "configMapKeyRef")) if r in ref]
    for key in refs:
        if key not in made:
            sys.exit("helm-checks: hook Job %s mounts %s %s, which no hook makes" % (d["metadata"]["name"], key[0], key[1]))
        e, w, policy, name = made[key]
        if e != events or w >= weight or not policy <= {"before-hook-creation", "hook-succeeded"}:
            sys.exit("helm-checks: hook Job %s mounts %s, a hook (%s, weight %d, deleted %s) that is not of its events, of a lower weight than %d, and kept until the hooks succeeded"
                     % (d["metadata"]["name"], name, e, w, ",".join(sorted(policy)) or "never", weight))
if not jobs:
    sys.exit("helm-checks: no hook Job: the check judged nothing")
PY
}
hooks "$work/erp-cell.json"
echo "helm-checks: erp-cell's hook Jobs mount only hooks of a lower weight, kept until the hooks succeeded"

echo "== erp-delivery"
chart=deploy/helm/erp-delivery
values="$chart/tests/cell.json"
"$HELM" lint --strict "$chart" -f "$values"
"$HELM" template erp-delivery "$chart" --namespace argocd -f "$values" >"$work/erp-delivery.yaml"
validate "$work/erp-delivery.yaml"
"$CONFTEST" parse --parser yaml "$work/erp-delivery.yaml" >"$work/erp-delivery.json"
"$HELM" template erp-delivery "$chart" --namespace argocd -f "$values" --set home=false >"$work/erp-delivery-other.yaml"
"$CONFTEST" parse --parser yaml "$work/erp-delivery-other.yaml" >"$work/erp-delivery-other.json"
"$CONFTEST" parse --parser yaml --combine deploy/helm/addons/values/argo-cd.yaml deploy/helm/erp-cell/values.yaml >"$work/limits.json"
python3 - "$work/erp-delivery.json" "$work/erp-delivery-other.json" "$chart/addons.txt" "$work/limits.json" <<'PY'
import json, sys
def fail(msg):
    sys.exit("helm-checks: erp-delivery: " + msg)
def apps(path):
    """The child Applications, in the order rendered (the root left out)."""
    return [d for d in json.load(open(path)) if d and d["kind"] == "Application" and d["metadata"]["name"] != "erp-delivery"]
pins = [l.split() for l in open(sys.argv[3]) if l.strip() and not l.lstrip().startswith("#")]
home, other = apps(sys.argv[1]), apps(sys.argv[2])
want = [p[0] for p in pins] + ["erp-cluster", "erp-cell", "erp-global"]
if [a["metadata"]["name"] for a in home] != want:
    fail("the home cell's Applications are %s, want %s" % ([a["metadata"]["name"] for a in home], want))
if [a["metadata"]["name"] for a in other] != want[:-1]:
    fail("a cell other than the home cell runs erp-global")
waves = [int(a["metadata"]["annotations"]["argocd.argoproj.io/sync-wave"]) for a in home]
addons, cluster, platform = waves[:len(pins)], waves[len(pins)], waves[len(pins) + 1:]
if addons != sorted(set(addons)) or max(addons) >= cluster or cluster >= min(platform):
    fail("the waves %s do not run the add-ons in order, then erp-cluster, then the platform" % waves)
for p, a in zip(pins, home):
    s = a["spec"]["sources"][0]
    if (a["spec"]["destination"]["namespace"], s["repoURL"], s["chart"], s["targetRevision"]) != tuple(p[1:5]):
        fail("%s is not installed as addons.txt pins it" % p[0])
for a in home:
    name, spec = a["metadata"]["name"], a["spec"]
    if spec["destination"].get("server") != "https://kubernetes.default.svc" or spec["project"] != "erp":
        fail("%s deploys elsewhere than the cell's own cluster, in project erp" % name)
    if not spec["syncPolicy"]["automated"].get("selfHeal") or "finalizers" in a["metadata"]:
        fail("%s must self-heal, and its deletion must leave what it deployed" % name)
    if name in ("erp-cell", "erp-global") and spec["source"]["helm"]["valueFiles"] != ["../../../deploy/environments/dev/dev1/release.json"]:
        fail("%s does not take the cell's release.json" % name)
project = next(d for d in json.load(open(sys.argv[1])) if d and d["kind"] == "AppProject")
if any(d.get("server") != "https://kubernetes.default.svc" for d in project["spec"]["destinations"]):
    fail("the project reaches a cluster other than the cell's own")
limits = json.load(open(sys.argv[4]))
argo, cell = limits[0]["contents"], limits[1]["contents"]
limit, deadline = int(argo["configs"]["params"]["controller.sync.timeout.seconds"]), int(cell["migrations"]["deadlineSeconds"])
if limit <= deadline:
    fail("Argo CD's sync limit (%ds) is not above the migrations Jobs' deadline (%ds)" % (limit, deadline))
PY
for value in environment cell; do
  if "$HELM" template erp-delivery "$chart" -f "$values" --set "$value=null" >/dev/null 2>"$work/err"; then
    echo "helm-checks: erp-delivery rendered without $value" >&2
    exit 1
  fi
  if ! grep -q "$value" "$work/err"; then
    cat "$work/err" >&2
    echo "helm-checks: rendering erp-delivery without $value failed for another reason" >&2
    exit 1
  fi
done
for bad in 'cell=dev-1' 'environment=sandbox' 'revision=other' 'repository=git@example.com:x.git' 'erpCell.images.server.digest=sha256:1' 'erpGlobal.images.router.digest=sha256:1'; do
  if "$HELM" template erp-delivery "$chart" -f "$values" --set "$bad" >/dev/null 2>&1; then
    echo "helm-checks: erp-delivery took $bad" >&2
    exit 1
  fi
done
echo "helm-checks: erp-delivery installs the add-ons in order, then the platform at the cell's release, on the cell's own cluster"

CRDS="$work/crds.yaml" HELM="$HELM" CONFTEST="$CONFTEST" ./scripts/admission-checks.sh
