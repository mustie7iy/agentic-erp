#!/usr/bin/env bash
# Tests restore-drill.sh (ADR-0040, T41e) without Docker, AWS or a
# cluster: one stand-in answers as docker, aws, kubectl and erp-server
# (by the name it is called as), logs every call, and plays erp-server's
# dr mark and dr verify, the verification failing or a restore job failing
# where a case says so. Each case runs the drill on a throwaway copy of the
# repository and checks its exit, its appended record, and that the side
# database and the drill's objects were removed.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0
mkdir -p "$work/bin" "$work/repo/scripts" "$work/repo/deploy/environments"
cp "$root/scripts/restore-drill.sh" "$work/repo/scripts/"
cp "$root/deploy/environments/path.json" "$work/repo/deploy/environments/"

cat >"$work/bin/fake" <<'PY'
#!/usr/bin/env python3
import base64, datetime, json, os, sys
name, a = os.path.basename(sys.argv[0]), sys.argv[1:]
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write("%s %s\n" % (name, " ".join(a)))
now = datetime.datetime.now(datetime.timezone.utc)
iso = lambda t: t.strftime("%Y-%m-%dT%H:%M:%S.%fZ")
state_file = os.environ["FAKE_STATE"]
state = json.load(open(state_file)) if os.path.exists(state_file) else {}

def erp(args):
    """erp-server dr mark|verify, as the real one prints them."""
    if args[:2] == ["dr", "mark"]:
        print(json.dumps({"tenant_id": args[args.index("-tenant") + 1], "name": args[args.index("-name") + 1], "at": iso(now),
                          "fingerprint": {"tables": []}}))
        return 0
    if args[:2] == ["dr", "verify"]:
        before = json.load(open(state.get("files", {}).get("before.json") or args[args.index("-restored") + 1]))
        problems = ["tenant x: audit chain broken at seq 3"] if os.environ.get("FAKE_VERIFY") == "fail" else []
        print(json.dumps({"schema_version": 77, "restored_marker": {"name": before["name"], "at": before["at"]}, "problems": problems}))
        return 1 if problems else 0
    return 0  # migrate

if name == "erp-server":
    sys.exit(erp(a))
if name == "docker":
    if a[0] == "exec" and "psql" in a:
        q = a[-1]
        print({"SELECT pg_walfile_name": "000000010000000000000003", "SELECT coalesce(last_archived_wal": "t",
               "SELECT pg_is_in_recovery": "f", "SELECT to_json(now())": iso(now)}.get(next((k for k in
               ["SELECT pg_walfile_name", "SELECT coalesce(last_archived_wal", "SELECT pg_is_in_recovery", "SELECT to_json(now())"] if q.startswith(k)), ""), ""))
    sys.exit(0)
if name == "aws":
    cmd = a[0:2]
    out = {}
    if cmd == ["rds", "describe-db-clusters"]:
        side = "drill" in a[a.index("--db-cluster-identifier") + 1]
        out = {"DBClusters": [{"Status": "available", "LatestRestorableTime": iso(now + datetime.timedelta(minutes=5)),
                               "Endpoint": "side.cluster.example", "Port": 5432,
                               "MasterUserSecret": {"SecretArn": "arn:secret:side", "SecretStatus": "active"} if side else {}}]}
    elif cmd == ["secretsmanager", "get-secret-value"]:
        out = {"SecretString": json.dumps({"username": "erp", "password": "p@ss/word"})}
    elif cmd == ["backup", "list-recovery-points-by-backup-vault"]:
        out = {"RecoveryPoints": [{"Status": "COMPLETED", "CreationDate": iso(now - datetime.timedelta(days=1)), "RecoveryPointArn": "arn:old"},
                                  {"Status": "COMPLETED", "CreationDate": iso(now), "RecoveryPointArn": "arn:copy"}]}
    elif cmd == ["backup", "start-restore-job"]:
        out = {"RestoreJobId": "job-1"}
    elif cmd == ["rds", "delete-db-cluster"] and os.environ.get("FAKE_DELETE") == "fail":
        sys.exit("InvalidParameterCombination: deletion protection is enabled")
    elif cmd == ["backup", "describe-restore-job"]:
        out = {"Status": os.environ.get("FAKE_RESTORE_JOB", "COMPLETED")}
    print(json.dumps(out))
    sys.exit(0)
