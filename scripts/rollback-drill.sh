#!/usr/bin/env bash
# The staging rollback drill (ADR-0035, T40d5): before a release reaches
# production, the drill cell (deploy/environments/path.json "drill") runs
# it, is rolled back to the release before it, and must stay healthy:
#   1. deploy N, the release the drill cell ran before N+1;
#   2. deploy N+1, the release it runs now (its migrations run);
#   3. revert to N: N's code on N+1's schema, which expand, migrate,
#      contract (06 §6.4) makes safe.
# After each step every drilled Application (erp-cell, and erp-global on a
# home cell) synced that revision and is healthy, the deployments run that
# release's images by digest and finished rolling out, and SMOKE, when
# set, passes (a command, run with DRILL_STEP and DRILL_RELEASE set).
# A step is a sync of the cell's own Argo CD to the main commit that
# pinned that release, so nothing is pushed: the root Application's
# automated sync is paused for the drill and restored when it ends,
# whatever the outcome, which brings every Application back to the branch.
# It refuses a drill whose charts changed since N was pinned: production's
# rollback renders the branch's charts with N's images, the drill's N's.
# It appends the result, passed or failed, to
# <drill cell>/drills/<N+1>.json for the release's commit, which a pull
# request of its own then merges (promotion-checks.sh keeps records
# append-only). Run it from a checkout of the repository, with access to
# the drill cell's cluster:
#   scripts/rollback-drill.sh https://<where the drill's log is kept>
# KUBECTL names kubectl, BRANCH the branch Argo CD runs (origin/main),
# DRILL_TIMEOUT a step's limit in seconds (5400, Argo CD's sync limit),
# DRILL_POLL the seconds between looks (10).
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - "${1:?usage: rollback-drill.sh RUN_URL}" <<'PY'
import json, os, subprocess, sys, time

run_url = sys.argv[1]
kubectl = os.environ.get("KUBECTL", "kubectl")
branch = os.environ.get("BRANCH", "origin/main")
timeout = int(os.environ.get("DRILL_TIMEOUT", "5400"))
poll = float(os.environ.get("DRILL_POLL", "10"))
argocd = "argocd"
root = "deploy/environments"

def fail(msg):
    sys.exit("rollback-drill: " + msg)

def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout

def kube(*args, check=True):
    r = subprocess.run([kubectl, *args], capture_output=True, text=True)
    if check and r.returncode != 0:
        raise Step("%s %s: %s" % (kubectl, " ".join(args), r.stderr.strip()))
    return r

class Step(Exception):
    """A step of the drill that failed: the drill's result is failed."""

if not run_url.startswith("https://"):
    fail("RUN_URL %r is not an https:// address of the drill's log" % run_url)
cell = json.load(open(root + "/path.json"))["drill"]
path = "%s/%s/release.json" % (root, cell)
revs = git("log", "--format=%H", branch, "--", path).split()
if not revs:
    fail("%s has no %s" % (branch, path))
def release_at(rev):
    return json.loads(git("show", "%s:%s" % (rev, path)))
new_rev, new = revs[0], release_at(revs[0])
old_rev = next((r for r in revs[1:] if release_at(r)["release"] != new["release"]), None)
if old_rev is None:
    fail("%s ran no release before %s: nothing to roll back to" % (cell, new["release"]["version"]))
old = release_at(old_rev)
# A production rollback reverts release.json alone: the branch's charts
# with N's images. The drill's revert syncs N's commit, N's charts, so it
# drills that rollback only where the charts are the same.
charts = ["deploy/helm/erp-cell", "deploy/helm/erp-global"]
if git("diff", "--name-only", old_rev, branch, "--", *charts).strip():
    fail("the charts changed since %s pinned %s: reverting to its commit would not drill the rollback production does"
         % (old_rev[:12], old["release"]["version"]))

def application(name):
    r = kube("-n", argocd, "get", "application", name, "-o", "json", check=False)
    return json.loads(r.stdout) if r.returncode == 0 else None

# The drilled Applications and the deployment each must run, by image.
apps = [("erp-cell", "erp", "erp-server", "server")]
if application("erp-global") is not None:
    apps.append(("erp-global", "erp-global", "erp-router", "router"))
