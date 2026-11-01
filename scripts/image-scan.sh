#!/usr/bin/env bash
# Scans the images for known vulnerabilities with grype (ADR-0032) and fails
# on any of severity high or above that has a fix. An image is the pinned
# base plus one Go binary, so two scans cover it:
#   - each per-platform SBOM that images.sh wrote (the Go modules in the
#     binary; ko's SBOM does not list the standard library, which
#     govulncheck covers in CI and weekly; erp-analytics's, syft's, also
#     lists DuckDB's module and the cc base's packages);
#   - the base images by digest, for each platform: .ko.yaml's (its Debian
#     packages, which ko's SBOM names but does not list) and erp-analytics's
#     runtime base, the Dockerfile's BASE_IMAGE (glibc, libstdc++).
# It also scans the upstream images the release copies and signs
# (deploy/images/upstream.txt, scripts/mirror-images.sh), each by its
# pinned digest on the one platform copied, read as the copy reads them
# (scripts/lib/upstream.sh): the release runs this scan before it pushes
# anything, so an upstream image with a fixable high or critical
# vulnerability is never copied, signed or recorded (ADR-0032 amendment of
# 2026-10-31).
# Exceptions go in .grype.yaml, each with a reason and an expiry date.
set -euo pipefail
OUT="${1:-dist/images}"
# shellcheck source=scripts/lib/upstream.sh
. "$(dirname "$0")/lib/upstream.sh"
upstream="$(upstream_images deploy/images/upstream.txt)"
# grype comes from scripts/ci-tools.sh, which pins its version, caches the
# binary and retries a failed build, rather than from `go run` on every scan.
GRYPE="${GRYPE:-$(./scripts/ci-tools.sh)/grype}"
args=(--fail-on high --only-fixed --config .grype.yaml)
shopt -s nullglob
sboms=("$OUT"/sbom/*-linux-*.spdx.json)
if [ ${#sboms[@]} -eq 0 ]; then
  echo "image-scan: no SBOMs in $OUT/sbom (run make images first)" >&2
  exit 1
fi
for sbom in "${sboms[@]}"; do
  echo "image-scan: $(basename "$sbom")"
  $GRYPE "sbom:$sbom" "${args[@]}"
done
# .ko.yaml writes the base and the platforms one per line; a file in another
# shape would make this scan nothing, so it fails instead.
base="$(sed -n 's/^defaultBaseImage: *//p' .ko.yaml)"
read -r -a platforms <<<"$(sed -n '/^defaultPlatforms:/,/^[a-z]/s/^ *- *//p' .ko.yaml | tr '\n' ' ')"
if [[ "$base" != *@sha256:* ]] || [ ${#platforms[@]} -eq 0 ]; then
  echo "image-scan: .ko.yaml must name defaultBaseImage by digest and list defaultPlatforms one per line" >&2
  exit 1
fi
cc="$(sed -n 's/^ARG BASE_IMAGE=//p' deploy/images/erp-analytics.Dockerfile)"
if [[ "$cc" != *@sha256:* ]]; then
  echo "image-scan: deploy/images/erp-analytics.Dockerfile must name ARG BASE_IMAGE by digest" >&2
  exit 1
fi
for image in "$base" "$cc"; do
  for platform in "${platforms[@]}"; do
    echo "image-scan: $image ($platform)"
    $GRYPE "registry:$image" --platform "$platform" "${args[@]}"
  done
done
while read -r -u 3 name source tag digest; do
  echo "image-scan: upstream $name, $source:$tag@$digest ($upstream_platform)"
  $GRYPE "registry:$source:$tag@$digest" --platform "$upstream_platform" "${args[@]}"
done 3<<<"$upstream"
