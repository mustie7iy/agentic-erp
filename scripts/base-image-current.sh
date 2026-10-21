#!/usr/bin/env bash
# Fails when the tag of a pinned base image now points at another digest
# than the one pinned (ADR-0032): a new release, usually with security
# fixes, that the images should take in a reviewed change bumping the
# digest. The pins: .ko.yaml's base, and erp-analytics's builder and
# runtime base (deploy/images/erp-analytics.Dockerfile, ARG GO_IMAGE and
# BASE_IMAGE). Every pin is checked before it fails. The weekly run calls
# it. CRANE names the tool.
set -euo pipefail
cd "$(dirname "$0")/.."
CRANE="${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.21.7}"
dockerfile=deploy/images/erp-analytics.Dockerfile
pins=(".ko.yaml $(sed -n 's/^defaultBaseImage: *//p' .ko.yaml)"
  "$dockerfile $(sed -n 's/^ARG GO_IMAGE=//p' "$dockerfile")"
  "$dockerfile $(sed -n 's/^ARG BASE_IMAGE=//p' "$dockerfile")")
moved=0
for pin in "${pins[@]}"; do
  read -r file pinned <<<"$pin"
  if [[ "$pinned" != *@sha256:* ]]; then
    echo "base-image-current: $file pins no base by digest (${pinned:-none})" >&2
    exit 1
  fi
  tag="${pinned%@*}"
  current="$($CRANE digest "$tag")"
  if [ "$current" != "${pinned#*@}" ]; then
    echo "base-image-current: $tag is now $current; $file pins ${pinned#*@}: bump the digest" >&2
    moved=1
  else
    echo "base-image-current: $tag is still ${pinned#*@}"
  fi
done
exit "$moved"
