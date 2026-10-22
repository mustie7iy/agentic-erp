#!/usr/bin/env bash
# Tests rollback-drill.sh (ADR-0035, T40d5) without a cluster: a stand-in
# kubectl keeps the cell's Applications and deployments in a file, syncs
# an Application to a revision by deploying the images its values or else
# that revision's release.json pin, and fails where a case tells it to. Each case runs
# the drill on a throwaway repository and checks its result, its record,
# the order of its syncs and that the root's automated sync is restored.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

cat >"$work/kubectl" <<'PY'
#!/usr/bin/env python3
# kubectl -n NS get application|deployment NAME -o json
# kubectl -n NS patch application NAME --type merge -p BODY
# kubectl -n NS rollout status deployment/NAME --timeout=Ns
import json, os, subprocess, sys
state_file, a = os.environ["FAKE_STATE"], sys.argv[1:]
state = json.load(open(state_file))
ns, verb = a[1], a[2]
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write(" ".join(a[2:5]) + (" " + a[-1] if verb == "patch" else "") + "\n")
def save():
    json.dump(state, open(state_file, "w"))
def merge(dst, src):
    for k, v in src.items():
        if v is None:
            dst.pop(k, None)
        elif isinstance(v, dict):
            # As a JSON merge patch: a null removes, even in a new object.
            if not isinstance(dst.get(k), dict):
                dst[k] = {}
            merge(dst[k], v)
        else:
            dst[k] = v
if verb == "get":
    obj = state[a[3]].get(a[4])
    if obj is None:
        sys.exit("not found")
    print(json.dumps(obj))
elif verb == "patch":
    app, body = state["application"][a[4]], json.loads(a[-1])
    rev = body.get("operation", {}).get("sync", {}).get("revision")
    if rev is None:
        merge(app, body)
    else:
        state["n"] += 1
        phase = "Failed" if os.environ.get("FAIL_SYNC") == "%s@%d" % (a[4], state["n"]) else "Succeeded"
        app["status"] = {"operationState": {"phase": phase, "startedAt": str(state["n"]), "message": "boom",
                                            "operation": {"sync": {"revision": rev}}}, "health": {"status": "Healthy"}}
        # The Application's values take precedence over the revision's release.json.
        rel = app["spec"].get("source", {}).get("helm", {}).get("valuesObject") or \
            json.loads(subprocess.run(["git", "show", rev + ":deploy/environments/staging/stg1/release.json"],
                                      capture_output=True, text=True, check=True).stdout)
        runs = {"erp-cell": [("server", "erp-server")], "erp-global": [("router", "erp-router"), ("web", "erp-web")]}[a[4]]
        for image, deployment in runs:
            if image not in rel["images"]:
                state["deployment"].pop(deployment, None)  # a release from before it has none
                continue
            i = rel["images"][image]
            ref = "%s@%s" % (i["repository"], i["digest"])
            if os.environ.get("STALE_IMAGE") == str(state["n"]) and image != "web" or os.environ.get("STALE_WEB") == str(state["n"]) and image == "web":
                ref += "0"
            state["deployment"][deployment] = {"spec": {"template": {"spec": {"containers": [{"image": ref}]}}}}
    save()
elif verb == "rollout":
    pass
PY
chmod +x "$work/kubectl"

# release VERSION DIGIT: a release.json whose commit and digests repeat DIGIT.
release() {
  local d c
  d="$(printf "%064d" 0 | tr 0 "$2")" c="$(printf "%040d" 0 | tr 0 "$2")"
  printf '{"release": {"version": "%s", "commit": "%s"}, "images": {' "$1" "$c"
  local images="server router pgbouncer nats analytics"
  # NO_WEB: v1.0.0 is a release from before the web workspace.
  if [ -z "${NO_WEB:-}" ] || [ "$1" != v1.0.0 ]; then images="$images web"; fi
  for i in $images; do
    printf '"%s": {"repository": "r.example/erp-%s", "digest": "sha256:%s"}, ' "$i" "$i" "$d"
  done
  printf '"temporal": {"repository": "r.example/erp-temporal", "digest": "sha256:%s"}}}\n' "$d"
}
app() { printf '"%s": {"spec": {"syncPolicy": {"automated": {"prune": true, "selfHeal": true}}}, "status": {}}' "$1"; }

