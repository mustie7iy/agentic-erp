#!/usr/bin/env bash
# Checks a promotion pull request (ADR-0035, T40d), HEAD (default HEAD),
# against BASE (the branch it merges into), without credentials. It reads
# HEAD's files as data only, so the promotion workflow runs BASE's copy of
# this script on the pull request's commit:
#   - it changes the release of one cell and no other file of the
#     repository: deploy/environments/<env>/<cell>/release.json, a values
#     file of the erp-cell and erp-global charts naming the release's
#     version, commit and image digests;
#   - that release is well formed: vX.Y.Z, a full commit, each of the
#     platform's images by digest from its own repository;
#   - it is the release the cell before it on deploy/environments/path.json
#     runs at BASE (dev takes any), every production cell after the canary
#     taking what the canary runs; or a release the cell ran before (a
#     rollback, which is a revert);
#   - a cell's own values, <env>/<cell>/cell.json (the erp-delivery chart's,
#     from its Terraform outputs), change in a pull request of their own
#     and name that cell, and hold only the cell's own values: no image
#     (release.json pins those), no repository or branch (the chart's),
#     no Argo CD settings and no migrations deadline (held under Argo
#     CD's sync limit by helm-checks.sh);
#   - a release reaches production only after its rollback drill in
#     staging passed for that very commit: <drill cell>/drills/<version>.json
#     at BASE lists the drill's runs ([{"release", "commit",
#     "rolled_back_to", "result", "run"}], scripts/rollback-drill.sh),
#     the last for that commit passed. Records are appended to by pull
#     requests of their own, as path.json changes are, never rewritten, so
#     a failed run stays failed; a version re-pinned to another commit
#     needs a drill again. A run added names a release and commit, and a
#     release rolled back to, that the drill cell ran at BASE;
#   - a release reaches production only while the drill cell's last passed
#     AWS restore drill (<drill cell>/drills/restore.json at BASE,
#     scripts/restore-drill.sh, ADR-0040) started within 90 days (NOW, an
#     ISO time, stands in for the clock in tests); a rollback is exempt,
#     being the way out of an incident. The record is append-only too, its
#     runs {"kind", "provider", "by", "started", "restore_point",
#     "rpo_seconds", "rto_seconds", "result", "run"}; a local run (weekly
#     CI) is recorded but does not count;
# With OUT set it writes "<commit> <image ref>" lines there, which the
# deploy workflow verifies with scripts/verify-image.sh (signatures,
# attestations, provenance naming the commit) before the promotion merges;
# with APPROVAL_OUT set, "production" there when the pull request needs an
# approver's approval (scripts/promotion-approvals.sh): it changes a
# production cell's release or values, or path.json, which orders every
# later promotion. Product owner: a production rollback to a known-good
# release, one the cell ran before whose drill passed for its commit,
# needs none; nor does a drill's record (appended to, checked above).
# What a cell ran is BASE's first-parent history, its merged states: a
# commit inside a merged pull request never ran anywhere.
#   scripts/promotion-checks.sh origin/main [HEAD]
set -euo pipefail
cd "$(dirname "$0")/.."
base="${1:?usage: promotion-checks.sh BASE [HEAD]}"
python3 - "$base" "${2:-HEAD}" "${OUT:-}" "${APPROVAL_OUT:-}" <<'PY'
import datetime, json, os, re, subprocess, sys

base, head, out, approval_out = sys.argv[1:5]
root = "deploy/environments"
images = {"server": "erp-server", "router": "erp-router", "web": "erp-web", "pgbouncer": "erp-pgbouncer", "nats": "erp-nats", "temporal": "erp-temporal", "analytics": "erp-analytics"}

def fail(msg):
    sys.exit("promotion-checks: " + msg)

def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout

def at(rev, path):
    """The file at rev, or None where it does not exist. Whether it exists
    is asked of ls-tree, so a failing git stops the checks rather than
    reading as a file never there (a drill's record as new, so rewritable)."""
    r = subprocess.run(["git", "ls-tree", "--name-only", rev, "--", path], capture_output=True, text=True)
    if r.returncode != 0:
        fail("git ls-tree %s -- %s: %s" % (rev, path, r.stderr.strip()))
    if r.stdout.strip() != path:
        return None
    r = subprocess.run(["git", "show", "%s:%s" % (rev, path)], capture_output=True, text=True)
    if r.returncode != 0:
        fail("git show %s:%s: %s" % (rev, path, r.stderr.strip()))
    return r.stdout

