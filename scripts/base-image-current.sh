#!/usr/bin/env bash
# Fails when the tag of the base image in .ko.yaml now points at another
# digest than the one pinned there (ADR-0032): a new distroless release,
# usually with security fixes, that the images should take in a reviewed
# change bumping the digest. The nightly run calls it.
set -euo pipefail
CRANE="${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.21.7}"
pinned="$(sed -n 's/^defaultBaseImage: *//p' .ko.yaml)"
tag="${pinned%@*}"
current="$($CRANE digest "$tag")"
if [ "$current" != "${pinned#*@}" ]; then
  echo "base-image-current: $tag is now $current; .ko.yaml pins ${pinned#*@}: bump the digest" >&2
  exit 1
fi
echo "base-image-current: $tag is still ${pinned#*@}"
