#!/usr/bin/env bash
# Tests the image pipeline's scripts (ADR-0032) with fake ko, docker, syft,
# crane, grype and cosign that write what the real ones would and log their
# calls; nothing is built, pulled or pushed:
#   - images.sh builds erp-temporal with ko from its own module (temporal/),
#     on .ko.yaml's base and platforms, listed after the four and before
#     erp-analytics, an SBOM per image, whether ko lists it pushed
#     (repo/name@digest) or built locally on one platform
#     (repo/name:tag@digest, ci-local's gates), and refuses a ko run
#     that lists no erp-temporal image;
#   - images.sh builds erp-analytics with buildx one platform at a time,
#     dated at the epoch, without buildx attestations, joins the platforms
#     into one index listed in images.txt after ko's images, writes an SBOM
#     per image whose first package is its digest, runs ko one build at a
#     time (each of three compiles api/gen in about 3 GB), honours KO_PLATFORMS,
#     pushes the index with crane under each tag, and refuses an archive
#     that is not one image of the platform asked, a platform that is not
#     one and a push whose digest is not the index built;
#   - images-reproducible.sh gives each build a buildx builder of its own,
#     removes it, and fails when the digests differ;
#   - image-scan.sh scans erp-analytics's SBOMs and its cc base, and each
#     upstream image of deploy/images/upstream.txt by its pin on
#     linux/arm64, failing on a fixable high unless .grype.yaml accepts it;
#   - sign-images.sh signs and attests every erp-analytics reference;
#   - base-image-current.sh checks the three pins and names the moved one;
#   - the release pushes every image it builds or copies into the release
#     registry (ERP_RELEASE_REGISTRY, tooling's ECR) and nowhere else, each
#     into a repository modules/ecr creates, under the names a cell's
#     release.json must use (promotion-checks.sh), and every image
#     release.json names is one the release builds or copies;
#   - the release scans (make image-scan, upstream pins included) before
#     it pushes, copies, signs or records anything.
set -euo pipefail
cd "$(dirname "$0")/.."
root="$PWD"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
export CALLS="$work/calls"

