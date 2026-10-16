#!/usr/bin/env bash
# The signing round trip (ADR-0032), against a throwaway registry REPO (for
# example localhost:5000/erp): pushes the images, signs them and attests
# their SBOM and provenance with a cosign key pair made for the run (it
# stands in for the KMS key, which comes with T40), and checks that
# verify-image.sh accepts them and refuses what it must refuse: another
# key, another commit, an unsigned image and a reference by tag.
set -euo pipefail
REPO="${1:?usage: signing-roundtrip.sh <registry/repository>, e.g. localhost:5000/erp}"
COSIGN="${COSIGN:-go run github.com/sigstore/cosign/v3/cmd/cosign@v3.1.1}"
export COSIGN
scripts="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export COSIGN_PASSWORD=""
mkdir -p "$work/release" "$work/other"
(cd "$work/release" && $COSIGN generate-key-pair >/dev/null 2>&1)
(cd "$work/other" && $COSIGN generate-key-pair >/dev/null 2>&1)

PUSH=true KO_DOCKER_REPO="$REPO" "$scripts/images.sh" "$work/out" >/dev/null
ERP_COSIGN_KEY="$work/release/cosign.key" "$scripts/sign-images.sh" "$work/out"
refs=()
while IFS= read -r ref; do refs+=("$ref"); done <"$work/out/images.txt"
commit="$(git rev-parse HEAD)"
ERP_COSIGN_PUBLIC_KEY="$work/release/cosign.pub" ERP_EXPECTED_COMMIT="$commit" "$scripts/verify-image.sh" "${refs[@]}"

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
refuses "another key" "signatures do not match" env ERP_COSIGN_PUBLIC_KEY="$work/other/cosign.pub" "$scripts/verify-image.sh" "${refs[0]}"
refuses "another commit" "was built from" env ERP_COSIGN_PUBLIC_KEY="$work/release/cosign.pub" ERP_EXPECTED_COMMIT=0000000000000000000000000000000000000000 \
  "$scripts/verify-image.sh" "${refs[0]}"
refuses "a reference by tag" "by digest, not by tag" env ERP_COSIGN_PUBLIC_KEY="$work/release/cosign.pub" "$scripts/verify-image.sh" "${refs[0]%@*}:latest"
# The same images pushed under another name carry no signature there:
# signatures live beside the image in its repository.
PUSH=true KO_DOCKER_REPO="$REPO-unsigned" "$scripts/images.sh" "$work/unsigned" >/dev/null
refuses "an unsigned image" "no signatures found" env ERP_COSIGN_PUBLIC_KEY="$work/release/cosign.pub" "$scripts/verify-image.sh" "$(head -1 "$work/unsigned/images.txt")"
echo "signing-roundtrip: ${#refs[@]} images signed and verified; the four refusals held"
