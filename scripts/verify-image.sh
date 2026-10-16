#!/usr/bin/env bash
# Checks images before they are deployed (ADR-0032): each REF (an image by
# digest) must carry a signature by the release key, an SBOM attestation
# and a build provenance attestation signed by the same key, and, when
# ERP_EXPECTED_COMMIT is set, the provenance must name that commit. It exits
# non-zero at the first image that fails. The release workflow runs it on
# what it pushed; delivery runs it again before a rollout (T40).
#
# ERP_COSIGN_PUBLIC_KEY verifies: the KMS key reference
# (awskms:///arn:...), whose public half cosign fetches, or a public key
# file. No transparency log is consulted: releases are not logged in one.
#
#   scripts/verify-image.sh registry/erp/erp-server@sha256:... [more refs]
set -euo pipefail
COSIGN="${COSIGN:-go run github.com/sigstore/cosign/v3/cmd/cosign@v3.1.1}"
key="${ERP_COSIGN_PUBLIC_KEY:?ERP_COSIGN_PUBLIC_KEY: the release key (awskms:///arn:... or a public key file)}"
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
  $COSIGN verify "${verify[@]}" "$ref" >/dev/null
  $COSIGN verify-attestation "${verify[@]}" --type spdxjson "$ref" >/dev/null
  provenance="$($COSIGN verify-attestation "${verify[@]}" --type slsaprovenance1 "$ref")"
  if [ -n "${ERP_EXPECTED_COMMIT:-}" ]; then
    commits="$(printf '%s\n' "$provenance" | jq -r '.payload | @base64d | fromjson
      | .predicate.buildDefinition.resolvedDependencies[] | .digest.gitCommit // empty' | sort -u)"
    if [ "$commits" != "$ERP_EXPECTED_COMMIT" ]; then
      echo "verify-image: $ref was built from ${commits:-no commit}, want $ERP_EXPECTED_COMMIT" >&2
      exit 1
    fi
  fi
  echo "verify-image: $ref: signature, SBOM and provenance verified"
done