fake() { printf '#!/usr/bin/env bash\nset -euo pipefail\necho "%s $*" >>"$CALLS"\n%s\n' "$1" "$2" >"$work/bin/$1"; chmod +x "$work/bin/$1"; }
# ko: each binary's index and per-platform images, with ko's SBOM names;
# where it ran and its configuration, when one was given, are kept. As ko
# v0.19.1 does, one platform builds one image and no index, listed under
# the first tag as well as its digest (repo/name:tag@sha256:...) when it is
# not pushed (ko's no-op publisher: the local listing the reproducibility
# gate and the scan read) or is pushed under one tag other than latest;
# an index's images are listed by digest alone. FAKE_UNLISTED names a
# binary whose image ko does not list.
fake ko 'refs="" sboms="" platforms="linux/amd64,linux/arm64" binaries=() push=false tags=latest
for a in "$@"; do case "$a" in --image-refs=*) refs="${a#*=}" ;; --sbom-dir=*) sboms="${a#*=}" ;; --platform=*) platforms="${a#*=}" ;; --push=*) push="${a#*=}" ;; --tags=*) tags="${a#*=}" ;; ./cmd/*) binaries+=("${a#./cmd/}") ;; esac; done
echo "ko-ran-in $PWD" >>"$CALLS"
[ -z "${KO_CONFIG_PATH:-}" ] || cp "$KO_CONFIG_PATH" "$CALLS.${binaries[0]}.ko.yaml"
[ -z "${FAKE_UNLISTED:-}" ] || [ "${binaries[0]}" != "$FAKE_UNLISTED" ] || refs=/dev/null
for b in "${binaries[@]}"; do
  IFS=, read -r -a ps <<<"$platforms"
  if [ "${#ps[@]}" -eq 1 ]; then
    d="sha256:$(printf "%s %s" "$b" "${ps[0]}" | shasum -a 256 | cut -c1-64)"
    if [ "$push" != true ] || { [ "${tags#*,}" = "$tags" ] && [ "$tags" != latest ]; }; then
      echo "$KO_DOCKER_REPO/$b:${tags%%,*}@$d" >>"$refs"
    else
      echo "$KO_DOCKER_REPO/$b@$d" >>"$refs"
    fi
    printf "{\"packages\": [{\"name\": \"%s\"}]}" "$d" >"$sboms/$b-${ps[0]//\//-}.spdx.json"
    continue
  fi
  d="sha256:$(printf "%s" "$b" | shasum -a 256 | cut -c1-64)"
  echo "$KO_DOCKER_REPO/$b@$d" >>"$refs"; printf "{\"packages\": [{\"name\": \"%s\"}]}" "$d" >"$sboms/$b-index.spdx.json"
  for p in "${ps[@]}"; do
    d="sha256:$(printf "%s %s" "$b" "$p" | shasum -a 256 | cut -c1-64)"
    echo "$KO_DOCKER_REPO/$b@$d" >>"$refs"; printf "{\"packages\": [{\"name\": \"%s\"}]}" "$d" >"$sboms/$b-${p//\//-}.spdx.json"
  done
done'
# docker buildx: an OCI archive of one image for the platform asked;
# FAKE_DRIFT varies it from build to build, FAKE_INDEX makes it an index.
fake docker '[ "$2" = build ] || exit 0
platform="" dest=""
while [ $# -gt 0 ]; do case "$1" in --platform) platform="$2"; shift ;; --output) dest="${2#*dest=}"; dest="${dest%%,*}"; shift ;; esac; shift; done
python3 - "$platform" "$dest" <<PY
import hashlib, io, json, os, sys, tarfile
platform, dest = sys.argv[1:]
blobs = {}
def blob(doc):
    raw = json.dumps(doc).encode(); d = "sha256:" + hashlib.sha256(raw).hexdigest(); blobs[d] = raw
    return {"digest": d, "size": len(raw)}
config = blob({"os": platform.split("/")[0], "architecture": os.environ.get("FAKE_ARCH") or platform.split("/")[1], "drift": os.environ.get("FAKE_DRIFT") and os.urandom(8).hex()})
manifest = blob({"schemaVersion": 2, "config": config, "layers": []})
kind = "application/vnd.oci.image.index.v1+json" if os.environ.get("FAKE_INDEX") else "application/vnd.oci.image.manifest.v1+json"
index = json.dumps({"schemaVersion": 2, "manifests": [dict(manifest, mediaType=kind)]}).encode()
with tarfile.open(dest, "w") as tar:
    for name, raw in [("index.json", index)] + [("blobs/sha256/" + d.split(":")[1], r) for d, r in blobs.items()]:
        info = tarfile.TarInfo(name); info.size = len(raw); tar.addfile(info, io.BytesIO(raw))
PY'
fake syft 'out="${!#}"; printf "{\"packages\": [{\"name\": \"duckdb\", \"primaryPackagePurpose\": \"LIBRARY\"}, {\"name\": \"image\", \"primaryPackagePurpose\": \"CONTAINER\"}]}" >"${out#spdx-json=}"'
# crane: push prints the index pushed (FAKE_PUSHED replaces its digest);
# digest prints the digest pinned for a tag unless FAKE_MOVED names it.
fake crane 'case "$1" in
push) ref="$4"; d="sha256:$(shasum -a 256 "$3/index.json" | cut -c1-64)"; echo "${ref%:*}@${FAKE_PUSHED:-$d}" ;;
digest) if [ "$2" = "${FAKE_MOVED:-}" ]; then echo "sha256:$(printf "%064d" 9)"; else grep -h -o "$2@sha256:[0-9a-f]*" .ko.yaml deploy/images/erp-analytics.Dockerfile | head -1 | sed "s/.*@//"; fi ;;
esac'
# grype: FAKE_FINDING="WHAT CVE" reports CVE, fixable and high, in each
# image whose reference contains WHAT, and fails as grype's --fail-on does
# unless the --config given ignores CVE (.grype.yaml's exceptions).
fake grype 'config=""
while [ $# -gt 0 ]; do case "$1" in --config) config="$2"; shift ;; esac; shift; done
read -r what cve <<<"${FAKE_FINDING:-}"
[ -n "$what" ] && grep -q "^grype registry:[^ ]*$what" <(tail -1 "$CALLS") || exit 0
if [ -n "$config" ] && grep -q "^ *- vulnerability: $cve$" "$config"; then exit 0; fi
echo "$cve High (fixed) in $what" >&2
echo "discovered vulnerabilities at or above the severity threshold" >&2
exit 1'
fake cosign ''
export KO="$work/bin/ko" DOCKER="$work/bin/docker" SYFT="$work/bin/syft" CRANE="$work/bin/crane" GRYPE="$work/bin/grype" COSIGN="$work/bin/cosign"

failed=0
fail() {
  echo "FAIL $1: $2" >&2
  failed=1
}
calls() { grep -c "$1" "$CALLS" || true; }
images() { : >"$CALLS"; "$root/scripts/images.sh" "$@" >"$work/log" 2>&1; }

# Both platforms: one buildx build each, then the index and its images.
images "$work/out" || { cat "$work/log" >&2; fail both-platforms "images.sh failed"; }
[ "$(calls "^docker buildx build")" -eq 2 ] || fail one-platform-a-build "$(calls "^docker buildx build") builds, want 2"
grep "^docker buildx build" "$CALLS" | grep -v -- "--platform linux/[a-z0-9]* --provenance=false --sbom=false --build-arg SOURCE_DATE_EPOCH=0 " &&
  fail build-flags "a build not of one platform, dated at the epoch, without attestations"
analytics=()
while IFS= read -r ref; do analytics+=("$ref"); done < <(grep "/erp-analytics@" "$work/out/images.txt")
[ "${#analytics[@]}" -eq 3 ] || fail listed "${#analytics[@]} erp-analytics references, want the index and two images"
[ "$(head -15 "$work/out/images.txt" | grep -c "/erp-analytics@")" -eq 0 ] || fail order "erp-analytics before ko's images"
first() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["packages"][0]["name"])' "$1"; }
[ "$(first "$work/out/sbom/erp-analytics-index.spdx.json")" = "${analytics[0]##*@}" ] || fail index-sbom "the index's SBOM does not name it first"
for p in amd64 arm64; do
  sbom="$work/out/sbom/erp-analytics-linux-$p.spdx.json"
  grep -q "erp-analytics@$(first "$sbom")$" "$work/out/images.txt" || fail platform-sbom "linux/$p's SBOM names an image not listed"
done
echo "test-images: two platforms built one at a time, joined into one index, an SBOM per image"
[ "$(calls "^ko build --jobs=1 ")" -eq 2 ] && [ "$(calls "^ko ")" -eq 2 ] ||
  fail ko-jobs "ko not run twice (the platform's module, then temporal/'s), each with --jobs=1: two api/gen builds at once stopped CI's runner"
echo "test-images: ko builds one binary and platform at a time"

# erp-temporal: ko in temporal/, with a configuration holding .ko.yaml's
# base and platforms and its one static build.
grep -q "^ko build .* \./cmd/erp-server \./cmd/erp-router \./cmd/erp-web \./cmd/erp$" "$CALLS" ||
  fail temporal "the platform's four not built in one ko run"
grep -q "^ko build .* \./cmd/erp-temporal$" "$CALLS" || fail temporal "erp-temporal not built alone"
[ "$(grep "^ko-ran-in " "$CALLS" | tail -1)" = "ko-ran-in $root/temporal" ] || fail temporal "erp-temporal not built in temporal/: $(grep "^ko-ran-in " "$CALLS" | tail -1)"
config="$CALLS.erp-temporal.ko.yaml"
if [ ! -f "$config" ]; then
  fail temporal "erp-temporal built without a configuration of its own"
else
  python3 - "$root/.ko.yaml" "$config" >"$work/log" 2>&1 <<'PY' || { cat "$work/log" >&2; fail temporal "erp-temporal's configuration"; }
import re, sys
root, temporal = open(sys.argv[1]).read(), open(sys.argv[2]).read()
base = lambda t: re.findall(r"^defaultBaseImage: *(\S+)$", t, re.M)
platforms = lambda t: re.findall(r"^  - (linux/\S+)$", t, re.M)
assert base(temporal) == base(root) and len(base(root)) == 1, "base %s, want .ko.yaml's %s" % (base(temporal), base(root))
assert platforms(temporal) == platforms(root) and platforms(root), "platforms %s, want .ko.yaml's %s" % (platforms(temporal), platforms(root))
assert "main: ./cmd/erp-temporal" in temporal and "env: [CGO_ENABLED=0]" in temporal and "flags: [-trimpath]" in temporal \
    and 'ldflags: ["-s -w -buildid="]' in temporal, "not one static build without a build id:\n" + temporal
PY
fi
temporal=()
while IFS= read -r ref; do temporal+=("$ref"); done < <(grep "/erp-temporal@" "$work/out/images.txt")
[ "${#temporal[@]}" -eq 3 ] || fail temporal "${#temporal[@]} erp-temporal references, want the index and two images"
[ "$(sed -n '13,15p' "$work/out/images.txt" | grep -c "/erp-temporal@")" -eq 3 ] || fail temporal "erp-temporal not listed after the four, before erp-analytics"
for ref in ${temporal[@]+"${temporal[@]}"}; do
  grep -l "\"${ref##*@}\"" "$work/out/sbom/"erp-temporal-*.spdx.json >/dev/null || fail temporal "$ref has no SBOM"
done
echo "test-images: erp-temporal built by ko in its own module on .ko.yaml's base and platforms, listed with its SBOMs"

: >"$CALLS"
ERP_COSIGN_KEY=k "$root/scripts/sign-images.sh" "$work/out" >"$work/log" 2>&1 || { cat "$work/log" >&2; fail sign "sign-images.sh failed"; }
for ref in "${analytics[@]}" ${temporal[@]+"${temporal[@]}"}; do
  [ "$(calls "^cosign .* $ref$")" -eq 3 ] || fail sign "$ref: $(calls "^cosign .* $ref$") cosign calls, want a signature and two attestations"
done
echo "test-images: every erp-analytics and erp-temporal reference signed with its SBOM and the provenance"

# A tool that fails is a refusal, never a pass: an SBOM that cannot be read
# stops the signing (it could be a second SBOM of an image), and a git
# status that fails stops the provenance (it would read as a clean
# checkout).
cp -R "$work/out" "$work/unreadable"
echo '{"packages": "not a list"}' >"$work/unreadable/sbom/zz-broken.spdx.json"
if ERP_COSIGN_KEY=k "$root/scripts/sign-images.sh" "$work/unreadable" >"$work/log" 2>&1; then fail unreadable-sbom "signed past an SBOM jq cannot read"; fi
grep -q "or one cannot be read" "$work/log" || { cat "$work/log" >&2; fail unreadable-sbom "refused without the reason"; }
printf '#!/usr/bin/env bash\n[ "$1" != status ] || { echo "git status: failed" >&2; exit 128; }\nexec "%s" "$@"\n' "$(command -v git)" >"$work/bin/git"
chmod +x "$work/bin/git"
if PATH="$work/bin:$PATH" "$root/scripts/provenance.sh" >"$work/log" 2>&1; then fail provenance-git "wrote a provenance though git status failed: $(cat "$work/log")"; fi
grep -q "git status: failed" "$work/log" || { cat "$work/log" >&2; fail provenance-git "failed for another reason than git status"; }
rm "$work/bin/git"
echo "test-images: an unreadable SBOM stops the signing, a failing git status the provenance"

: >"$CALLS"
"$root/scripts/image-scan.sh" "$work/out" >"$work/log" 2>&1 || { cat "$work/log" >&2; fail scan "image-scan.sh failed"; }
for p in amd64 arm64; do
  [ "$(calls "^grype sbom:.*/erp-analytics-linux-$p.spdx.json ")" -eq 1 ] || fail scan "erp-analytics's linux/$p SBOM not scanned"
  [ "$(calls "^grype registry:gcr.io/distroless/cc-debian12:nonroot@sha256:[0-9a-f]* --platform linux/$p ")" -eq 1 ] || fail scan "the cc base not scanned for linux/$p"
