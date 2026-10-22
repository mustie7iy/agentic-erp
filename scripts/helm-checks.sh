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
#   - the logs add-on (fluent-bit) ships every container's lines, parsed,
#     to the log group Terraform made, creating none; the traces add-on
#     (opentelemetry-collector) takes OTLP/HTTP from the server alone and
#     sends 5% of traces and every failed one to X-Ray; neither makes
#     metrics of what it ships; the logging namespace is default deny,
#     reaching DNS and 443 in the VPC alone (erp-cluster);
#   - deploy/helm/erp-cluster and erp-cell are linted, rendered and
#     validated against the pinned add-ons' own CRDs, and must refuse to
#     render without each value they require; erp-cell's workloads must
#     pass deploy/helm/policy (non-root, read-only, digests, budgets,
#     default deny); the workflow engine's configuration it renders must be
#     one erp-temporal accepts (check-config, built from temporal/); its
#     SLO rules pass promtool's checks and unit tests (rules/); its
#     dashboards (dashboards/) render as the ConfigMaps Grafana's sidecar
#     loads;
#   - deploy/helm/erp-global likewise, its router reading the PROXY header
#     from, and taking public connections from, the load balancer's
#     subnets alone, ready only once the load balancer finds it healthy;
#     erp-web (ADR-0052) likewise behind its own load balancer, in
#     production, on its own https hostname, reaching the router's public
#     listener alone, with no SPIFFE certificate;
#   - deploy/helm/erp-delivery likewise: the cell's Argo CD Applications,
#     one per add-on of addons.txt in its order, then erp-cluster, erp-cell
#     and (on the home cell alone) erp-global with the cell's release.json;
#     the hooks erp-cell's hook Jobs mount are hooks of a lower weight that
#     Argo CD keeps until its sync ends, and Argo CD's sync limit is above
#     the Jobs' deadline;
#   - scripts/admission-checks.sh then runs its policies against signed
#     images and certificates.
# HELM, KUBECONFORM, CONFTEST (which turns YAML into JSON here) and
# PROMTOOL name the binaries; admission-checks.sh takes its own.
set -euo pipefail
cd "$(dirname "$0")/.."
HELM="${HELM:-helm}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"
CONFTEST="${CONFTEST:-conftest}"
PROMTOOL="${PROMTOOL:-promtool}"
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
    fluent-bit) echo logging/erp-logging ;;
    opentelemetry-collector) echo tracing/erp-tracing ;;
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
# account, annotated with an IRSA role, and a Deployment or DaemonSet
# running as it.
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
if not any(d and d.get("kind") in ("Deployment", "DaemonSet") and d["metadata"].get("namespace") == namespace
           and d["spec"]["template"]["spec"].get("serviceAccountName") == name for d in docs):
    sys.exit("helm-checks: no Deployment or DaemonSet in %s runs as %s, the service account its IRSA role trusts" % (namespace, name))
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
if "- receiver: erp-heartbeat" not in config or """matchers: ['alertname="ErpWatchdog"']""" not in config:
    fail("Alertmanager does not send erp-cell's ErpWatchdog to erp-heartbeat")
if any(d["kind"] == "Secret" and ("admin-password" in d.get("data", {}) or "alertmanager.yaml" in d.get("data", {})) for d in docs):
    fail("the chart makes a Grafana admin password or Alertmanager's configuration itself")
grafana = one("Deployment", "erp-monitoring-grafana")["spec"]["template"]["spec"]["containers"]
env = {e["name"]: e.get("value") for c in grafana for e in c.get("env", [])}
if env.get("GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION") != "true":
    fail("Grafana creates an admin of its own")
# The dashboards' sidecar keeps watching (not only an init container at
# start) for erp-cell's ConfigMaps by the label they carry.
sidecar = {e["name"]: e.get("value") for c in grafana if c["name"].endswith("-sc-dashboard") for e in c.get("env", [])}
if (sidecar.get("LABEL"), sidecar.get("LABEL_VALUE"), sidecar.get("NAMESPACE")) != ("grafana_dashboard", "1", "ALL"):
    fail("Grafana's dashboards sidecar must load grafana_dashboard=1 ConfigMaps from every namespace, got %s" % sidecar)
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

