#!/usr/bin/env bash
# Checks images before they are deployed (ADR-0032): each REF (an image by
# digest) must carry a signature by the release key, an SBOM attestation
# and a build provenance attestation signed by the same key. The
# provenance must name ERP_EXPECTED_COMMIT and a clean checkout;
# ERP_EXPECTED_COMMIT=any skips that check and has to be asked for, so a
# caller cannot forget it. An upstream image the release copied
# (scripts/mirror-images.sh) carries a copy's provenance instead, naming
# the digest it copied, which must be the image's: the same copy is
# attested again by every release that pins it, so such an image passes
# when one of its copies names ERP_EXPECTED_COMMIT
# from a clean checkout; a built image must
# name that commit alone (ADR-0032 amendment of 2026-10-28, T72i). It
# exits non-zero at the first image that fails. The release workflow runs
# it on what it pushed; delivery runs it again before a rollout (T40).
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
copy=https://github.com/mustie7iy/agentic-erp/mirror-images@v1
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
  predicates="$(printf '%s\n' "$provenance" | jq -c '.payload | @base64d | fromjson | .predicate')"
  # A copy's provenance names the digest it copied; any other image's
  # provenance is a build's. One image is one or the other.
  kinds="$(printf '%s\n' "$predicates" | jq -r --arg copy "$copy" 'if .buildDefinition.buildType == $copy then "copy" else "build" end' | sort -u)"
  if [ "$kinds" = copy ]; then
    # Asserted positively, so that a jq error refuses too.
    if ! printf '%s\n' "$predicates" | jq -e -s --arg digest "${ref##*@}" 'all(.[]; .buildDefinition.externalParameters.digest == $digest)' >/dev/null; then
      echo "verify-image: $ref: a copy's provenance names another digest than the image's" >&2
      exit 1
    fi
  elif [ "$kinds" != build ]; then
    echo "verify-image: $ref: its provenance says both copy and build" >&2
    exit 1
  fi
  if [ "$expected" != any ] && [ "$kinds" = copy ]; then
    # Only the expected release's copy counts: other releases' attestations
    # of the same digest, a failed one's among them, neither help nor harm.
    if ! printf '%s\n' "$predicates" | jq -e -s --arg c "$expected" 'any(.[]; (.buildDefinition.resolvedDependencies | any(.digest.gitCommit == $c))
        and .buildDefinition.internalParameters.sourceModified == false)' >/dev/null; then
      echo "verify-image: $ref was not copied by the release at $expected from a clean checkout" >&2
      exit 1
    fi
  elif [ "$expected" != any ]; then
    commits="$(printf '%s\n' "$predicates" | jq -r '.buildDefinition.resolvedDependencies[] | .digest.gitCommit // empty' | sort -u)"
    if [ "$commits" != "$expected" ]; then
      echo "verify-image: $ref was built from ${commits:-no commit}, want $expected" >&2
      exit 1
    fi
    # Asserted positively, as above: a jq error (a provenance it cannot
    # read) refuses the image rather than reading as a clean checkout.
    if ! printf '%s\n' "$predicates" | jq -e -s 'all(.[]; .buildDefinition.internalParameters.sourceModified == false)' >/dev/null; then
      echo "verify-image: $ref was built from uncommitted changes, or its provenance does not say it was not" >&2
      exit 1
    fi
  fi
  echo "verify-image: $ref: signature, SBOM and provenance verified"
done