done
echo "test-images: erp-analytics's SBOMs and its cc base scanned"
# The upstream images the release copies (ADR-0032 amendment of
# 2026-10-31): each pin of deploy/images/upstream.txt, PgBouncer's and
# NATS's, scanned once by digest on the platform copied, with the same
# threshold and exceptions as the platform's images; a fixable high fails
# the scan, one .grype.yaml accepts does not.
scan_args="--fail-on high --only-fixed --config .grype.yaml"
pins=0
while read -r name source tag pin; do
  case "$name" in '' | '#'*) continue ;; esac
  [ "$(calls "^grype registry:$source:$tag@$pin --platform linux/arm64 $scan_args$")" -eq 1 ] ||
    fail upstream-scan "$name ($source:$tag@$pin) not scanned once on linux/arm64 with $scan_args"
  pins=$((pins + 1))
done <"$root/deploy/images/upstream.txt"
[ "$(calls "^grype registry:")" -eq $((pins + 4)) ] || fail upstream-scan "$(calls "^grype registry:") registry scans, want the bases' 4 and the $pins pins"
for name in pgbouncer nats; do
  [ "$(calls "^grype registry:[^ ]*/$name:")" -eq 1 ] || fail upstream-scan "$name not scanned"
done
: >"$CALLS"
if FAKE_FINDING="cloudnative-pg/pgbouncer CVE-2099-00001" "$root/scripts/image-scan.sh" "$work/out" >"$work/log" 2>&1; then
  fail upstream-scan "a fixable high in PgBouncer's pin passed the scan"
