#!/usr/bin/env bash
# Tests scripts/mirror-images.sh against a local registry standing for both
# the upstream and the release registry:
#   - an image is copied into erp-<name> by its pinned digest, keeping it,
#     tagged with its upstream tag, listed in images.txt, and given an SBOM
#     whose first package is the digest (how sign-images.sh finds it);
#   - a second release finds the tag in place and copies nothing;
#   - a pin the source does not serve, a tag in the release registry that
#     names another image, an empty list and a malformed digest are refused;
#   - a dry run on deploy/images/upstream.txt plans PgBouncer's and NATS's
#     copies by their pins without a registry or a file written;
#   - as the release goes on (ADR-0032 amendment of 2026-10-28, T72i):
#     sign-images.sh, with a key made for the test, attests the copy's
#     provenance completed with the release's commit and run (a built
#     image beside it keeps the build provenance), and verify-image.sh
#     accepts the copy for every release that attested it and refuses it
#     for another commit or one whose checkout had uncommitted changes, a
#     copy's provenance naming another digest, an image whose provenance
#     says both copy and build, a built image attested at two commits and
#     one whose checkout state jq fails to read;
#     two copies of one digest, or an unreadable one, stop the signing.
# ZOT, CRANE, SYFT and COSIGN name the binaries.
set -euo pipefail
cd "$(dirname "$0")/.."
root="$PWD"
export CRANE="${CRANE:-crane}" SYFT="${SYFT:-syft}" COSIGN="${COSIGN:-cosign}"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/registry.sh
. "$root/scripts/lib/registry.sh"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'stop_registry; rm -rf "$work"'
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

# The real list, planned offline: tools that fail if called, no OUT left.
plan="$(DRY_RUN=true CRANE=false SYFT=false REGISTRY=registry.example "$root/scripts/mirror-images.sh" dry)" ||
  fail "the dry run failed: $plan"
[ ! -e dry ] || fail "the dry run wrote $(ls dry)"
while read -r name source tag pin; do
  case "$name" in '' | '#'*) continue ;; esac
  grep -qxF "mirror-images: would copy $source:$tag@$pin to registry.example/erp-$name:$tag" <<<"$plan" ||
    fail "the dry run did not plan $name: $plan"
done <"$root/deploy/images/upstream.txt"
for name in pgbouncer nats; do grep -q "/erp-$name:" <<<"$plan" || fail "the dry run did not plan erp-$name: $plan"; done
echo "test-mirror-images: the dry run plans every pinned upstream image, PgBouncer's and NATS's, offline"

