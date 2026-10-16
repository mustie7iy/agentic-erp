#!/usr/bin/env bash
# Scans the images for known vulnerabilities with grype (ADR-0032) and fails
# on any of severity high or above that has a fix. An image is the pinned
# base plus one Go binary, so two scans cover it:
#   - each per-platform SBOM that images.sh wrote (the Go modules in the
#     binary; govulncheck also checks the code that is called);
#   - the base image by its digest from .ko.yaml, for each platform (its
#     Debian packages, which ko's SBOM names but does not list).
# Exceptions go in .grype.yaml, each with a reason and an expiry date.
set -euo pipefail
OUT="${1:-dist/images}"
GRYPE="${GRYPE:-go run github.com/anchore/grype/cmd/grype@v0.120.1}"
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
base="$(sed -n 's/^defaultBaseImage: *//p' .ko.yaml)"
for platform in $(sed -n '/^defaultPlatforms:/,/^[a-z]/s/^ *- *//p' .ko.yaml); do
  echo "image-scan: $base ($platform)"
  $GRYPE "registry:$base" --platform "$platform" "${args[@]}"
done