fi
grep -q "CVE-2099-00001 High (fixed) in cloudnative-pg/pgbouncer" "$work/log" || { cat "$work/log" >&2; fail upstream-scan "the scan failed without the finding"; }
: >"$CALLS"
excepted="$(sed -n 's/^ *- vulnerability: //p' "$root/.grype.yaml" | head -1)"
[ -n "$excepted" ] || fail upstream-scan ".grype.yaml lists no exception to test with"
FAKE_FINDING="library/nats $excepted" "$root/scripts/image-scan.sh" "$work/out" >"$work/log" 2>&1 ||
  { cat "$work/log" >&2; fail upstream-scan "$excepted, which .grype.yaml accepts, failed NATS's scan"; }
[ "$(calls "^grype registry:[^ ]*/library/nats:")" -eq 1 ] || fail upstream-scan "NATS not scanned with the exception"
echo "test-images: every upstream pin scanned on linux/arm64; a fixable high fails the scan, an exception in .grype.yaml does not"

KO_PLATFORMS=linux/amd64 images "$work/one" || { cat "$work/log" >&2; fail one-platform "images.sh failed"; }
[ "$(calls "^docker buildx build .*--platform linux/amd64 ")" -eq 1 ] && [ "$(calls "^docker buildx build")" -eq 1 ] ||
  fail one-platform "KO_PLATFORMS=linux/amd64 built $(calls "^docker buildx build") times"
