#!/usr/bin/env bash
# Scans the images for known vulnerabilities with grype (ADR-0032) and fails
# on any of severity high or above that has a fix. An image is the pinned
# base plus one Go binary, so two scans cover it:
#   - each per-platform SBOM that images.sh wrote (the Go modules in the
#     binary; ko's SBOM does not list the standard library, which
#     govulncheck covers in CI and weekly);
#   - the base image by its digest from .ko.yaml, for each platform (its
#     Debian packages, which ko's SBOM names but does not list).
# Exceptions go in .grype.yaml, each with a reason and an expiry date.
set -euo pipefail
OUT="${1:-dist/images}"
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
for platform in "${platforms[@]}"; do
  echo "image-scan: $base ($platform)"
  $GRYPE "registry:$base" --platform "$platform" "${args[@]}"
done
