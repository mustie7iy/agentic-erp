#!/usr/bin/env bash
# The reproducibility gate (ADR-0032): builds the images twice, each time
# with empty Go and ko caches and a buildx builder of its own (for
# erp-analytics: a docker-container builder, removed with its cache after
# the build), and fails unless every image has the same digest both times. An image whose digest depends on the machine,
# the time or a cache cannot be verified against its source, so it is not
# released. The caches are private temporary directories, removed at the
# end: the shared Go build cache is never cleaned. The first build's caches
# are removed before the second build starts, so the two never take the
# disk together. The first build is kept in OUT (default dist/images) for
# the scan.
set -euo pipefail
OUT="${1:-dist/images}"
DOCKER="${DOCKER:-docker}"
work="$(mktemp -d)"
builder=""
# A failed build's builder goes too: its state would hold the cache.
cleanup() {
  if [ -n "$builder" ]; then $DOCKER buildx rm --force "$builder" >/dev/null 2>&1 || true; fi
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
for run in first second; do
  mkdir -p "$work/$run/gocache" "$work/$run/kocache"
  builder="erp-reproducible-$run-$$"
  $DOCKER buildx create --name "$builder" --driver docker-container >/dev/null
  GOCACHE="$work/$run/gocache" KOCACHE="$work/$run/kocache" BUILDX_BUILDER="$builder" \
    GOFLAGS="${GOFLAGS:+$GOFLAGS }-modcacherw" \
    "$(dirname "$0")/images.sh" "$work/$run/out" >/dev/null
  $DOCKER buildx rm --force "$builder" >/dev/null
  builder=""
  # The module cache may hold read-only files; if chmod cannot fix one,
  # the rm below fails loudly.
  chmod -R u+w "$work/$run/gocache" "$work/$run/kocache" 2>/dev/null || true
  rm -rf "$work/$run/gocache" "$work/$run/kocache"
  echo "images-reproducible: $run build done, $(df -h / | tail -1)"
done
if ! diff -u "$work/first/out/images.txt" "$work/second/out/images.txt"; then
  echo "images-reproducible: the two builds produced different digests (above)" >&2
  exit 1
fi
rm -rf "$OUT"
mkdir -p "$(dirname "$OUT")"
mv "$work/first/out" "$OUT"
echo "images-reproducible: $(wc -l <"$OUT/images.txt" | tr -d ' ') image digests identical across two clean builds"
cat "$OUT/images.txt"