[ "$(grep -c "/erp-analytics@" "$work/one/images.txt")" -eq 2 ] || fail one-platform "want the index and one image listed"
# ko lists a local one-platform build as repo/name:tag@digest (ci-local's
# reproducibility gate and scan): erp-temporal is listed as the four are,
# with its SBOM.
for b in erp-server erp-router erp-web erp erp-temporal; do
  [ "$(grep -c "^erp.local/$b:latest@sha256:[0-9a-f]\{64\}$" "$work/one/images.txt")" -eq 1 ] || fail one-platform "$b not listed once as ko lists a local build"
done
ref="$(grep "/erp-temporal:" "$work/one/images.txt" || true)"
grep -l "\"${ref##*@}\"" "$work/one/sbom/erp-temporal-linux-amd64.spdx.json" >/dev/null 2>&1 || fail one-platform "erp-temporal's local image has no SBOM"
echo "test-images: KO_PLATFORMS builds one platform, erp-temporal listed from a local build"

KO_DOCKER_REPO=registry.test/erp KO_TAGS=v1.2.3 PUSH=true images "$work/pushed" || { cat "$work/log" >&2; fail push "images.sh failed"; }
[ "$(calls "^crane push --index .* registry.test/erp/erp-analytics:v1.2.3$")" -eq 1 ] || fail push "the index not pushed under v1.2.3"
grep -q "^registry.test/erp/erp-analytics@sha256:" "$work/pushed/images.txt" || fail push "the pushed index not listed by digest"
# refused WANT ENV...: images.sh fails, saying WANT.
refused() {
  local want="$1"
  shift
  if env "$@" "$root/scripts/images.sh" "$work/refused" >"$work/log" 2>&1; then fail refused "$* was accepted"; fi
  grep -q "$want" "$work/log" || { cat "$work/log" >&2; fail refused "$* failed without \"$want\""; }
}
refused "not the index built" PUSH=true FAKE_PUSHED="sha256:$(printf "%064d" 1)"
refused "want one image manifest" FAKE_INDEX=1
refused "holds a linux-s390x image" FAKE_ARCH=s390x
refused "is not one platform" KO_PLATFORMS=all
refused "ko listed no erp-temporal image" FAKE_UNLISTED=erp-temporal
echo "test-images: the index pushed under each tag; another digest pushed, an archive of an index or of another platform and a platform that is not one refused"

