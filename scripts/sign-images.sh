#!/usr/bin/env bash
# Signs the images a pushed build wrote to OUT (default dist/images, by
# scripts/images.sh with PUSH=true) and attaches two signed attestations to
# each (ADR-0032):
#   - its SPDX SBOM (type spdxjson), the one ko wrote for that image;
#   - the build provenance of scripts/provenance.sh (type slsaprovenance1).
# Every reference in images.txt is signed: each multi-platform index and
# each per-platform image, so a verifier holding either digest finds a
# signature. Signing a release again adds signatures and attestations
# beside the earlier ones; verification accepts either.
#
# ERP_COSIGN_KEY is the signing key: in releases the AWS KMS key,
# awskms:///arn:aws:kms:<region>:<account>:key/<id>; in tests a cosign key
# file. Nothing goes to a transparency log: release/signing-config.json
# names no Sigstore service, because the repository is private and Rekor is
# public (ADR-0032). COSIGN_PASSWORD unlocks a key file.
set -euo pipefail
OUT="${1:-dist/images}"
COSIGN="${COSIGN:-go run github.com/sigstore/cosign/v3/cmd/cosign@v3.1.1}"
key="${ERP_COSIGN_KEY:?ERP_COSIGN_KEY: the signing key (awskms:///arn:... or a cosign key file)}"
config="$(dirname "$0")/../release/signing-config.json"
provenance="$(mktemp)"
trap 'rm -f "$provenance"' EXIT
"$(dirname "$0")/provenance.sh" >"$provenance"
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
signed=0
while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  if ! sbom="$(sbom_for "${ref##*@}")"; then
    echo "sign-images: $ref: no single SBOM in $OUT/sbom describes it" >&2
    exit 1
  fi
  $COSIGN sign --yes --key "$key" --signing-config "$config" "$ref"
  $COSIGN attest --yes --key "$key" --signing-config "$config" --type spdxjson --predicate "$sbom" "$ref"
  $COSIGN attest --yes --key "$key" --signing-config "$config" --type slsaprovenance1 --predicate "$provenance" "$ref"
  signed=$((signed + 1))
done <"$OUT/images.txt"
if [ "$signed" -eq 0 ]; then
  echo "sign-images: $OUT/images.txt lists no image" >&2
  exit 1
fi
echo "sign-images: $signed images signed, each with its SBOM and the build provenance attested"
