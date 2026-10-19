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
#     and name that cell, and no image: release.json pins those;
#   - a release reaches production only after its rollback drill in
#     staging passed for that very commit: <drill cell>/drills/<version>.json
#     at BASE records it ({"release", "commit", "rolled_back_to", "result",
#     "run"}, committed by a pull request of its own, as path.json changes
#     are), so a version re-pinned to another commit needs a drill again.
# With OUT set it writes "<commit> <image ref>" lines there, which the
# deploy workflow verifies with scripts/verify-image.sh (signatures,
# attestations, provenance naming the commit) before the promotion merges;
# with CELL_OUT set, the promoted cell (<env>/<cell>), whose production
# promotions, rollbacks among them, need an approver's approval
# (scripts/promotion-approvals.sh).
#   scripts/promotion-checks.sh origin/main [HEAD]
set -euo pipefail
cd "$(dirname "$0")/.."
base="${1:?usage: promotion-checks.sh BASE [HEAD]}"
python3 - "$base" "${2:-HEAD}" "${OUT:-}" "${CELL_OUT:-}" <<'PY'
import json, re, subprocess, sys

base, head, out, cell_out = sys.argv[1:5]
root = "deploy/environments"
images = {"server": "erp-server", "router": "erp-router", "pgbouncer": "erp-pgbouncer", "nats": "erp-nats", "temporal": "erp-temporal"}

def fail(msg):
    sys.exit("promotion-checks: " + msg)

def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout

def at(rev, path):
    """The file at rev, or None where it does not exist."""
    r = subprocess.run(["git", "show", "%s:%s" % (rev, path)], capture_output=True, text=True)
    return r.stdout if r.returncode == 0 else None

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

def ran_before(path, rel):
    """Whether the cell ran rel before BASE's: a rollback."""
    for rev in git("log", "--format=%H", base, "--", path).split()[1:]:
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
others = [p for p in changed if p not in releases and p not in cells and p not in drills and p != root + "/path.json"]
if others:
    fail("only cells' release.json and cell.json, the drills' records and path.json live under %s, not %s" % (root, ", ".join(others)))
for p in cells:
    env, name = p[len(root) + 1:].split("/")[:2]
    d = load(at(head, p) or fail("%s is removed: a cell's values are replaced, never removed" % p), p)
    if not isinstance(d, dict) or d.get("environment") != env or d.get("cell") != name:
        fail('%s must be an object naming its cell: "environment": "%s", "cell": "%s"' % (p, env, name))
    if any(isinstance(d.get(c), dict) and "images" in d[c] for c in ("erpCell", "erpGlobal")):
        fail("%s pins no image: the cell's release.json does" % p)
    print("promotion-checks: %s changes the values of %s/%s" % (p, env, name))
if root + "/path.json" in changed:
    new = load(at(head, root + "/path.json") or fail("%s/path.json is removed" % root), "path.json")
    if not isinstance(new.get("path"), list) or not new["path"] or not all(isinstance(c, str) for c in new["path"]) \
            or new.get("drill") not in new["path"]:
        fail("path.json must name a non-empty path of cells and a drill cell on it")
for p in drills:
    text = at(head, p) or fail("%s is removed: a drill's record is kept" % p)
    d = load(text, p)
    name = p.rsplit("/", 1)[1][:-len(".json")]
    if set(d) != {"release", "commit", "rolled_back_to", "result", "run"} or d["release"] != name \
            or not re.fullmatch(r"[0-9a-f]{40}", str(d["commit"])) or d["result"] not in ("passed", "failed") \
            or not re.fullmatch(r"v\d+\.\d+\.\d+", str(d["rolled_back_to"])) or not str(d["run"]).startswith("https://"):
        fail('%s must be {"release": "%s", "commit": "<full commit>", "rolled_back_to": "vX.Y.Z", "result": "passed" or "failed", "run": "https://..."}' % (p, name))
    print("promotion-checks: %s records the drill of %s: %s" % (drill_cell, name, d["result"]))
if not releases:
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

if ran_before(target, rel):
    print("promotion-checks: %s rolls back to %s, which it ran before" % (cell, version))
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
    if env == "production":
        record = at(base, "%s/%s/drills/%s.json" % (root, drill_cell, version))
        drill = load(record, "drill") if record is not None else None
        if not drill or drill.get("result") != "passed" or drill.get("release") != version or drill.get("commit") != rel[1]:
            fail("%s (%s) reaches production only after its rollback drill in %s passed for that commit (%s/drills/%s.json)"
                 % (version, rel[1], drill_cell, drill_cell, version))
    print("promotion-checks: %s takes %s" % (cell, version))

if cell_out:
    with open(cell_out, "w") as f:
        f.write(cell + "\n")
if out:
    with open(out, "w") as f:
        for name in sorted(images):
            img = doc["images"][name]
            f.write("%s %s@%s\n" % (doc["release"]["commit"], img["repository"], img["digest"]))
PY