: >"$CALLS"
"$root/scripts/images-reproducible.sh" "$work/repro" >"$work/log" 2>&1 || { cat "$work/log" >&2; fail reproducible "the gate failed"; }
[ "$(calls "^docker buildx create")" -eq 2 ] && [ "$(calls "^docker buildx rm --force erp-reproducible-")" -eq 2 ] ||
  fail reproducible "want a builder created and removed per build"
[ "$(calls "^docker buildx build --builder erp-reproducible-")" -eq 4 ] || fail reproducible "the builds did not use their own builders"
: >"$CALLS"
if FAKE_DRIFT=1 "$root/scripts/images-reproducible.sh" "$work/repro" >"$work/log" 2>&1; then fail drift "differing digests accepted"; fi
grep -q "different digests" "$work/log" || fail drift "refused without naming the digests"
[ "$(calls "^docker buildx create")" -eq "$(calls "^docker buildx rm --force")" ] || fail drift "a builder left behind"
echo "test-images: each build on a builder of its own; differing erp-analytics digests fail the gate"

: >"$CALLS"
"$root/scripts/base-image-current.sh" >"$work/log" 2>&1 || { cat "$work/log" >&2; fail current "base-image-current.sh failed"; }
[ "$(calls "^crane digest")" -eq 3 ] || fail current "$(calls "^crane digest") pins checked, want 3"
if FAKE_MOVED=gcr.io/distroless/cc-debian12:nonroot "$root/scripts/base-image-current.sh" >"$work/log" 2>&1; then fail moved "a moved base accepted"; fi
grep -q "cc-debian12:nonroot is now .*erp-analytics.Dockerfile pins" "$work/log" || { cat "$work/log" >&2; fail moved "the moved pin not named"; }
grep -q "golang:1.26.10-bookworm is still" "$work/log" || fail moved "the pins after the moved one not checked"
echo "test-images: base-image-current checks ko's base and erp-analytics's builder and base"

# The release's registry (ADR-0032 amendment of 2026-10-29): what a
# release pushes, built (images.sh) or copied (mirror-images.sh, planned
# offline), lands in tooling's ECR, each image in a repository modules/ecr
# creates and no repository left without an image, under the name
# promotion-checks.sh requires of a cell's release.json; and every image a
# release.json names is one of them.
ecr=111111111111.dkr.ecr.eu-central-1.amazonaws.com
KO_DOCKER_REPO="$ecr" KO_TAGS=v1.2.3 PUSH=true images "$work/ecr" || { cat "$work/log" >&2; fail registry "images.sh failed"; }
plan="$(DRY_RUN=true CRANE=false SYFT=false REGISTRY="$ecr" "$root/scripts/mirror-images.sh" "$work/ecr-mirror")" ||
  fail registry "the mirror's plan failed: $plan"
sed -n 's/^mirror-images: would copy .* to \(.*\):[^:]*$/\1/p' <<<"$plan" >"$work/copies"
python3 - "$ecr" "$work/ecr/images.txt" "$work/copies" "$root/deploy/terraform/modules/ecr/main.tf" "$root/scripts/promotion-checks.sh" \
  >"$work/log" 2>&1 <<'PY' || { cat "$work/log" >&2; fail registry "the release's images, the registry's repositories and release.json disagree"; }
import ast, re, sys
host, built, copies, tf, checks = sys.argv[1:]
refs = [l.strip() for l in open(built) if l.strip()]
targets = [l.strip() for l in open(copies) if l.strip()]
assert refs and targets, "nothing built (%d) or copied (%d)" % (len(refs), len(targets))
repos = set()
for ref in [r.split("@")[0] for r in refs] + targets:
    assert ref.startswith(host + "/") and "/" not in ref[len(host) + 1:], "%s is not in the release registry %s" % (ref, host)
    repos.add(ref[len(host) + 1:])