def load(text, path):
    try:
        return json.loads(text)
    except ValueError as e:
        fail("%s is not JSON: %s" % (path, e))

def release(path, doc):
    """The release a file names, checked: what promotion compares."""
    if set(doc) != {"release", "images"}:
        fail("%s must hold release and images only" % path)
    rel, imgs = doc["release"], doc["images"]
    if not isinstance(rel, dict) or set(rel) != {"version", "commit"}:
        fail("%s: release must name its version and commit" % path)
    if not re.fullmatch(r"v\d+\.\d+\.\d+", str(rel["version"])):
        fail("%s: version %r is not vX.Y.Z" % (path, rel["version"]))
    if not re.fullmatch(r"[0-9a-f]{40}", str(rel["commit"])):
        fail("%s: commit %r is not a full commit" % (path, rel["commit"]))
    if not isinstance(imgs, dict) or set(imgs) != set(images):
        fail("%s: images must be exactly %s" % (path, ", ".join(sorted(images))))
    digests = {}
    for name, repo in images.items():
        img = imgs[name]
        if not isinstance(img, dict) or set(img) != {"repository", "digest"}:
            fail("%s: images.%s must name its repository and digest" % (path, name))
        if not re.fullmatch(r"[a-z0-9.-]+(:[0-9]+)?/" + re.escape(repo), str(img["repository"])):
            fail("%s: images.%s must come from a registry's %s repository, not %r" % (path, name, repo, img["repository"]))
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", str(img["digest"])):
            fail("%s: images.%s.digest %r is not a sha256 digest" % (path, name, img["digest"]))
        digests[name] = img["digest"]
    # The registry differs by region (ECR replicas); the release does not.
    return (rel["version"], rel["commit"], tuple(sorted(digests.items())))

def ran_at_drill(version, commit=None):
    """Whether the drill cell's release.json named version (at commit,
    when given) at BASE or before: what a drill's record may name."""
    p = "%s/%s/release.json" % (root, drill_cell)
    for rev in git("log", "--first-parent", "--format=%H", base, "--", p).split():
        text = at(rev, p)
        d = load(text, p) if text is not None else {}
        r = d.get("release") if isinstance(d, dict) else None
        if isinstance(r, dict) and r.get("version") == version and commit in (None, r.get("commit")):
            return True
    return False

def drilled(version, commit):
    """Whether the last drill at BASE of version at commit passed."""
    record = at(base, "%s/%s/drills/%s.json" % (root, drill_cell, version))
    runs = load(record, "drill") if record is not None else []
    mine = [d for d in runs if isinstance(runs, list) and isinstance(d, dict) and d.get("release") == version and d.get("commit") == commit]
    return bool(mine) and mine[-1].get("result") == "passed"

RESTORE_KEYS = {"kind", "provider", "by", "started", "restore_point", "rpo_seconds", "rto_seconds", "result", "run"}
RESTORE_MAX_AGE = datetime.timedelta(days=90)

def when(text):
    """An ISO time in UTC (...Z), or None."""
    try:
        return datetime.datetime.fromisoformat(str(text).replace("Z", "+00:00")) if str(text).endswith("Z") else None
    except ValueError:
        return None

def now():
    return when(os.environ["NOW"]) if os.environ.get("NOW") else datetime.datetime.now(datetime.timezone.utc)

def restore_run_ok(d):
    """Whether d is a well-formed restore drill run, started in the past
    (a future time would count as fresh until it came)."""
    number = lambda v: isinstance(v, (int, float)) and not isinstance(v, bool) and v >= 0
    return isinstance(d, dict) and set(d) == RESTORE_KEYS and d["kind"] in ("point-in-time", "dr-copies") \
        and d["provider"] in ("aws", "local") and isinstance(d["by"], str) and d["by"] != "" and when(d["started"]) is not None \
        and when(d["started"]) <= now() \
        and d["result"] in ("passed", "failed") and str(d["run"]).startswith("https://") \
        and (d["result"] == "failed" or (when(d["restore_point"]) is not None and number(d["rpo_seconds"]) and number(d["rto_seconds"])))