# case NAME WANT GREP [HOME] [STEPS]: the drill of v1.1.0 (after v1.0.0)
# on a fresh repository, with erp-global when HOME is set; WANT is passed,
# failed or refused, GREP what its output must say.
case_() {
  local name="$1" want="$2" grep="$3" home="${4:-}" got repo="$work/$1"
  mkdir -p "$repo/scripts" "$repo/deploy/environments/staging/stg1"
  cp "$root/scripts/rollback-drill.sh" "$repo/scripts/"
  cp "$root/deploy/environments/path.json" "$repo/deploy/environments/"
  git -C "$repo" init -q -b main
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name test
  release v1.0.0 1 >"$repo/deploy/environments/staging/stg1/release.json"
  git -C "$repo" add . && git -C "$repo" commit -q -m v1.0.0
  if [ "${ONE_RELEASE:-}" != 1 ]; then
    release v1.1.0 2 >"$repo/deploy/environments/staging/stg1/release.json"
    git -C "$repo" commit -q -am v1.1.0
  fi
  printf '{"n": 0, "deployment": {}, "application": {%s, %s%s}}' "$(app erp-delivery)" "$(app erp-cell)" \
    "${home:+, $(app erp-global)}" >"$work/state.json"
  : >"$work/log"
  if (cd "$repo" && eval "${PRE:-:}" && KUBECTL="$work/kubectl" BRANCH=main DRILL_POLL=0 DRILL_TIMEOUT=5 \
    FAKE_STATE="$work/state.json" FAKE_LOG="$work/log" ./scripts/rollback-drill.sh "${RUN:-https://ci.example/drill/1}") \
    >"$work/out" 2>&1; then got=passed; else got=failed; fi
  record="$repo/deploy/environments/staging/stg1/drills/v1.1.0.json"
  if [ "$got" = failed ] && [ ! -e "$record" ]; then got=refused; fi
  local why=""
  if [ "$got" != "$want" ]; then why="got $got, want $want"
  elif [ -n "$grep" ] && ! grep -q "$grep" "$work/out"; then why="output lacks $grep"
  elif [ "$got" != refused ]; then
    if ! python3 - "$record" "$want" <<'CHECK'; then why="its record is not the last run's"; fi
import json, sys
runs = json.load(open(sys.argv[1]))
want = {"release": "v1.1.0", "commit": "2" * 40, "rolled_back_to": "v1.0.0", "result": sys.argv[2], "run": "https://ci.example/drill/1"}
sys.exit(runs[-1] != want)
CHECK
    if [ -z "$why" ] && [ "$(tail -1 "$work/log")" != 'patch application erp-delivery {"spec": {"syncPolicy": {"automated": {"prune": true, "selfHeal": true}}}}' ]; then
      why="the root's automated sync was not restored last"
    fi
    if [ -z "$why" ] && ! python3 -c 'import json, sys
apps = json.load(open(sys.argv[1]))["application"].values()
sys.exit(any({"release", "images"} & set(a["spec"].get("source", {}).get("helm", {}).get("valuesObject", {})) for a in apps))' "$work/state.json"; then
      why="the drill left a release or images in an Application's values"
    fi
  fi
  if [ -z "$why" ] && [ -n "${REVS:-}" ] && [ "$(grep -o '"revision": "[0-9a-f]*"' "$work/log" | cut -d'"' -f4 | sort -u | wc -l | tr -d ' ')" != "$REVS" ]; then
    why="it synced $(grep -o '"revision": "[0-9a-f]*"' "$work/log" | sort -u | wc -l | tr -d ' ') revisions, want $REVS"
  fi
  if [ -z "$why" ] && [ -n "${SYNCS:-}" ] && [ "$(grep -o '"revision": "[0-9a-f]*"' "$work/log" | wc -l | tr -d ' ')" != "$SYNCS" ]; then
    why="it synced $(grep -c '"revision"' "$work/log") times, want $SYNCS"
  fi
  if [ -n "$why" ]; then
    echo "FAIL $name: $why" >&2
    cat "$work/out" >&2
    failed=1
  else
    echo "ok   $name"
  fi
}

SYNCS=3 case_ passes passed "v1.1.0 passed (rolled back to v1.0.0)"
SYNCS=6 case_ home-cell passed "" home
# The web workspace is checked on a home cell too (T73b), and is not looked
# for in a release from before it (NO_WEB: v1.0.0 has no web image).
STALE_WEB=4 SYNCS=4 case_ home-cell-stale-web failed "deploy N+1: erp-web runs" home
NO_WEB=1 SYNCS=6 case_ home-cell-before-the-web passed "" home
FAIL_SYNC=erp-cell@2 SYNCS=2 case_ failed-upgrade failed "deploy N+1 erp-cell: sync Failed"
STALE_IMAGE=3 case_ stale-image failed "revert to N: erp-server runs"
SMOKE='test "$DRILL_STEP" != "revert to N"' case_ smoke-fails failed "the smoke tests failed"
SMOKE='test "$DRILL_RELEASE" = v1.0.0 -o "$DRILL_RELEASE" = v1.1.0' case_ smoke-passes passed ""
PRE='mkdir -p deploy/environments/staging/stg1/drills && echo "[{\"release\": \"v1.1.0\", \"result\": \"failed\"}]" >deploy/environments/staging/stg1/drills/v1.1.0.json' \
  case_ appends passed ""
if [ "$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$work/appends/deploy/environments/staging/stg1/drills/v1.1.0.json")" != 2 ]; then
  echo "FAIL appends: the earlier run is not kept" >&2
  failed=1
fi
ONE_RELEASE=1 case_ nothing-to-roll-back-to refused "nothing to roll back to"
PRE='mkdir -p deploy/helm/erp-cell && echo x >deploy/helm/erp-cell/Chart.yaml && git add . && git commit -q -m chart' \
  case_ charts-changed refused "the charts changed"
# Across the chart change: every step on the branch's commit, N's images by values.
PRE='mkdir -p deploy/helm/erp-cell && echo x >deploy/helm/erp-cell/Chart.yaml && git add . && git commit -q -m chart' \
  DRILL_CHARTS=current SYNCS=3 REVS=1 case_ current-charts passed "revert to N: v1.0.0 at .* (the current charts)"
PRE='mkdir -p deploy/helm/erp-cell && echo x >deploy/helm/erp-cell/Chart.yaml && git add . && git commit -q -m chart' \
  DRILL_CHARTS=current SYNCS=6 case_ current-charts-home passed "" home
DRILL_CHARTS=current STALE_IMAGE=3 case_ current-charts-stale-image failed "revert to N: erp-server runs"
DRILL_CHARTS=both case_ unknown-mode refused "neither n nor current"
RUN=http://ci.example/1 case_ not-https refused "not an https"
exit "$failed"