# The signing, as the release runs it after the mirror: a key made for
# the test, and the build provenance of a clean checkout at commit A, then
# at commit B (PROVENANCE stands for provenance.sh in a release).
export COSIGN_PASSWORD=""
mkdir keys && (cd keys && "$COSIGN" generate-key-pair >/dev/null 2>&1)
commit_a="$(printf '%040d' 0 | tr 0 a)" commit_b="$(printf '%040d' 0 | tr 0 b)" commit_c="$(printf '%040d' 0 | tr 0 c)"
(cd "$root" && ./scripts/provenance.sh) >base.json
# at COMMIT OUT: the build provenance of a clean checkout at COMMIT.
at() {
  jq --arg c "$1" '.buildDefinition.internalParameters.sourceModified = false
    | .buildDefinition.resolvedDependencies[0].digest.gitCommit = $c' base.json >"$2"
}
at "$commit_a" build-a.json
at "$commit_b" build-b.json
# A built image beside the copy in OUT, with the SBOM ko would write.
built="$(push_image erp-server v1)"
echo "$registry/erp-server@$built" >>out/images.txt
jq -n --arg d "$built" '{spdxVersion: "SPDX-2.3", packages: [{name: $d}]}' >out/sbom/erp-server.spdx.json
# sign OUT BUILD: sign-images.sh on OUT as a release at BUILD's commit.
sign() { ERP_COSIGN_KEY=keys/cosign.key PROVENANCE="$2" "$root/scripts/sign-images.sh" "$1" >"$1.sign.log" 2>&1 || { cat "$1.sign.log" >&2; fail "signing $1 failed"; }; }
verify() { ERP_COSIGN_PUBLIC_KEY=keys/cosign.pub ERP_EXPECTED_COMMIT="$1" "$root/scripts/verify-image.sh" "$2"; }
# verified COMMIT REF: verify-image.sh accepts REF for COMMIT.
verified() { verify "$1" "$2" >verify.log 2>&1 || { cat verify.log >&2; fail "$2 was refused for $1"; }; }
# unverified COMMIT REF WANT: verify-image.sh refuses REF for COMMIT, saying WANT.
unverified() {
  if verify "$1" "$2" >verify.log 2>&1; then fail "$2 was accepted for $1"; fi
  grep -q "$3" verify.log || { cat verify.log >&2; fail "$2 was refused for $1 without \"$3\""; }
}
copy="$registry/erp-proxy@$digest"
sign out build-a.json
grep -q "2 images signed.*1 of them copies" out.sign.log || fail "sign-images: $(tail -1 out.sign.log)"
verified "$commit_a" "$copy"
verified "$commit_a" "$registry/erp-server@$built"
statement() { "$COSIGN" verify-attestation --key keys/cosign.pub --insecure-ignore-tlog=true --type slsaprovenance1 "$1" 2>/dev/null | jq -c '.payload | @base64d | fromjson'; }
statement "$copy" | jq -e --arg d "$digest" --arg s "$registry/upstream/proxy" --arg c "$commit_a" --arg m "$copy" '
  .subject[0].digest.sha256 == ($d | ltrimstr("sha256:"))
  and .predicate.buildDefinition.buildType == "https://github.com/mustie7iy/agentic-erp/mirror-images@v1"
  and .predicate.buildDefinition.externalParameters.source == $s
  and .predicate.buildDefinition.externalParameters.tag == "1.0"
  and .predicate.buildDefinition.externalParameters.digest == $d
  and .predicate.buildDefinition.externalParameters.mirrored == $m
  and .predicate.buildDefinition.internalParameters.sourceModified == false
  and ([.predicate.buildDefinition.resolvedDependencies[].digest.gitCommit // empty] == [$c])
  and (.predicate.buildDefinition.resolvedDependencies | any(.uri == ("oci://" + $s + ":1.0") and .digest.sha256 == ($d | ltrimstr("sha256:"))))' >/dev/null ||
  fail "the copy's attested provenance: $(statement "$copy")"
statement "$registry/erp-server@$built" | jq -e '.predicate.buildDefinition.buildType == "https://github.com/mustie7iy/agentic-erp/ko-images@v1"' >/dev/null ||
  fail "the built image's attested provenance is not the build's"
echo "test-mirror-images: the copy is signed with its SBOM and a copy provenance at the release's commit; a built image keeps its build provenance"

# The next release, at commit B, attests the same copy again.
mirror list.txt next || { cat next.log >&2; fail "the next release's mirror failed"; }
sign next build-b.json
verified "$commit_b" "$copy"
verified "$commit_a" "$copy"
unverified "$commit_c" "$copy" "was not copied by the release at $commit_c"
# A release at C from a checkout with uncommitted changes attests it too:
# C's own verification refuses it, A's still passes.
jq '.buildDefinition.internalParameters.sourceModified = true
  | .buildDefinition.resolvedDependencies[0].digest.gitCommit = "'"$commit_c"'"' build-a.json >build-c-dirty.json
mirror list.txt dirty || { cat dirty.log >&2; fail "the dirty release's mirror failed"; }
sign dirty build-c-dirty.json
unverified "$commit_c" "$copy" "was not copied by the release at $commit_c from a clean checkout"
verified "$commit_a" "$copy"
echo "test-mirror-images: a copy attested by two releases verifies for either commit, not for a third nor for one from uncommitted changes"

# Refusals, each on an image of its own.
other_ref="$registry/erp-proxy@$other"
jq --arg d "$digest" '.buildDefinition.externalParameters.digest = $d' "out/provenance/erp-proxy.json" >wrong.json
jq -n '{spdxVersion: "SPDX-2.3", packages: []}' >empty-sbom.json
attest() { "$COSIGN" attest --yes --key keys/cosign.key --signing-config "$root/release/signing-config.json" --type "$2" --predicate "$3" "$1" >/dev/null 2>&1 || fail "attesting $1 failed"; }
"$COSIGN" sign --yes --key keys/cosign.key --signing-config "$root/release/signing-config.json" "$other_ref" >/dev/null 2>&1 || fail "signing $other_ref failed"
attest "$other_ref" spdxjson empty-sbom.json
attest "$other_ref" slsaprovenance1 wrong.json
unverified any "$other_ref" "names another digest"
jq --arg d "$built" '.buildDefinition.externalParameters.digest = $d' "out/provenance/erp-proxy.json" >mixed.json
attest "$registry/erp-server@$built" slsaprovenance1 mixed.json
unverified any "$registry/erp-server@$built" "both copy and build"
mixed="$(push_image erp-router v1)"
"$COSIGN" sign --yes --key keys/cosign.key --signing-config "$root/release/signing-config.json" "$registry/erp-router@$mixed" >/dev/null 2>&1 || fail "signing erp-router failed"
attest "$registry/erp-router@$mixed" spdxjson empty-sbom.json
attest "$registry/erp-router@$mixed" slsaprovenance1 build-a.json
attest "$registry/erp-router@$mixed" slsaprovenance1 build-b.json
unverified "$commit_a" "$registry/erp-router@$mixed" "was built from"
# A check whose tool fails refuses: jq failing on the checkout state of a
# clean build at A (as an unreadable provenance or a broken jq would)
# cannot read as a clean checkout.
mkdir -p failing
printf '#!/usr/bin/env bash\ncase "$*" in *sourceModified*) echo "jq: error" >&2; exit 5 ;; esac\nexec "%s" "$@"\n' "$(command -v jq)" >failing/jq
chmod +x failing/jq
clean="$(push_image erp-web v1)"
"$COSIGN" sign --yes --key keys/cosign.key --signing-config "$root/release/signing-config.json" "$registry/erp-web@$clean" >/dev/null 2>&1 || fail "signing erp-web failed"
attest "$registry/erp-web@$clean" spdxjson empty-sbom.json
attest "$registry/erp-web@$clean" slsaprovenance1 build-a.json
verified "$commit_a" "$registry/erp-web@$clean"
if PATH="$PWD/failing:$PATH" verify "$commit_a" "$registry/erp-web@$clean" >verify.log 2>&1; then fail "a jq error on the checkout state passed for a clean checkout"; fi
grep -q "was built from uncommitted changes, or its provenance does not say" verify.log || { cat verify.log >&2; fail "a jq error on the checkout state was refused without the reason"; }
cp out/provenance/erp-proxy.json out/provenance/erp-twin.json
if ERP_COSIGN_KEY=keys/cosign.key PROVENANCE=build-a.json "$root/scripts/sign-images.sh" out >twin.log 2>&1; then fail "two copies of one digest were signed"; fi
grep -q "no single readable copy" twin.log || { cat twin.log >&2; fail "two copies of one digest failed without the reason"; }
rm out/provenance/erp-twin.json
echo '{"buildDefinition":' >out/provenance/erp-broken.json
if ERP_COSIGN_KEY=keys/cosign.key PROVENANCE=build-a.json "$root/scripts/sign-images.sh" out >broken.log 2>&1; then fail "a broken copy provenance was signed past"; fi
grep -q "no single readable copy" broken.log || { cat broken.log >&2; fail "a broken copy provenance failed without the reason"; }
echo "test-mirror-images: another digest, copy and build in one, a build at two commits or whose checkout state jq fails to read, two copies of one digest and an unreadable copy are refused"
finished