def restored_lately():
    """When the last passed AWS restore drill at BASE started, and whether
    that is within RESTORE_MAX_AGE of now."""
    text = at(base, "%s/%s/drills/restore.json" % (root, drill_cell))
    runs = load(text, "restore.json") if text is not None else []
    passed = [when(d["started"]) for d in runs if isinstance(runs, list) and restore_run_ok(d) and d["provider"] == "aws" and d["result"] == "passed"]
    last = max(passed) if passed else None
    return last, last is not None and now() - last <= RESTORE_MAX_AGE

def ran_before(path, rel):
    """Whether the cell ran rel before BASE's: a rollback."""
    for rev in git("log", "--first-parent", "--format=%H", base, "--", path).split()[1:]:
        text = at(rev, path)
        if text is not None and release(path, load(text, path)) == rel:
            return True
    return False

order = load(at(base, root + "/path.json") or fail("no %s/path.json at %s" % (root, base)), "path.json")
path, drill_cell = order["path"], order["drill"]
# The whole tree: a promotion that also changed a script or a workflow
# would otherwise land it on BASE unreviewed by these checks.
# No rename detection: a rename is a removal too.
everything = git("diff", "--no-renames", "--name-only", "%s...%s" % (base, head)).split()
changed = [p for p in everything if p.startswith(root + "/")]
releases = [p for p in changed if re.fullmatch(root + r"/[a-z]+/[a-z][a-z0-9]{1,19}/release\.json", p)]
cells = [p for p in changed if re.fullmatch(root + r"/[a-z]+/[a-z][a-z0-9]{1,19}/cell\.json", p)]
drills = [p for p in changed if re.fullmatch(re.escape("%s/%s/drills/" % (root, drill_cell)) + r"v\d+\.\d+\.\d+\.json", p)]
restores = [p for p in changed if p == "%s/%s/drills/restore.json" % (root, drill_cell)]
others = [p for p in changed if p not in releases and p not in cells and p not in drills and p not in restores and p != root + "/path.json"]
if others:
    fail("only cells' release.json and cell.json, the drills' records and path.json live under %s, not %s" % (root, ", ".join(others)))
for p in cells:
    env, name = p[len(root) + 1:].split("/")[:2]
    d = load(at(head, p) or fail("%s is removed: a cell's values are replaced, never removed" % p), p)
    if not isinstance(d, dict) or d.get("environment") != env or d.get("cell") != name:
        fail('%s must be an object naming its cell: "environment": "%s", "cell": "%s"' % (p, env, name))
    extra = set(d) - {"_comment", "environment", "cell", "home", "addons", "erpCluster", "erpCell", "erpGlobal"}
    if extra:
        fail("%s holds only the cell's own values, not %s" % (p, ", ".join(sorted(extra))))
    if not all(isinstance(d.get(c, {}), dict) for c in ("addons", "erpCluster", "erpCell", "erpGlobal")):
        fail("%s: addons, erpCluster, erpCell and erpGlobal are objects" % p)
    if any("images" in d.get(c, {}) for c in ("erpCell", "erpGlobal")):
        fail("%s pins no image: the cell's release.json does" % p)
    if "argo-cd" in d.get("addons", {}) or "migrations" in d.get("erpCell", {}):
        fail("%s sets neither Argo CD nor the migrations deadline: the charts' own values hold them" % p)
    print("promotion-checks: %s changes the values of %s/%s" % (p, env, name))
if root + "/path.json" in changed:
    new = load(at(head, root + "/path.json") or fail("%s/path.json is removed" % root), "path.json")
    if not isinstance(new.get("path"), list) or not new["path"] or not all(isinstance(c, str) for c in new["path"]) \
            or new.get("drill") not in new["path"]:
        fail("path.json must name a non-empty path of cells and a drill cell on it")