# The logs and traces add-ons (ADR-0043, T54g): what they ship and where,
# and that neither turns a log field or a span attribute (a tenant's id
# among them) into a metric: no log_to_metrics filter, no connector.
logs() {
  python3 - "$1" <<'PY'
import json, re, sys
docs = [d for d in json.load(open(sys.argv[1])) if d]
def fail(msg):
    sys.exit("helm-checks: fluent-bit: " + msg)
def one(kind, name):
    found = [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
    if len(found) != 1:
        fail("renders %d %s %s, want one" % (len(found), kind, name))
    return found[0]
conf = one("ConfigMap", "erp-logging")["data"]["fluent-bit.conf"]
sections = []
for line in conf.splitlines():
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    if line.startswith("["):
        sections.append((line.strip("[]"), {}))
    else:
        key, _, value = line.partition(" ")
        sections[-1][1][key.lower()] = value.strip()
def of(kind):
    return [s for k, s in sections if k == kind]
outputs = [(o.get("name"), o.get("match"), o.get("log_group_name"), o.get("auto_create_group"), o.get("log_retention_days")) for o in of("OUTPUT")]
if outputs != [("cloudwatch_logs", "kube.*", "/erp/dev1/containers", "Off", None)]:
    fail("its one output must send every container's lines to /erp/dev1/containers, creating no group and setting no retention (Terraform's): %s" % outputs)
if [(i.get("name"), i.get("path")) for i in of("INPUT")] != [("tail", "/var/log/containers/*.log")]:
    fail("its one input must tail every container's log")
filters = of("FILTER")
if [(f.get("name"), f.get("merge_log")) for f in filters] != [("kubernetes", "On")]:
    fail("its one filter must be kubernetes, parsing the server's JSON lines into fields (tenant_id stays a field, never a metric): %s" % filters)
if filters[0].get("kube_url") != "https://0123456789ABCDEF0123456789ABCDEF.gr7.eu-central-1.eks.amazonaws.com":
    fail("its kubernetes filter must read the cluster's private endpoint (erp.kubeURL), in the VPC, which its namespace's policy admits; the kubernetes Service's address is not")
if one("DaemonSet", "erp-logging")["spec"]["template"]["spec"].get("tolerations") != [{"operator": "Exists"}]:
    fail("it must run on every node, the tainted ones too")
PY
}
traces() {
  python3 - "$1" "$work/relay.yaml" <<'PY'
import json, sys
docs = [d for d in json.load(open(sys.argv[1])) if d]
relay = [d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "erp-tracing"]
if len(relay) != 1:
    sys.exit("helm-checks: opentelemetry-collector: renders %d ConfigMap erp-tracing, want one" % len(relay))
open(sys.argv[2], "w").write(relay[0]["data"]["relay"])
deploy = [d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "erp-tracing"]
if len(deploy) != 1 or deploy[0]["spec"]["replicas"] != 1:
    sys.exit("helm-checks: opentelemetry-collector: one Deployment of one replica, so each trace's spans meet in one place for tail sampling")
policy = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "erp-tracing"]
if len(policy) != 1 or [([p["port"] for p in r["ports"]], r["from"]) for r in policy[0]["spec"]["ingress"]] != [([4318], [
        {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "erp"}}, "podSelector": {"matchLabels": {"app.kubernetes.io/name": "erp-server"}}}])]:
    sys.exit("helm-checks: opentelemetry-collector: its policy must admit the erp namespace's server to 4318 alone")
PY
  "$CONFTEST" parse --parser yaml "$work/relay.yaml" >"$work/relay.json"
  python3 - "$work/relay.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
def fail(msg):
    sys.exit("helm-checks: opentelemetry-collector: " + msg)
pipelines = c["service"]["pipelines"]
if pipelines != {"traces": {"receivers": ["otlp"], "processors": ["memory_limiter", "tail_sampling", "batch"], "exporters": ["awsxray"]}}:
    fail("its one pipeline must take OTLP traces, limit memory, sample and batch, and send them to X-Ray: %s" % pipelines)
if c.get("connectors") or set(c["receivers"]) != {"otlp"} or set(c["receivers"]["otlp"]["protocols"]) != {"http"} or set(c["exporters"]) != {"awsxray"}:
    fail("receives OTLP/HTTP and exports to X-Ray only, with no connector (spans make no metrics)")
