#!/usr/bin/env bash
# The signing round trip (ADR-0032), against a throwaway registry REPO (for
# example localhost:5000/erp): pushes the images, signs them and attests
# their SBOM and provenance with a cosign key pair made for the run (it
# stands in for the KMS key, which comes with T40), and checks that
# verify-image.sh accepts them and refuses what it must refuse: another
# key, another commit, a reference by tag, an unsigned image, an image with
# attestations but no signature, and provenance from uncommitted changes.
set -euo pipefail
REPO="${1:?usage: signing-roundtrip.sh <registry/repository>, e.g. localhost:5000/erp}"
COSIGN="${COSIGN:-go run github.com/sigstore/cosign/v3/cmd/cosign@v3.1.1}"
export COSIGN
scripts="$(cd "$(dirname "$0")" && pwd)"
config="$scripts/../release/signing-config.json"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export COSIGN_PASSWORD=""
mkdir -p "$work/release" "$work/other"
(cd "$work/release" && $COSIGN generate-key-pair >/dev/null 2>&1)
(cd "$work/other" && $COSIGN generate-key-pair >/dev/null 2>&1)
key="$work/release/cosign.key"
pub="$work/release/cosign.pub"

PUSH=true KO_DOCKER_REPO="$REPO" "$scripts/images.sh" "$work/out" >/dev/null
ERP_COSIGN_KEY="$key" "$scripts/sign-images.sh" "$work/out"
refs=()
while IFS= read -r ref; do refs+=("$ref"); done <"$work/out/images.txt"
commit="$(git rev-parse HEAD)"
# A checkout with uncommitted changes builds provenance that verification
# with a commit refuses (checked below with a provenance made for it), so
# such a run accepts its own images without the commit check.
expected="$commit"
if [ -n "$(git status --porcelain)" ]; then
  if [ -n "${GITHUB_ACTIONS:-}" ]; then
    echo "signing-roundtrip: CI's checkout has uncommitted changes; the commit check must run" >&2
    exit 1
  fi
  expected=any
  echo "signing-roundtrip: the checkout has uncommitted changes: images verified without the commit check"
fi
ERP_COSIGN_PUBLIC_KEY="$pub" ERP_EXPECTED_COMMIT="$expected" "$scripts/verify-image.sh" "${refs[@]}"

# refuses runs verify-image.sh and fails the round trip unless it fails
# with the expected reason, so a broken tool cannot pass for a refusal.
refuses() {
  local what="$1" reason="$2"
  shift 2
  if "$@" >"$work/refused.log" 2>&1; then
    echo "signing-roundtrip: verify-image.sh accepted $what" >&2
    cat "$work/refused.log" >&2
    exit 1
  fi
  if ! grep -q "$reason" "$work/refused.log"; then
    echo "signing-roundtrip: verify-image.sh refused $what, but not with \"$reason\":" >&2
    cat "$work/refused.log" >&2
    exit 1
  fi
  echo "signing-roundtrip: refused $what ($reason)"
}
verify() { env ERP_COSIGN_PUBLIC_KEY="$1" ERP_EXPECTED_COMMIT="$2" "$scripts/verify-image.sh" "$3"; }
refuses "another key" "signatures do not match" verify "$work/other/cosign.pub" any "${refs[0]}"
refuses "another commit" "was built from" verify "$pub" 0000000000000000000000000000000000000000 "${refs[0]}"
refuses "a reference by tag" "by digest, not by tag" verify "$pub" any "${refs[0]%@*}:latest"

# The same images pushed under another name carry nothing there: signatures
# and attestations live beside the image in its repository.
PUSH=true KO_DOCKER_REPO="$REPO-unsigned" "$scripts/images.sh" "$work/unsigned" >/dev/null
bare=()
while IFS= read -r ref; do bare+=("$ref"); done <"$work/unsigned/images.txt"
refuses "an unsigned image" "no signatures found" verify "$pub" any "${bare[0]}"
sbom="$(ls "$work/unsigned/sbom/"*.spdx.json | head -1)"
"$scripts/provenance.sh" >"$work/provenance.json"
$COSIGN attest --yes --key "$key" --signing-config "$config" --type spdxjson --predicate "$sbom" "${bare[1]}" >/dev/null 2>&1
$COSIGN attest --yes --key "$key" --signing-config "$config" --type slsaprovenance1 --predicate "$work/provenance.json" "${bare[1]}" >/dev/null 2>&1
refuses "attestations without an image signature" "no image signature" verify "$pub" any "${bare[1]}"
jq --arg commit "$commit" '.buildDefinition.internalParameters.sourceModified = true
  | .buildDefinition.resolvedDependencies[0].digest.gitCommit = $commit' "$work/provenance.json" >"$work/modified.json"
$COSIGN sign --yes --key "$key" --signing-config "$config" "${bare[2]}" >/dev/null 2>&1
$COSIGN attest --yes --key "$key" --signing-config "$config" --type spdxjson --predicate "$sbom" "${bare[2]}" >/dev/null 2>&1
$COSIGN attest --yes --key "$key" --signing-config "$config" --type slsaprovenance1 --predicate "$work/modified.json" "${bare[2]}" >/dev/null 2>&1
refuses "provenance from uncommitted changes" "uncommitted changes" verify "$pub" "$commit" "${bare[2]}"
echo "signing-roundtrip: ${#refs[@]} images signed and verified; the six refusals held"