if application("erp-delivery") is None or application("erp-cell") is None:
    fail("no erp-delivery or erp-cell Application in %s: is this %s's cluster?" % (argocd, cell))

def patch(name, body):
    kube("-n", argocd, "patch", "application", name, "--type", "merge", "-p", json.dumps(body))

def wait(what, done):
    deadline = time.monotonic() + timeout
    while True:
        got = done()
        if got is True:
            return
        if got is not None:
            raise Step("%s: %s" % (what, got))
        if time.monotonic() > deadline:
            raise Step("%s: not done in %ds" % (what, timeout))
        time.sleep(poll)

def started(name):
    return ((application(name) or {}).get("status", {}).get("operationState") or {}).get("startedAt")

def synced(name, rev, before):
    """True once a sync of name to rev started after before succeeded and
    name is healthy; a message if that sync failed."""
    def done():
        st = (application(name) or {}).get("status", {})
        op = st.get("operationState") or {}
        if op.get("startedAt") == before or op.get("operation", {}).get("sync", {}).get("revision") != rev \
                or op.get("phase") in (None, "Running", "Terminating"):
            return None
        if op["phase"] != "Succeeded":
            return "sync %s: %s" % (op["phase"], op.get("message", ""))
        return True if st.get("health", {}).get("status") == "Healthy" else None
    return done

def step(label, rev, rel):
    version = rel["release"]["version"]
    print("rollback-drill: %s: %s at %s" % (label, version, rev))
    for name, namespace, deployment, image in apps:
        before = started(name)
        patch(name, {"operation": {"initiatedBy": {"username": "rollback-drill"}, "sync": {"revision": rev}}})
        wait("%s %s" % (label, name), synced(name, rev, before))
        d = json.loads(kube("-n", namespace, "get", "deployment", deployment, "-o", "json").stdout)
        want = "%s@%s" % (rel["images"][image]["repository"], rel["images"][image]["digest"])
        got = [c["image"] for c in d["spec"]["template"]["spec"]["containers"]]
        if want not in got:
            raise Step("%s: %s runs %s, not %s" % (label, deployment, ", ".join(got), want))
        kube("-n", namespace, "rollout", "status", "deployment/" + deployment, "--timeout=%ds" % timeout)
    smoke = os.environ.get("SMOKE")
    if smoke:
        r = subprocess.run(smoke, shell=True, env=dict(os.environ, DRILL_STEP=label, DRILL_RELEASE=version))
        if r.returncode != 0:
            raise Step("%s: the smoke tests failed (%s exited %d)" % (label, smoke, r.returncode))

# Paused from the root down: the root would sync the others back to the
# branch; restored from the root, whose self-heal restores the others.
result = "failed"
try:
    for name in ["erp-delivery"] + [a[0] for a in apps]:
        patch(name, {"spec": {"syncPolicy": {"automated": None}}})
    step("deploy N", old_rev, old)
    step("deploy N+1", new_rev, new)
    step("revert to N", old_rev, old)
    result = "passed"
except Step as e:
    print("rollback-drill: " + str(e), file=sys.stderr)
finally:
    try:
        patch("erp-delivery", {"spec": {"syncPolicy": {"automated": {"prune": True, "selfHeal": True}}}})
    except Step as e:
        result = "failed"
        print("rollback-drill: %s: restore it by hand" % e, file=sys.stderr)

record = "%s/%s/drills/%s.json" % (root, cell, new["release"]["version"])
runs = json.load(open(record)) if os.path.exists(record) else []
runs.append({"release": new["release"]["version"], "commit": new["release"]["commit"],
             "rolled_back_to": old["release"]["version"], "result": result, "run": run_url})
os.makedirs(os.path.dirname(record), exist_ok=True)
with open(record, "w") as f:
    json.dump(runs, f, indent=1)
    f.write("\n")
print("rollback-drill: %s %s (rolled back to %s); recorded in %s" % (new["release"]["version"], result, old["release"]["version"], record))
sys.exit(0 if result == "passed" else 1)
PY
