#!/usr/bin/env bash
# Signs the images a pushed build wrote to OUT (default dist/images, by
# scripts/images.sh with PUSH=true) and attaches two signed attestations to
# each (ADR-0032):
#   - its SPDX SBOM (type spdxjson), the one ko wrote for that image;
#   - the build provenance of scripts/provenance.sh (type slsaprovenance1).
# Every reference in images.txt is signed: each multi-platform index and
# each per-platform image, so a verifier holding either digest finds a
# signature.
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
signed=0
for sbom in "$OUT"/sbom/*.spdx.json; do
  digest="$(jq -r '.packages[0].name' "$sbom")"
  ref="$(grep -E "@${digest}\$" "$OUT/images.txt" || true)"
  if [ "$(printf '%s\n' "$ref" | grep -c .)" -ne 1 ]; then
    echo "sign-images: $(basename "$sbom") describes $digest, which images.txt lists $(printf '%s\n' "$ref" | grep -c .) times" >&2
    exit 1
  fi
  $COSIGN sign --yes --key "$key" --signing-config "$config" "$ref"
  $COSIGN attest --yes --key "$key" --signing-config "$config" --type spdxjson --predicate "$sbom" "$ref"
  $COSIGN attest --yes --key "$key" --signing-config "$config" --type slsaprovenance1 --predicate "$provenance" "$ref"
  signed=$((signed + 1))
done
if [ "$signed" -ne "$(grep -c . "$OUT/images.txt")" ]; then
  echo "sign-images: signed $signed images of $(grep -c . "$OUT/images.txt") in images.txt: an image has no SBOM" >&2
  exit 1
fi
echo "sign-images: $signed images signed, each with its SBOM and the build provenance attested"