ecr = set(ast.literal_eval(re.search(r'variable "repositories" \{.*?default\s*=\s*(\[[^]]*\])', open(tf).read(), re.S).group(1)))
assert repos == ecr, "the release pushes %s; modules/ecr creates %s" % (sorted(repos), sorted(ecr))
images = ast.literal_eval(re.search(r"^images = (\{.*\})$", open(checks).read(), re.M).group(1))
missing = sorted(set(images.values()) - repos)
assert not missing, "release.json names %s, which no release pushes" % missing
for name, repo in images.items():
    assert re.fullmatch(r"[a-z0-9.-]+(:[0-9]+)?/" + re.escape(repo), "%s/%s" % (host, repo)), \
        "%s/%s is not a name promotion-checks.sh takes for images.%s" % (host, repo, name)
PY
# The workflow pushes and copies into ERP_RELEASE_REGISTRY and names no
# other registry.
python3 - "$root/.github/workflows/release.yml" >"$work/log" 2>&1 <<'PY' || { cat "$work/log" >&2; fail registry "release.yml"; }
import re, sys
text = open(sys.argv[1]).read()
steps = {m.group(1): m.group(2) for m in re.finditer(r"^      - name: (.*)\n((?:(?!      - name: ).*\n)*)", text, re.M)}
for name, run in (("Images, pushed", 'KO_DOCKER_REPO="$REGISTRY" '), ("Upstream images, mirrored into the release registry", "./scripts/mirror-images.sh")):
    step = steps.get(name)
    assert step is not None, "no step %r" % name
    assert "REGISTRY: ${{ vars.ERP_RELEASE_REGISTRY }}" in step and run in step, "%r does not push to ERP_RELEASE_REGISTRY:\n%s" % (name, step)
assert text.count("KO_DOCKER_REPO=") == 1, "release.yml sets KO_DOCKER_REPO more than once"
for other in ("ghcr.io", "docker.io", "packages: write"):
    assert other not in text, "release.yml names %s: the release's registry is tooling's ECR" % other
PY
echo "test-images: the release pushes every image it builds or copies to ERP_RELEASE_REGISTRY, into modules/ecr's repositories, under release.json's names"

# The release scans before anything leaves the runner (ADR-0032 amendment
# of 2026-10-31): make image-scan, which scans the upstream pins too, runs
# in the step before the first push, so a fixable high in a pin fails the
# release before its copy is pushed, signed, attested or listed; and the
# copy reads the list the scan read.
python3 - "$root/.github/workflows/release.yml" "$root/Makefile" >"$work/log" 2>&1 <<'PY' || { cat "$work/log" >&2; fail release-scan "release.yml"; }
import re, sys
text, make = open(sys.argv[1]).read(), open(sys.argv[2]).read()
names = re.findall(r"^      - name: (.*)$", text, re.M)
steps = {m.group(1): m.group(2) for m in re.finditer(r"^      - name: (.*)\n((?:(?!      - name: ).*\n)*)", text, re.M)}
scan = "Images, built and scanned before anything is pushed"
assert scan in steps and re.search(r"^ *make image-scan$", steps[scan], re.M), "no step %r running make image-scan" % scan
assert re.search(r"^image-scan:\n\t\./scripts/image-scan\.sh dist/images$", make, re.M), "make image-scan does not run image-scan.sh"
assert "continue-on-error" not in steps[scan] and "image-scan ||" not in steps[scan], "a failed scan does not stop the release"
for later in ("Images, pushed", "Upstream images, mirrored into the release registry", "Sign and attest"):
    assert later in names, "no step %r" % later
for name in names:
    if any(w in steps[name] for w in ("PUSH=true", "mirror-images.sh", "sign-images.sh", "cosign ", "crane ")):
        assert names.index(name) > names.index(scan), "%r pushes, copies or signs before %r" % (name, scan)
assert not re.search(r"\bLIST=", text), "release.yml gives the mirror another list than the scan's"
PY
echo "test-images: the release scans the platform's images and the upstream pins before it pushes, copies or signs anything"

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "test-images: all cases passed"