if c["exporters"]["awsxray"].get("skip_timestamp_validation") is not True:
    fail("its X-Ray exporter must accept the server's W3C trace ids (skip_timestamp_validation), or it drops every span")
policies = c["processors"]["tail_sampling"]["policies"]
if sorted((p["type"], json.dumps(p.get(p["type"]), sort_keys=True)) for p in policies) != [
        ("probabilistic", '{"sampling_percentage": 5}'), ("status_code", '{"status_codes": ["ERROR"]}')]:
    fail("must keep 5%% of traces and every failed one: %s" % policies)
PY
}
logs "$work/fluent-bit.json"
traces "$work/opentelemetry-collector.json"
for sa in 'logging +=.*namespace = "logging", name = "erp-logging"' 'tracing +=.*namespace = "tracing", name = "erp-tracing"'; do
  grep -qE "^ +$sa" deploy/terraform/modules/cell/irsa.tf || { echo "helm-checks: irsa.tf trusts no ${sa%% *} service account" >&2; exit 1; }
done
echo "helm-checks: container logs go to their log group, traces sampled to X-Ray, and neither makes a metric"

echo "== erp-cluster"
chart=deploy/helm/erp-cluster
values="$chart/tests/values.yaml"
"$HELM" lint --strict "$chart" -f "$values"
"$HELM" template erp-cluster "$chart" -f "$values" >"$work/erp-cluster.yaml"
validate "$work/erp-cluster.yaml"
# Each value the chart requires, removed in turn, fails the render and
# names the value.
for value in region subordinateCaArn clusterKmsKeyArn trustDomain vpcCidr loggingNamespace release.publicKey release.registries namespaces; do
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
"$CONFTEST" parse --parser yaml "$work/erp-cluster.yaml" >"$work/erp-cluster.json"
python3 - "$work/erp-cluster.json" <<'PY'
import json, sys
docs = [d for d in json.load(open(sys.argv[1])) if d and d.get("kind") == "NetworkPolicy"]
want = [{"metadata": {"name": "erp-logging", "namespace": "logging"}, "spec": {
    "podSelector": {}, "policyTypes": ["Ingress", "Egress"], "egress": [
        {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
         "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]},
        {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "TCP", "port": 443}]}]}}]
if [{"metadata": {k: d["metadata"].get(k) for k in ("name", "namespace")}, "spec": d["spec"]} for d in docs] != want:
    sys.exit("helm-checks: erp-cluster's erp-logging policy must deny the logging namespace every ingress and every egress but DNS and 443 in the VPC: %s" % docs)
PY
echo "helm-checks: the logging namespace takes no connections and reaches DNS and the VPC's 443 alone"

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
mkdir "$work/rules"
python3 - "$work/erp-cell.json" "$work/temporal.yaml" "$work/rules/erp.rules.yaml" <<'PY'
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
        != [({"kubernetes.io/metadata.name": "monitoring"}, {"operator.prometheus.io/name": "erp-monitoring-prometheus"}, [9090])]:
    sys.exit("helm-checks: erp-cell's erp-server-metrics policy must admit the monitoring namespace's Prometheus to 9090 alone")
# The server sends its spans to the cell's collector (T54g), and may.
server = [d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "erp-server"][0]
env = {e["name"]: e.get("value") for c in server["spec"]["template"]["spec"]["containers"] for e in c.get("env", [])}
if env.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "http://erp-tracing.tracing.svc:4318/v1/traces":
    sys.exit("helm-checks: erp-cell's server must send its spans to the cell's collector, erp-tracing.tracing.svc:4318")
egress = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "erp-server"][0]["spec"]["egress"]
if not any([p["port"] for p in r["ports"]] == [4318] and r.get("to") == [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "tracing"}},
        "podSelector": {"matchLabels": {"app.kubernetes.io/name": "opentelemetry-collector", "app.kubernetes.io/instance": "opentelemetry-collector"}}}] for r in egress):
    sys.exit("helm-checks: erp-cell's erp-server policy must let the server reach the tracing namespace's collector on 4318")
# The SLO rules as the cell's Prometheus gets them, for promtool.
rules = [d for d in docs if d["kind"] == "PrometheusRule" and d["metadata"]["name"] == "erp-slo"]
if len(rules) != 1:
    sys.exit("helm-checks: erp-cell renders no erp-slo PrometheusRule")
