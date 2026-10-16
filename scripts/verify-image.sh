#!/usr/bin/env bash
# Checks images before they are deployed (ADR-0032): each REF (an image by
# digest) must carry a signature by the release key, an SBOM attestation
# and a build provenance attestation signed by the same key. The
# provenance must name ERP_EXPECTED_COMMIT and a clean checkout;
# ERP_EXPECTED_COMMIT=any skips that check and has to be asked for, so a
# caller cannot forget it. It exits non-zero at the first image that
# fails. The release workflow runs it on what it pushed; delivery runs it
# again before a rollout (T40).
#
# ERP_COSIGN_PUBLIC_KEY verifies: the KMS key reference
# (awskms:///arn:...), whose public half cosign fetches, or a public key
# file. No transparency log is consulted: releases are not logged in one.
#
#   scripts/verify-image.sh registry/erp/erp-server@sha256:... [more refs]
set -euo pipefail
COSIGN="${COSIGN:-go run github.com/sigstore/cosign/v3/cmd/cosign@v3.1.1}"
key="${ERP_COSIGN_PUBLIC_KEY:?ERP_COSIGN_PUBLIC_KEY: the release key (awskms:///arn:... or a public key file)}"
expected="${ERP_EXPECTED_COMMIT:?ERP_EXPECTED_COMMIT: the commit the images must be built from, or any}"
if [ $# -eq 0 ]; then
  echo "usage: verify-image.sh <image@sha256:digest>..." >&2
  exit 2
fi
verify=(--key "$key" --insecure-ignore-tlog=true)
for ref in "$@"; do
  if [[ "$ref" != *@sha256:* ]]; then
    echo "verify-image: $ref: name images by digest, not by tag" >&2
    exit 1
  fi
  # cosign verify also accepts an attestation as a signature: look for an
  # image signature among what it verified.
  verified="$($COSIGN verify "${verify[@]}" "$ref")"
  if ! printf '%s\n' "$verified" | jq -e 'any(.[]; .critical.type == "https://sigstore.dev/cosign/sign/v1")' >/dev/null; then
    echo "verify-image: $ref: no image signature by the key, only attestations" >&2
    exit 1
  fi
  $COSIGN verify-attestation "${verify[@]}" --type spdxjson "$ref" >/dev/null
  provenance="$($COSIGN verify-attestation "${verify[@]}" --type slsaprovenance1 "$ref")"
  if [ "$expected" != any ]; then
    predicates="$(printf '%s\n' "$provenance" | jq -c '.payload | @base64d | fromjson | .predicate')"
    commits="$(printf '%s\n' "$predicates" | jq -r '.buildDefinition.resolvedDependencies[] | .digest.gitCommit // empty' | sort -u)"
    if [ "$commits" != "$expected" ]; then
      echo "verify-image: $ref was built from ${commits:-no commit}, want $expected" >&2
      exit 1
    fi
    if printf '%s\n' "$predicates" | jq -e 'select(.buildDefinition.internalParameters.sourceModified != false)' >/dev/null; then
      echo "verify-image: $ref was built from uncommitted changes" >&2
      exit 1
    fi
  fi
  echo "verify-image: $ref: signature, SBOM and provenance verified"
done
