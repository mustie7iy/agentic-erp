#!/usr/bin/env bash
# Tests scripts/mirror-images.sh against a local registry standing for both
# the upstream and the release registry:
#   - an image is copied into erp-<name> by its pinned digest, keeping it,
#     tagged with its upstream tag, listed in images.txt, and given an SBOM
#     whose first package is the digest (how sign-images.sh finds it);
#   - a second release finds the tag in place and copies nothing;
#   - a pin the source does not serve, a tag in the release registry that
#     names another image, an empty list and a malformed digest are refused.
# ZOT, CRANE and SYFT name the binaries.
set -euo pipefail
cd "$(dirname "$0")/.."
root="$PWD"
export CRANE="${CRANE:-crane}" SYFT="${SYFT:-syft}"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/registry.sh
. "$root/scripts/lib/registry.sh"
trap 'stop_registry; rm -rf "$work"' EXIT
cd "$work"
start_registry "$work"
export REGISTRY="$registry" INSECURE=true

fail() {
  echo "test-mirror-images: $*" >&2
  exit 1
}
# mirror LIST OUT: runs the script, its output in OUT.log.
mirror() { LIST="$1" "$root/scripts/mirror-images.sh" "$2" >"$2.log" 2>&1; }

digest="$(push_image upstream/proxy 1.0)"
other="$(push_image upstream/proxy 2.0)"
printf '# name source tag digest\nproxy %s/upstream/proxy 1.0 %s\n' "$registry" "$digest" >list.txt

mirror list.txt out || { cat out.log >&2; fail "the mirror failed"; }
got="$("$CRANE" digest --insecure "$registry/erp-proxy:1.0")"
[ "$got" = "$digest" ] || fail "erp-proxy:1.0 is $got, want $digest"
[ "$(cat out/images.txt)" = "$registry/erp-proxy@$digest" ] || fail "images.txt: $(cat out/images.txt)"
first="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["packages"][0]["name"])' out/sbom/erp-proxy.spdx.json)"
[ "$first" = "$digest" ] || fail "the SBOM's first package is $first, want $digest"
copied="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["buildDefinition"]["externalParameters"]["digest"])' out/provenance/erp-proxy.json)"
[ "$copied" = "$digest" ] || fail "the provenance names $copied, want $digest"
echo "test-mirror-images: copied by digest, listed, with its SBOM and a provenance of the copy"

mirror list.txt again || { cat again.log >&2; fail "a second release failed"; }
grep -q "erp-proxy@$digest" again/images.txt || fail "a second release did not list the image"
echo "test-mirror-images: a second release finds the image in place"

# refused LIST WANT: the mirror fails, saying WANT.
refused() {
  if mirror "$1" refused; then fail "$1 was mirrored"; fi
  grep -q "$2" refused.log || { cat refused.log >&2; fail "$1 failed without \"$2\""; }
  rm -rf refused refused.log
}
printf 'proxy %s/upstream/proxy 1.0 sha256:%064d\n' "$registry" 0 >unserved.txt
refused unserved.txt "MANIFEST_UNKNOWN\|served"
"$CRANE" tag --insecure "$registry/upstream/proxy@$other" 1.1 >/dev/null 2>&1
"$CRANE" copy --insecure "$registry/upstream/proxy@$other" "$registry/erp-proxy:1.1" >/dev/null 2>&1
printf 'proxy %s/upstream/proxy 1.1 %s\n' "$registry" "$digest" >taken.txt
refused taken.txt "already names $other"
echo '# nothing' >empty.txt
refused empty.txt "lists no image"
printf 'proxy %s/upstream/proxy 1.0 latest\n' "$registry" >malformed.txt
refused malformed.txt "is not a sha256 digest"
printf 'proxy %s/upstream/proxy 1.0 sha256:%064d\n' "$registry" 0 | tr 0 Z >nothex.txt
refused nothex.txt "is not a sha256 digest"
printf '../x %s/upstream/proxy 1.0 %s\n' "$registry" "$digest" >badname.txt
refused badname.txt "is not a repository name"
index="$("$CRANE" index append --insecure -m "$registry/upstream/proxy@$digest" -t "$registry/upstream/proxy:index" 2>/dev/null | sed 's/.*@//')"
printf 'proxy %s/upstream/proxy index %s\n' "$registry" "$index" >index.txt
refused index.txt "want one linux/arm64 image"
amd64="$("$CRANE" mutate --insecure --set-platform linux/amd64 "$registry/upstream/proxy@$digest" -t "$registry/upstream/proxy:amd64" 2>/dev/null | sed 's/.*@//')"
printf 'proxy %s/upstream/proxy amd64 %s\n' "$registry" "$amd64" >amd64.txt
refused amd64.txt "is linux/amd64, want one linux/arm64 image"
echo "test-mirror-images: unserved pins, taken tags, empty lists, malformed digests and names, indexes and other platforms are refused"