json.dump(rules[0]["spec"], open(sys.argv[3], "w"))
# Each dashboard as the sidecar loads it: a ConfigMap of the file as it is.
import glob, os
want = {os.path.basename(f): open(f).read().rstrip("\n") for f in glob.glob("deploy/helm/erp-cell/dashboards/*.json")}
got = {k: v for d in docs if d["kind"] == "ConfigMap" and d["metadata"].get("labels", {}).get("grafana_dashboard") == "1" for k, v in d["data"].items()}
if not want or got != want:
    sys.exit("helm-checks: erp-cell's grafana_dashboard ConfigMaps must hold dashboards/*.json as they are: got %s, want %s" % (sorted(got), sorted(want)))
PY
# The SLOs and alerts (ADR-0043): the rendered rules pass promtool's
# checks and lint, and its unit tests run against them.
cp "$chart/rules/erp.rules.test.yaml" "$work/rules/"
"$PROMTOOL" check rules --lint=all --lint-fatal "$work/rules/erp.rules.yaml"
(cd "$work/rules" && "$PROMTOOL" test rules erp.rules.test.yaml)
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
required cell region trustDomain vpcCidr monitoringNamespace tracingNamespace server.otlpEndpoint trust.caBundle secrets.prefix \
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
bindings = {d["metadata"]["name"]: d for d in docs if d["kind"] == "TargetGroupBinding"}
if sorted(bindings) != ["erp-router", "erp-web"] or any("networking" in b["spec"] for b in bindings.values()):
    sys.exit("helm-checks: erp-global's TargetGroupBindings %s: want erp-router's and erp-web's, managing no security group rule (Terraform owns them)" % sorted(bindings))
gates = [g["conditionType"] for g in router["spec"]["template"]["spec"].get("readinessGates", [])]
if gates != ["target-health.elbv2.k8s.aws/erp-router"]:
    sys.exit("helm-checks: erp-global's router is ready before the load balancer finds it healthy (readiness gate %r)" % gates)
# The web workspace (ADR-0052): its own balancer's subnets alone, in
# production on its https hostname, reaching the router's public listener,
# the directory database (its sessions, over verified TLS) and the
# identity providers on 443, nothing else; its SPIFFE identity a client
# certificate for the router alone, which no cell accepts (T73c).
web = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "erp-web")
pod = web["spec"]["template"]["spec"]
env = {e["name"]: e.get("value") for e in pod["containers"][0]["env"]}
for name, value in {"ERP_ENV": "production", "ERP_WEB_PROXY_FROM": ",".join(want), "ERP_WEB_PUBLIC_URL": "https://app.dev.erp.example",
                    "ERP_WEB_API_URL": "https://api.dev.erp.example", "ERP_WEB_API_DIAL": "erp-router.erp-global.svc.cluster.local:8081",
                    "ERP_WEB_DIRECTORY_PASSWORD": "file:/etc/erp/secrets/directory-password", "ERP_WEB_SESSION_KEY": "file:/etc/erp/secrets/session-key",
                    "ERP_WEB_CLIENT_CREDENTIALS": "file:/etc/erp/web-clients/", "ERP_TLS_CERT": "file:/etc/erp/tls/tls.crt",
                    "ERP_TLS_KEY": "file:/etc/erp/tls/tls.key"}.items():
    if env.get(name) != value:
        sys.exit("helm-checks: erp-web's %s is %r, want %r" % (name, env.get(name), value))
if "sslmode=verify-full&sslrootcert=/etc/erp/trust/rds-ca.pem" not in (env.get("ERP_WEB_DIRECTORY_URL") or ""):
    sys.exit("helm-checks: erp-web's ERP_WEB_DIRECTORY_URL %r does not verify the directory's certificate" % env.get("ERP_WEB_DIRECTORY_URL"))
if [g["conditionType"] for g in pod.get("readinessGates", [])] != ["target-health.elbv2.k8s.aws/erp-web"]:
    sys.exit("helm-checks: erp-web is ready before its load balancer finds it healthy")
if pod.get("automountServiceAccountToken") is not False:
    sys.exit("helm-checks: erp-web mounts a service account token")
