#!/usr/bin/env bash
# Signs the images a pushed build wrote to OUT (default dist/images, by
# scripts/images.sh with PUSH=true) and attaches two signed attestations to
# each (ADR-0032):
#   - its SPDX SBOM (type spdxjson), the one ko wrote for that image;
#   - the build provenance of scripts/provenance.sh (type slsaprovenance1),
#     or, for an upstream image scripts/mirror-images.sh copied (a file of
#     OUT/provenance naming its digest), that copy's provenance completed
#     with the build provenance's commit, checkout state and run: the
#     release that copied it, at the commit whose deploy/images/upstream.txt
#     pinned it (ADR-0032 amendment of 2026-10-28, T72i).
# Every reference in images.txt is signed: each multi-platform index and
# each per-platform image, so a verifier holding either digest finds a
# signature. Signing a release again adds signatures and attestations
# beside the earlier ones; verification accepts either.
#
# ERP_COSIGN_KEY is the signing key: in releases the AWS KMS key,
# awskms:///arn:aws:kms:<region>:<account>:key/<id>; in tests a cosign key
# file. Nothing goes to a transparency log: release/signing-config.json
# names no Sigstore service, because the repository is private and Rekor is
# public (ADR-0032). COSIGN_PASSWORD unlocks a key file. PROVENANCE, when
# set, is a build provenance file used in place of provenance.sh's (the
# tests', which stand for releases at other commits).
set -euo pipefail
OUT="${1:-dist/images}"
COSIGN="${COSIGN:-go run github.com/sigstore/cosign/v3/cmd/cosign@v3.1.1}"
key="${ERP_COSIGN_KEY:?ERP_COSIGN_KEY: the signing key (awskms:///arn:... or a cosign key file)}"
config="$(dirname "$0")/../release/signing-config.json"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
provenance="$work/build.json"
if [ -n "${PROVENANCE:-}" ]; then
  cp "$PROVENANCE" "$provenance"
else
  "$(dirname "$0")/provenance.sh" >"$provenance"
fi
# sbom_for prints the one SBOM in OUT/sbom that describes the image digest
# $1 (ko names the image it describes first), and fails unless exactly one
# does.
sbom_for() {
  local found="" f
  for f in "$OUT"/sbom/*.spdx.json; do
    [ -e "$f" ] || continue
    if [ "$(jq -r '.packages[0].name' "$f")" = "$1" ]; then
      [ -z "$found" ] || return 1
      found="$f"
    fi
  done
  [ -n "$found" ] && echo "$found"
}
# provenance_for prints the provenance to attest for the image digest $1:
# a copy's, completed with this release's commit, checkout state and run,
# when one file of OUT/provenance copied that digest, else the build
# provenance; it fails when several copies name it or the copy cannot be
# completed.
provenance_for() {
  local found="" f
  for f in "$OUT"/provenance/*.json; do
    [ -e "$f" ] || continue
    if [ "$(jq -r '.buildDefinition.externalParameters.digest' "$f")" = "$1" ]; then
      [ -z "$found" ] || return 1
      found="$f"
    fi
  done
  if [ -z "$found" ]; then
    echo "$provenance"
    return
  fi
  jq --slurpfile build "$provenance" '
    $build[0].buildDefinition as $b
    | .buildDefinition.externalParameters = ($b.externalParameters + .buildDefinition.externalParameters)
    | .buildDefinition.internalParameters = {sourceModified: $b.internalParameters.sourceModified}
    | .buildDefinition.resolvedDependencies = ([$b.resolvedDependencies[] | select(.digest.gitCommit)] + .buildDefinition.resolvedDependencies)
    | .runDetails = $build[0].runDetails' "$found" >"$work/${1#sha256:}.json" || return 1
  echo "$work/${1#sha256:}.json"
}
signed=0
copies=0
while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  if ! sbom="$(sbom_for "${ref##*@}")"; then
    echo "sign-images: $ref: no single SBOM in $OUT/sbom describes it" >&2
    exit 1
  fi
  if ! attested="$(provenance_for "${ref##*@}")"; then
    echo "sign-images: $ref: no single readable copy in $OUT/provenance names it" >&2
    exit 1
  fi
  [ "$attested" = "$provenance" ] || copies=$((copies + 1))
  $COSIGN sign --yes --key "$key" --signing-config "$config" "$ref"
  $COSIGN attest --yes --key "$key" --signing-config "$config" --type spdxjson --predicate "$sbom" "$ref"
  $COSIGN attest --yes --key "$key" --signing-config "$config" --type slsaprovenance1 --predicate "$attested" "$ref"
  signed=$((signed + 1))
done <"$OUT/images.txt"
if [ "$signed" -eq 0 ]; then
  echo "sign-images: $OUT/images.txt lists no image" >&2
  exit 1
fi
echo "sign-images: $signed images signed, each with its SBOM and its provenance attested ($copies of them copies of upstream images)"
