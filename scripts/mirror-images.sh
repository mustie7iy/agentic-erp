#!/usr/bin/env bash
# Copies the upstream images of deploy/images/upstream.txt into the release
# registry (ADR-0035), each by its pinned digest into erp-<name>, tagged with
# its upstream tag, and appends them to OUT (default dist/images) in the
# layout scripts/images.sh writes, so that sign-images.sh signs and attests
# them like the platform's own:
#   OUT/images.txt  one reference per line, by digest;
#   OUT/sbom/       an SPDX SBOM per image (syft), its first package the
#                   image's digest, as sign-images.sh looks it up;
#   OUT/provenance/ a SLSA v1 provenance per image saying what it is: a
#                   copy of source@digest by this repository's release,
#                   not a build of it. sign-images.sh attests it in place
#                   of the release's build provenance (when T39's and
#                   T40's scripts meet; ADR-0035).
# The copy keeps the digest: the source must serve exactly the pinned
# manifest, and a tag already in the registry must name it, since release
# repositories' tags are immutable. REGISTRY is the release registry's host
# (tooling's ECR, the release workflow's ERP_RELEASE_REGISTRY). CRANE and
# SYFT name the binaries; INSECURE=true allows a plain-HTTP registry (the
# tests' only).
set -euo pipefail
OUT="${1:-dist/images}"
LIST="${LIST:-$(dirname "$0")/../deploy/images/upstream.txt}"
CRANE="${CRANE:-crane}"
SYFT="${SYFT:-syft}"
registry="${REGISTRY:?REGISTRY: the release registry host, e.g. 111111111111.dkr.ecr.eu-central-1.amazonaws.com}"
insecure=()
if [ "${INSECURE:-false}" = true ]; then
  insecure=(--insecure)
  export SYFT_REGISTRY_INSECURE_USE_HTTP=true
fi
export SYFT_CHECK_FOR_APP_UPDATE=false
mkdir -p "$OUT/sbom"
copied=0
while read -r -u 3 name source tag digest; do
  case "$name" in '' | '#'*) continue ;; esac
  if ! [[ "$name" =~ ^[a-z][a-z0-9-]{0,40}$ ]]; then
    echo "mirror-images: $name is not a repository name (lower-case letters, digits, hyphens)" >&2
    exit 1
  fi
  if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "mirror-images: $name: $digest is not a sha256 digest" >&2
    exit 1
  fi
  target="$registry/erp-$name"
  # The source must still serve the pinned manifest: crane copies what it
  # is given, and the digest is what everything downstream trusts.
  served="$("$CRANE" digest ${insecure[@]+"${insecure[@]}"} "$source@$digest")"
  if [ "$served" != "$digest" ]; then
    echo "mirror-images: $source served $served for $digest" >&2
    exit 1
  fi
  # One linux/arm64 image, not an index: the cells' platform, signed as
  # one image.
  kind="$("$CRANE" manifest ${insecure[@]+"${insecure[@]}"} "$source@$digest" | python3 -c 'import json, sys; m = json.load(sys.stdin); print("index" if "manifests" in m else "image")')"
  if [ "$kind" != image ]; then
    echo "mirror-images: $source@$digest is an index, want one linux/arm64 image" >&2
    exit 1
  fi
  platform="$("$CRANE" config ${insecure[@]+"${insecure[@]}"} "$source@$digest" | python3 -c 'import json, sys; c = json.load(sys.stdin); print("%s/%s" % (c.get("os"), c.get("architecture")))')"
  if [ "$platform" != linux/arm64 ]; then
    echo "mirror-images: $source@$digest is $platform, want one linux/arm64 image" >&2
    exit 1
  fi
  if existing="$("$CRANE" digest ${insecure[@]+"${insecure[@]}"} "$target:$tag" 2>/dev/null)"; then
    if [ "$existing" != "$digest" ]; then
      echo "mirror-images: $target:$tag already names $existing, not $digest" >&2
      exit 1
    fi
  else
    "$CRANE" copy ${insecure[@]+"${insecure[@]}"} "$source@$digest" "$target:$tag" >/dev/null
  fi
  copied_digest="$("$CRANE" digest ${insecure[@]+"${insecure[@]}"} "$target:$tag")"
  if [ "$copied_digest" != "$digest" ]; then
    echo "mirror-images: $target:$tag is $copied_digest after the copy, not $digest" >&2
    exit 1
  fi
  ref="$target@$digest"
  sbom="$OUT/sbom/erp-$name.spdx.json"
  "$SYFT" scan "registry:$ref" -q -o "spdx-json=$sbom.raw"
  # The image itself first, named by its digest: sign-images.sh finds an
  # image's SBOM by .packages[0].name, as ko writes them.
  python3 - "$sbom.raw" "$sbom" "$digest" "$source:$tag" <<'PY'
import json, sys
raw, out, digest, origin = sys.argv[1:]
doc = json.load(open(raw))
roots = [p for p in doc["packages"] if p.get("primaryPackagePurpose") == "CONTAINER"]
if len(roots) != 1:
    sys.exit("mirror-images: %s: %d container packages in the SBOM, want 1" % (origin, len(roots)))
root = roots[0]
root["name"], root["sourceInfo"] = digest, "copied from %s@%s" % (origin, digest)
doc["packages"] = [root] + [p for p in doc["packages"] if p is not root]
json.dump(doc, open(out, "w"))
PY
  rm -f "$sbom.raw"
  mkdir -p "$OUT/provenance"
  python3 - "$OUT/provenance/erp-$name.json" "$source" "$tag" "$digest" "$ref" <<'PY'
import json, sys
out, source, tag, digest, ref = sys.argv[1:]
json.dump({
    "buildDefinition": {
        "buildType": "https://github.com/mustie7iy/agentic-erp/mirror-images@v1",
        "externalParameters": {"source": source, "tag": tag, "digest": digest},
        "resolvedDependencies": [{"uri": "%s:%s" % (source, tag), "digest": {"sha256": digest.split(":", 1)[1]}}],
    },
    "runDetails": {"builder": {"id": "https://github.com/mustie7iy/agentic-erp/scripts/mirror-images.sh"},
                   "metadata": {"invocationId": ref}},
}, open(out, "w"))
PY
  echo "$ref" >>"$OUT/images.txt"
  echo "mirror-images: $ref"
  copied=$((copied + 1))
done 3<"$LIST"
if [ "$copied" -eq 0 ]; then
  echo "mirror-images: $LIST lists no image" >&2
  exit 1
fi