for p in drills:
    runs = load(at(head, p) or fail("%s is removed: a drill's record is kept" % p), p)
    name = p.rsplit("/", 1)[1][:-len(".json")]
    before = at(base, p)
    kept = load(before, p) if before is not None else []
    if not isinstance(runs, list) or runs[:len(kept)] != kept or len(runs) == len(kept):
        fail("%s is append-only: a pull request adds runs to it, never changes or removes one" % p)
    for d in runs[len(kept):]:
        if not isinstance(d, dict) or set(d) != {"release", "commit", "rolled_back_to", "result", "run"} or d["release"] != name \
                or not re.fullmatch(r"[0-9a-f]{40}", str(d["commit"])) or d["result"] not in ("passed", "failed") \
                or not re.fullmatch(r"v\d+\.\d+\.\d+", str(d["rolled_back_to"])) or not str(d["run"]).startswith("https://"):
            fail('%s: each run must be {"release": "%s", "commit": "<full commit>", "rolled_back_to": "vX.Y.Z", "result": "passed" or "failed", "run": "https://..."}' % (p, name))
        if not ran_at_drill(d["release"], d["commit"]) or not ran_at_drill(d["rolled_back_to"]):
            fail("%s: %s never ran %s at %s, or %s: a record names what the drill ran" % (p, drill_cell, name, d["commit"], d["rolled_back_to"]))
        print("promotion-checks: %s records a drill of %s: %s" % (drill_cell, name, d["result"]))
for p in restores:
    runs = load(at(head, p) or fail("%s is removed: the restore drills' record is kept" % p), p)
    before = at(base, p)
    kept = load(before, p) if before is not None else []
    if not isinstance(runs, list) or runs[:len(kept)] != kept or len(runs) == len(kept):
        fail("%s is append-only: a pull request adds runs to it, never changes or removes one" % p)
    for d in runs[len(kept):]:
        if not restore_run_ok(d):
            fail('%s: each run must be {"kind": "point-in-time" or "dr-copies", "provider": "aws" or "local", "by": "<who>", '
                 '"started": "<UTC time>Z", "restore_point": "<UTC time>Z", "rpo_seconds": n, "rto_seconds": n, '
                 '"result": "passed" or "failed", "run": "https://..."} (a failed run may lack the point and the measures)' % p)
        print("promotion-checks: %s records a %s restore drill (%s): %s" % (drill_cell, d["kind"], d["provider"], d["result"]))
production = root + "/path.json" in changed or any(p.startswith(root + "/production/") for p in releases + cells)

def approval(known_good):
    """Writes whether an approver must approve: see the header."""
    if approval_out:
        with open(approval_out, "w") as f:
            if production and not known_good:
                f.write("production\n")

if not releases:
    approval(False)
    print("promotion-checks: no release changes")
    sys.exit()
if len(releases) != 1 or len(releases) != len(everything):
    fail("a promotion changes one cell's release.json and nothing else, not %s" % ", ".join(everything))
target = releases[0]
cell = target[len(root) + 1:-len("/release.json")]
text = at(head, target) or fail("%s is removed: a cell's release is replaced, never removed" % target)
doc = load(text, target)
rel = release(target, doc)
version = rel[0]
known_good = False

if ran_before(target, rel):
    print("promotion-checks: %s rolls back to %s, which it ran before" % (cell, version))
    known_good = production and drilled(version, rel[1])
    if production and known_good:
        print("promotion-checks: %s's drill passed for %s: a known-good rollback, no approver needed" % (version, rel[1]))
else:
    env = cell.split("/")[0]
    if cell in path:
        before = path[path.index(cell) - 1] if path.index(cell) > 0 else None
    elif env == "production":
        before = path[-1]
    else:
        fail("%s is not on %s/path.json" % (cell, root))
    if before is not None:
        prev = at(base, "%s/%s/release.json" % (root, before))
        if prev is None or release(before, load(prev, before)) != rel:
            fail("%s may take only the release %s runs, not %s" % (cell, before, version))
    if env == "production" and not drilled(version, rel[1]):
        fail("%s (%s) reaches production only after its rollback drill in %s passed for that commit (%s/drills/%s.json)"
             % (version, rel[1], drill_cell, drill_cell, version))
    if env == "production":
        last, fresh = restored_lately()
        if not fresh:
            fail("%s reaches production only while %s's last passed restore drill on AWS is at most 90 days old (%s/drills/restore.json): %s"
                 % (version, drill_cell, drill_cell, "the last started %s" % last.strftime("%Y-%m-%d") if last else "there is none"))
    print("promotion-checks: %s takes %s" % (cell, version))
approval(known_good)

if out:
    with open(out, "w") as f:
        for name in sorted(images):
            img = doc["images"][name]
            f.write("%s %s@%s\n" % (doc["release"]["commit"], img["repository"], img["digest"]))
PY