if name == "kubectl":
    verb = a[2]
    if verb == "get" and a[3] == "deployment":
        print("erp-cell" if "instance" in a[-1] else "registry.example/erp-server@sha256:" + "1" * 64)
    elif verb == "get" and a[3] == "secret":
        print(base64.b64encode(b"postgres://migrator:x@writer.example:5432/erp?sslmode=verify-full&sslrootcert=/etc/erp/rds/ca.pem").decode())
    elif verb == "create":
        for f in [x.split("=", 1)[1] for x in a if x.startswith("--from-file=")]:
            key, path = (f.split("=", 1) if "=" in f else (os.path.basename(f), f))
            state.setdefault("files", {})[key] = path
            if key == "database-url":
                state["url"] = open(path).read()
    elif verb == "apply":
        pod = json.load(open(a[a.index("-f") + 1]))
        state.setdefault("pods", {})[pod["metadata"]["name"]] = pod["spec"]["containers"][0]["args"]
        # Selected by the chart's erp-restore-drill NetworkPolicy, or the default deny holds it.
        if pod["metadata"]["labels"] != {"app.kubernetes.io/name": "erp-restore-drill", "app.kubernetes.io/instance": "erp-cell"}:
            sys.exit("pod %s: labels %s match no NetworkPolicy" % (pod["metadata"]["name"], pod["metadata"]["labels"]))
    elif verb == "get" and a[3] == "pod":
        print("Succeeded")
    elif verb == "logs":
        import io, contextlib
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            code = erp(state["pods"][a[3]])
        state.setdefault("phases", {})[a[3]] = code
        print(buf.getvalue())
    json.dump(state, open(state_file, "w"))
    sys.exit(0)
PY
chmod +x "$work/bin/fake"
for c in docker aws kubectl erp-server; do ln -s fake "$work/bin/$c"; done
record="$work/repo/deploy/environments/staging/stg1/drills/restore.json"

# case NAME WANT_EXIT WANT_RESULT GREP_LOG ENV...: runs the drill with ENV;
# WANT_RESULT is the last run's result ("" when no run may be recorded),
# GREP_LOG what the calls' log must hold.
case_() {
  local name="$1" want="$2" result="$3" grep="$4" got
  shift 4
  : >"$work/log"
  rm -f "$work/state"
  if env FAKE_LOG="$work/log" FAKE_STATE="$work/state" PATH="$work/bin:$PATH" RESTORE_POLL=0 RESTORE_TIMEOUT=5 \
      ERP_SERVER="$work/bin/erp-server" DRILL_BY=ada "$@" "$work/repo/scripts/restore-drill.sh" https://ci.example/run/9 >"$work/out" 2>&1; then got=0; else got=1; fi
  local last=""
  [ -f "$record" ] && last="$(python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print(r[-1]["result"] if r else "")' "$record")"
  if [ "$got" != "$want" ] || [ "$last" != "$result" ] || { [ -n "$grep" ] && ! grep -q -- "$grep" "$work/log"; }; then
    echo "FAIL $name: got exit $got and result '$last', want $want and '$result' (log: $grep)" >&2
    cat "$work/out" "$work/log" >&2
    failed=1
  else
    echo "ok   $name"
  fi
}

