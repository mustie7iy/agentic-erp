#!/usr/bin/env bash
# Builds the container images of the three binaries with ko (.ko.yaml,
# ADR-0032), into OUT (default dist/images), and pushes them to
# KO_DOCKER_REPO when PUSH=true (the release workflow); otherwise nothing
# leaves the machine:
#   OUT/images.txt  one image reference per line, the multi-platform index
#                   of each binary first, then its per-platform images, all
#                   by digest;
#   OUT/sbom/       an SPDX SBOM per image (Go modules and the base image).
# KO_DOCKER_REPO names the images (default erp.local, which cannot be
# pushed to); ERP_VERSION stamps the release version into the binaries.
# KO_TAGS is the tag a push gives them (default latest; releases tag the
# version, never a moving tag, since an image is trusted by digest after
# verification, not by tag). A push also uploads ko's SBOMs as
# sha256-<digest>.sbom tags: unsigned copies; the SBOM a verifier trusts is
# the attestation sign-images.sh signs. GOCACHE and KOCACHE are honoured, so the
# reproducibility gate can give each build caches of its own.
set -euo pipefail
OUT="${1:-dist/images}"
KO="${KO:-go run github.com/google/ko@v0.19.1}"
rm -rf "$OUT"
mkdir -p "$OUT/sbom"
KO_DOCKER_REPO="${KO_DOCKER_REPO:-erp.local}" ERP_VERSION="${ERP_VERSION:-}" $KO build --push="${PUSH:-false}" --base-import-paths \
  --tags="${KO_TAGS:-latest}" \
  --sbom=spdx --sbom-dir="$OUT/sbom" --image-refs="$OUT/images.txt" \
  ./cmd/erp-server ./cmd/erp-router ./cmd/erp >/dev/null
cat "$OUT/images.txt"
