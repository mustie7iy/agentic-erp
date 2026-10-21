#!/usr/bin/env bash
# Tests the image pipeline's scripts (ADR-0032) with fake ko, docker, syft,
# crane, grype and cosign that write what the real ones would and log their
# calls; nothing is built, pulled or pushed:
#   - images.sh builds erp-analytics with buildx one platform at a time,
#     dated at the epoch, without buildx attestations, joins the platforms
#     into one index listed in images.txt after ko's images, writes an SBOM
#     per image whose first package is its digest, honours KO_PLATFORMS,
#     pushes the index with crane under each tag, and refuses an archive
#     that is not one image of the platform asked, a platform that is not
#     one and a push whose digest is not the index built;
#   - images-reproducible.sh gives each build a buildx builder of its own,
#     removes it, and fails when the digests differ;
#   - image-scan.sh scans erp-analytics's SBOMs and its cc base;
#   - sign-images.sh signs and attests every erp-analytics reference;
#   - base-image-current.sh checks the three pins and names the moved one.
set -euo pipefail
cd "$(dirname "$0")/.."
root="$PWD"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
export CALLS="$work/calls"

fake() { printf '#!/usr/bin/env bash\nset -euo pipefail\necho "%s $*" >>"$CALLS"\n%s\n' "$1" "$2" >"$work/bin/$1"; chmod +x "$work/bin/$1"; }
# ko: each binary's index and per-platform images, with ko's SBOM names.
fake ko 'refs="" sboms="" platforms="linux/amd64,linux/arm64"
for a in "$@"; do case "$a" in --image-refs=*) refs="${a#*=}" ;; --sbom-dir=*) sboms="${a#*=}" ;; --platform=*) platforms="${a#*=}" ;; esac; done
for b in erp-server erp-router erp; do
  d="sha256:$(printf "%s" "$b" | shasum -a 256 | cut -c1-64)"
  echo "$KO_DOCKER_REPO/$b@$d" >>"$refs"; printf "{\"packages\": [{\"name\": \"%s\"}]}" "$d" >"$sboms/$b-index.spdx.json"
  IFS=, read -r -a ps <<<"$platforms"
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
fake grype ''
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
[ "$(head -9 "$work/out/images.txt" | grep -c "/erp-analytics@")" -eq 0 ] || fail order "erp-analytics before ko's images"
first() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["packages"][0]["name"])' "$1"; }
[ "$(first "$work/out/sbom/erp-analytics-index.spdx.json")" = "${analytics[0]##*@}" ] || fail index-sbom "the index's SBOM does not name it first"
for p in amd64 arm64; do
  sbom="$work/out/sbom/erp-analytics-linux-$p.spdx.json"
  grep -q "erp-analytics@$(first "$sbom")$" "$work/out/images.txt" || fail platform-sbom "linux/$p's SBOM names an image not listed"
done
echo "test-images: two platforms built one at a time, joined into one index, an SBOM per image"

: >"$CALLS"
ERP_COSIGN_KEY=k "$root/scripts/sign-images.sh" "$work/out" >"$work/log" 2>&1 || { cat "$work/log" >&2; fail sign "sign-images.sh failed"; }
for ref in "${analytics[@]}"; do
  [ "$(calls "^cosign .* $ref$")" -eq 3 ] || fail sign "$ref: $(calls "^cosign .* $ref$") cosign calls, want a signature and two attestations"
done
echo "test-images: every erp-analytics reference signed with its SBOM and the provenance"

: >"$CALLS"
"$root/scripts/image-scan.sh" "$work/out" >"$work/log" 2>&1 || { cat "$work/log" >&2; fail scan "image-scan.sh failed"; }
for p in amd64 arm64; do
  [ "$(calls "^grype sbom:.*/erp-analytics-linux-$p.spdx.json ")" -eq 1 ] || fail scan "erp-analytics's linux/$p SBOM not scanned"
  [ "$(calls "^grype registry:gcr.io/distroless/cc-debian12:nonroot@sha256:[0-9a-f]* --platform linux/$p ")" -eq 1 ] || fail scan "the cc base not scanned for linux/$p"
done
echo "test-images: erp-analytics's SBOMs and its cc base scanned"

KO_PLATFORMS=linux/amd64 images "$work/one" || { cat "$work/log" >&2; fail one-platform "images.sh failed"; }
[ "$(calls "^docker buildx build .*--platform linux/amd64 ")" -eq 1 ] && [ "$(calls "^docker buildx build")" -eq 1 ] ||
  fail one-platform "KO_PLATFORMS=linux/amd64 built $(calls "^docker buildx build") times"
[ "$(grep -c "/erp-analytics@" "$work/one/images.txt")" -eq 2 ] || fail one-platform "want the index and one image listed"
echo "test-images: KO_PLATFORMS builds one platform"

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
grep -q "golang:1.26.2-bookworm is still" "$work/log" || fail moved "the pins after the moved one not checked"
echo "test-images: base-image-current checks ko's base and erp-analytics's builder and base"

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "test-images: all cases passed"