case_ local-passes 0 passed "docker volume rm erp-restore-" RESTORE_PROVIDER=local
python3 - "$record" <<'PY' || failed=1
import json, sys
r = json.load(open(sys.argv[1]))[-1]
assert set(r) == {"kind", "provider", "by", "started", "restore_point", "rpo_seconds", "rto_seconds", "result", "run"}, r
assert r["provider"] == "local" and r["by"] == "ada" and r["rpo_seconds"] >= 1 and r["rto_seconds"] >= 0, r
print("ok   local-record")
PY
case_ local-verification-fails 1 failed "docker rm -f erp-restore-" RESTORE_PROVIDER=local FAKE_VERIFY=fail
case_ aws-point-in-time 0 passed "rds delete-db-cluster --db-cluster-identifier erp-stg1-drill-" RESTORE_PROVIDER=aws RESTORE_CLUSTER=erp-stg1 \
  DRILL_TENANT=0d5c7a1e-41d0-4e7b-9a0d-d2111e5e0099 RESTORE_ARGS="--db-subnet-group-name erp-stg1"
if ! grep -q "restore-db-cluster-to-point-in-time --source-db-cluster-identifier erp-stg1 .*--db-subnet-group-name erp-stg1" "$work/log" \
    || ! python3 -c 'import json,sys; u=json.load(open(sys.argv[1]))["url"]; assert u.startswith("postgres://erp:p%40ss%2Fword@side.cluster.example:5432/erp?sslmode=verify-full"), u' "$work/state" \
    || ! grep -q "kubectl -n erp delete secret restore-drill-.*-db" "$work/log"; then
  echo "FAIL aws-point-in-time: the side cluster's restore, URL or clean-up" >&2
  failed=1
else
  echo "ok   aws-side-database"
fi
case_ aws-dr-copies 0 passed "backup start-restore-job --region eu-central-2 --recovery-point-arn arn:copy" RESTORE_PROVIDER=aws RESTORE_KIND=dr-copies \
  RESTORE_CLUSTER=erp-stg1 DRILL_TENANT=0d5c7a1e-41d0-4e7b-9a0d-d2111e5e0099 RESTORE_DR_REGION=eu-central-2 RESTORE_DR_VAULT=erp-stg1-dr \
  RESTORE_ROLE_ARN=arn:role RESTORE_METADATA='{"DBClusterInstanceClass": "db.m6gd.large"}'
python3 - "$record" <<'PY' || failed=1
import json, sys
r = json.load(open(sys.argv[1]))
assert len(r) == 4 and [x["result"] for x in r] == ["passed", "failed", "passed", "passed"], r  # appended, never rewritten
assert r[-1]["kind"] == "dr-copies" and r[-1]["provider"] == "aws", r[-1]
print("ok   record-appended")
PY
case_ aws-restore-job-fails 1 failed "rds delete-db-cluster --region eu-central-2" RESTORE_PROVIDER=aws RESTORE_KIND=dr-copies FAKE_RESTORE_JOB=FAILED \
  RESTORE_CLUSTER=erp-stg1 DRILL_TENANT=0d5c7a1e-41d0-4e7b-9a0d-d2111e5e0099 RESTORE_DR_REGION=eu-central-2 RESTORE_DR_VAULT=erp-stg1-dr RESTORE_ROLE_ARN=arn:role
case_ aws-verification-fails 1 failed "rds delete-db-cluster" RESTORE_PROVIDER=aws RESTORE_CLUSTER=erp-stg1 \
  DRILL_TENANT=0d5c7a1e-41d0-4e7b-9a0d-d2111e5e0099 FAKE_VERIFY=fail
case_ aws-side-cluster-left 1 failed "rds delete-db-cluster" RESTORE_PROVIDER=aws RESTORE_CLUSTER=erp-stg1 \
  DRILL_TENANT=0d5c7a1e-41d0-4e7b-9a0d-d2111e5e0099 FAKE_DELETE=fail
grep -q "delete the side cluster erp-stg1-drill-.* by hand" "$work/out" || { echo "FAIL aws-side-cluster-left: not reported" >&2; failed=1; }
rm -f "$record"
case_ unknown-provider 1 "" "" RESTORE_PROVIDER=gcp
case_ local-dr-copies 1 "" "" RESTORE_PROVIDER=local RESTORE_KIND=dr-copies
case_ aws-without-cluster 1 "" "" RESTORE_PROVIDER=aws DRILL_TENANT=x
exit "$failed"