policy = next(d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "erp-web")
sources = [p["ipBlock"]["cidr"] for rule in policy["spec"]["ingress"] for p in rule["from"]]
if sources != want:
    sys.exit("helm-checks: erp-web's public listener takes %r, want its load balancer's subnets alone" % sources)
egress = [([p.get("podSelector", {}).get("matchLabels", {}).get("app.kubernetes.io/name") or p.get("ipBlock", {}).get("cidr") for p in rule.get("to", [])],
           [x["port"] for x in rule["ports"]]) for rule in policy["spec"]["egress"]]
outside = [p.get("ipBlock", {}).get("except") for p in policy["spec"]["egress"][2].get("to", [])]
if egress != [(["erp-router"], [8081]), (["10.16.0.0/16"], [5432]), (["0.0.0.0/0"], [443])] or outside != [["10.16.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]]:
    sys.exit("helm-checks: erp-web reaches %r, want the router's public listener, the directory in the VPC and the identity providers alone" % egress)
into = next(d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "erp-router-from-web")
if [p["podSelector"]["matchLabels"]["app.kubernetes.io/name"] for rule in into["spec"]["ingress"] for p in rule["from"]] != ["erp-web"]:
    sys.exit("helm-checks: the router's listener is opened to more than erp-web")
certs = [d for d in docs if d["kind"] == "Certificate" and "erp-web" in d["metadata"]["name"]]
if [(c["metadata"]["name"], c["spec"].get("uris"), sorted(c["spec"].get("usages", [])), c["spec"].get("dnsNames")) for c in certs] != \
        [("erp-web-tls", ["spiffe://dev.erp.example/global/erp-web"], ["client auth", "digital signature"], None)]:
    sys.exit("helm-checks: erp-web's certificates %r: want one, its SPIFFE identity as a client alone (ADR-0033)" % certs)
if "ERP_WEB_SESSION_KEY_PREVIOUS" in env:
    sys.exit("helm-checks: erp-web opens sessions under a previous key while no rotation runs")
PY
# During a rotation of the session key the previous one is mounted too.
"$HELM" template erp-global "$chart" --namespace erp-global -f "$values" --set web.sessionKeyPrevious=true >"$work/erp-global-rotating.yaml"
"$CONFTEST" parse --parser yaml "$work/erp-global-rotating.yaml" >"$work/erp-global-rotating.json"
python3 - "$work/erp-global-rotating.json" <<'PY'
import json, sys
docs = [d for d in json.load(open(sys.argv[1])) if d]
web = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "erp-web")
env = {e["name"]: e.get("value") for e in web["spec"]["template"]["spec"]["containers"][0]["env"]}
secret = next(d for d in docs if d["kind"] == "ExternalSecret" and d["metadata"]["name"] == "erp-web")
keys = {x["secretKey"]: x["remoteRef"]["key"] for x in secret["spec"]["data"]}
if env.get("ERP_WEB_SESSION_KEY_PREVIOUS") != "file:/etc/erp/secrets/session-key-previous" or keys.get("session-key-previous") != "erp/global/web-session-key-previous":
    sys.exit("helm-checks: a rotation's previous session key is not mounted (%r, %r)" % (env.get("ERP_WEB_SESSION_KEY_PREVIOUS"), keys))
PY
for value in trustDomain vpcCidr trust.caBundle secrets.prefix images.router.repository images.router.digest \
  router.targetGroupArn router.proxyFrom router.operatorIssuers directory.endpoint directory.masterSecretArn directory.caBundle \
  images.web.repository images.web.digest web.hostname web.apiUrl web.targetGroupArn web.proxyFrom; do
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
for bad in 'images.router.digest=latest' 'router.proxyFrom={}' 'router.proxyFrom={10.16.0.0}' 'images.web.digest=latest' \
  'web.proxyFrom={}' 'web.proxyFrom={10.16.0.0}' 'web.hostname=App_Workspace' 'web.apiUrl=http://api.dev.erp.example' 'web.apiUrl=https://api.dev.erp.example/v1'; do
  if "$HELM" template erp-global "$chart" --namespace erp-global -f "$values" --set "$bad" >/dev/null 2>&1; then
    echo "helm-checks: erp-global took $bad" >&2
    exit 1
  fi
done
echo "helm-checks: erp-global refuses to render without each value it requires, with an image not by digest, no load balancer subnets, or a web hostname or API address that is not one"
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
