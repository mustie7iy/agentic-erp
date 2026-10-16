#!/usr/bin/env bash
# The reproducibility gate (ADR-0032): builds the images twice, each time
# with empty Go and ko caches of its own, and fails unless every image has
# the same digest both times. An image whose digest depends on the machine,
# the time or a cache cannot be verified against its source, so it is not
# released. The caches are private temporary directories, removed at the
# end: the shared Go build cache is never cleaned. The first build is kept
# in OUT (default dist/images) for the scan.
set -euo pipefail
OUT="${1:-dist/images}"
work="$(mktemp -d)"
trap 'chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT
for run in first second; do
  mkdir -p "$work/$run/gocache" "$work/$run/kocache"
  GOCACHE="$work/$run/gocache" KOCACHE="$work/$run/kocache" GOFLAGS=-modcacherw \
    "$(dirname "$0")/images.sh" "$work/$run/out" >/dev/null
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
