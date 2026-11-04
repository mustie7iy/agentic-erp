#!/usr/bin/env bash
# Builds the container images of the six binaries (ADR-0032) into OUT
# (default dist/images), and pushes them to KO_DOCKER_REPO when PUSH=true
# (the release workflow); otherwise nothing leaves the machine:
#   OUT/images.txt  one image reference per line, the multi-platform index
#                   of each binary first, then its per-platform images, all
#                   by digest;
#   OUT/sbom/       an SPDX SBOM per image (Go modules and the base image),
#                   its first package the image's digest.
# erp-server, erp-router, erp-web and erp are ko's (.ko.yaml), built one
# binary and platform at a time (--jobs=1): ko otherwise runs GOMAXPROCS go
# builds at once, and three of them compile api/gen, about 3 GB each with
# an empty cache: on CI's 2-CPU, 7938 MiB runner erp-server built for both
# platforms at once and the runner was stopped (main's full runs
# 40460447291 and 40547406558); measured cold at GOMAXPROCS=2, ko's build
# peaks at 6484 MiB that way and 3843 MiB one at a time. erp-temporal,
# Temporal's server with the platform's authorization (ADR-0035), is a Go
# module of its own (temporal/): ko builds it from that directory, since ko
# finds builds in its working directory's module only, with a configuration
# written from .ko.yaml's base and platforms (the base is pinned once),
# static and one at a time like the four, listed after them; it has no
# buildinfo package, so no version is stamped into it. erp-analytics links
# DuckDB through cgo (ADR-0044), which ko does not cross-compile: docker
# buildx builds it from deploy/images/erp-analytics.Dockerfile one platform
# at a time (each compiles api/gen in a few GB of the Docker VM's memory),
# each an OCI archive dated at the epoch; the per-platform images are
# joined into one OCI index here, syft writes their SBOMs, and crane pushes
# the index. The buildx builder must export OCI archives (Docker Desktop's,
# or a docker-container builder, as CI sets up); BUILDX_BUILDER names one.
# KO_DOCKER_REPO names the images (default erp.local, which cannot be
# pushed to); ERP_VERSION stamps the release version into the binaries.
# KO_TAGS is the tag a push gives them (default latest; releases tag the
# version, never a moving tag, since an image is trusted by digest after
# verification, not by tag). A push also uploads ko's SBOMs as
# sha256-<digest>.sbom tags: unsigned copies; the SBOM a verifier trusts is
# the attestation sign-images.sh signs. GOCACHE, KOCACHE and BUILDX_BUILDER
# are honoured, so the reproducibility gate can give each build caches of
# its own. KO_PLATFORMS (e.g. linux/amd64) builds those platforms instead
# of .ko.yaml's, for every image: pull requests check one, to fit the
# runner's disk; the weekly run builds them all, and releases refuse the
# setting. DOCKER, SYFT and CRANE name the tools.
set -euo pipefail
OUT="${1:-dist/images}"
KO="${KO:-go run github.com/google/ko@v0.19.1}"
DOCKER="${DOCKER:-docker}"
SYFT="${SYFT:-go run github.com/anchore/syft/cmd/syft@v1.52.0}"
CRANE="${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.21.7}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="${KO_DOCKER_REPO:-erp.local}"
rm -rf "$OUT"
mkdir -p "$OUT/sbom"
KO_DOCKER_REPO="$repo" ERP_VERSION="${ERP_VERSION:-}" $KO build --jobs=1 --push="${PUSH:-false}" --base-import-paths \
  --tags="${KO_TAGS:-latest}" ${KO_PLATFORMS:+--platform="$KO_PLATFORMS"} \
  --sbom=spdx --sbom-dir="$OUT/sbom" --image-refs="$OUT/images.txt" \
  ./cmd/erp-server ./cmd/erp-router ./cmd/erp-web ./cmd/erp >/dev/null
work="$(mktemp -d)"
# shellcheck source=lib/on-exit.sh
. "$here/lib/on-exit.sh"
on_exit 'rm -rf "$work"'

# erp-temporal, from its own module. OUT is made absolute: ko runs in
# temporal/.
out="$(cd "$OUT" && pwd)"
koconfig="$work/temporal.ko.yaml"
sed -n '/^defaultBaseImage:/p; /^defaultPlatforms:/,/^[a-z]/{/^defaultPlatforms:/p; /^ *- /p;}' "$here/../.ko.yaml" >"$koconfig"
if ! grep -q '^defaultBaseImage: .*@sha256:' "$koconfig" || ! grep -q '^ *- ' "$koconfig"; then
  echo "images: .ko.yaml must name defaultBaseImage by digest and list defaultPlatforms one per line (erp-temporal's configuration is written from them)" >&2
  exit 1
fi
cat >>"$koconfig" <<'YAML'
builds:
  - id: erp-temporal
    main: ./cmd/erp-temporal
    env: [CGO_ENABLED=0]
    flags: [-trimpath]
    ldflags: ["-s -w -buildid="]
YAML
(cd "$here/../temporal" && KO_CONFIG_PATH="$koconfig" KO_DOCKER_REPO="$repo" $KO build --jobs=1 --push="${PUSH:-false}" --base-import-paths \
  --tags="${KO_TAGS:-latest}" ${KO_PLATFORMS:+--platform="$KO_PLATFORMS"} \
  --sbom=spdx --sbom-dir="$out/sbom" --image-refs="$work/temporal.txt" ./cmd/erp-temporal >/dev/null)
# ko lists every image of a multi-platform index as repo/name@digest, but
# one image (one platform) as repo/name:tag@digest when it does not push it
# (ko's no-op publisher: ci-local's reproducibility gate and scan) or pushes
# it under one tag other than latest, as it lists the four above.
if ! grep -qE "^$repo/erp-temporal(:[^@/]+)?@sha256:[0-9a-f]{64}$" "$work/temporal.txt"; then
  echo "images: ko listed no erp-temporal image" >&2
  exit 1
fi
cat "$work/temporal.txt" >>"$OUT/images.txt"

# erp-analytics, for the same platforms.
if [ -n "${KO_PLATFORMS:-}" ]; then
  IFS=, read -r -a platforms <<<"$KO_PLATFORMS"
else
  read -r -a platforms <<<"$(sed -n '/^defaultPlatforms:/,/^[a-z]/s/^ *- *//p' "$here/../.ko.yaml" | tr '\n' ' ')"
fi
if [ ${#platforms[@]} -eq 0 ]; then
  echo "images: no platform to build erp-analytics for (.ko.yaml's defaultPlatforms, one per line, or KO_PLATFORMS)" >&2
  exit 1
fi
for platform in "${platforms[@]}"; do
  if ! [[ "$platform" =~ ^[a-z0-9]+/[a-z0-9]+(/v[0-9])?$ ]]; then
    echo "images: \"$platform\" is not one platform (os/arch, e.g. linux/arm64)" >&2
    exit 1
  fi
done
for platform in "${platforms[@]}"; do
  # No buildx attestations: the SBOM and provenance are signed attestations
  # of their own (sign-images.sh), and an attestation manifest would make
  # the archive an index rather than the image.
  $DOCKER buildx build ${BUILDX_BUILDER:+--builder "$BUILDX_BUILDER"} -f "$here/../deploy/images/erp-analytics.Dockerfile" \
    --platform "$platform" --provenance=false --sbom=false \
    --build-arg SOURCE_DATE_EPOCH=0 --build-arg ERP_VERSION="${ERP_VERSION:-}" \
    --output "type=oci,dest=$work/${platform//\//-}.tar,rewrite-timestamp=true" "$here/.." >&2
done
mkdir -p "$work/layout"
# The archives joined into one OCI layout whose index.json is the
# multi-platform index (written here, so its digest is the hash of the
# file crane pushes); printed: the index's digest, then each platform's
# "os-arch digest archive".
python3 - "$work/layout" "$work"/*.tar >"$work/digests" <<'PY'
import hashlib, json, os, re, sys, tarfile

layout, archives = sys.argv[1], sys.argv[2:]
manifests, lines = [], []
for archive in archives:
    asked = os.path.basename(archive)[: -len(".tar")]
    with tarfile.open(archive) as tar:
        index = json.load(tar.extractfile("index.json"))
        if len(index.get("manifests", [])) != 1:
            sys.exit("images: %s holds %d manifests, want one image" % (archive, len(index.get("manifests", []))))
        desc = index["manifests"][0]
        if desc["mediaType"] != "application/vnd.oci.image.manifest.v1+json":
            sys.exit("images: %s holds a %s, want one image manifest" % (archive, desc["mediaType"]))
        for member in tar.getmembers():
            if member.isfile() and re.fullmatch(r"blobs/sha256/[0-9a-f]{64}", member.name):
                dest = os.path.join(layout, member.name)
                os.makedirs(os.path.dirname(dest), exist_ok=True)
                with open(dest, "wb") as f:
                    f.write(tar.extractfile(member).read())
    blob = lambda d: json.load(open(os.path.join(layout, "blobs", *d.split(":", 1))))
    config = blob(blob(desc["digest"])["config"]["digest"])
    platform = {"architecture": config["architecture"], "os": config["os"]}
    if config.get("variant"):
        platform["variant"] = config["variant"]
    # The variant (arm64's v8) is not in the name asked for.
    built = "%s-%s" % (platform["os"], platform["architecture"])
    if asked != built and not asked.startswith(built + "-"):
        sys.exit("images: %s holds a %s image" % (archive, built))
    manifests.append({"mediaType": desc["mediaType"], "digest": desc["digest"], "size": desc["size"], "platform": platform})
    lines.append("%s-%s %s %s" % (platform["os"], platform["architecture"], desc["digest"], archive))
manifests.sort(key=lambda m: (m["platform"]["os"], m["platform"]["architecture"]))
raw = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": manifests},
                 separators=(",", ":"), sort_keys=True).encode()
open(os.path.join(layout, "index.json"), "wb").write(raw)
open(os.path.join(layout, "oci-layout"), "w").write('{"imageLayoutVersion":"1.0.0"}')
print("sha256:" + hashlib.sha256(raw).hexdigest())
print("\n".join(sorted(lines)))
PY
index="$(head -1 "$work/digests")"
if [ "${PUSH:-false}" = true ]; then
  IFS=, read -r -a tags <<<"${KO_TAGS:-latest}"
  for tag in "${tags[@]}"; do
    pushed="$($CRANE push --index "$work/layout" "$repo/erp-analytics:$tag")"
    if [ "${pushed##*@}" != "$index" ]; then
      echo "images: $repo/erp-analytics:$tag was pushed as ${pushed##*@}, not the index built, $index" >&2
      exit 1
    fi
  done
fi
export SYFT_CHECK_FOR_APP_UPDATE=false
echo "$repo/erp-analytics@$index" >>"$OUT/images.txt"
platform_sboms=()
while read -r platform digest archive; do
  sbom="$OUT/sbom/erp-analytics-$platform.spdx.json"
  $SYFT scan "oci-archive:$archive" -q -o "spdx-json=$sbom.raw"
  python3 "$here/lib/sbom-root.py" "$sbom.raw" "$sbom" "$digest" "built from deploy/images/erp-analytics.Dockerfile for $platform"
  rm -f "$sbom.raw"
  echo "$repo/erp-analytics@$digest" >>"$OUT/images.txt"
  platform_sboms+=("$sbom")
done < <(tail -n +2 "$work/digests")
# The index's SBOM names the index first, then the images it holds, as
# ko's index SBOMs do.
python3 - "$OUT/sbom/erp-analytics-index.spdx.json" "$index" "${platform_sboms[@]}" <<'PY'
import json, sys

out, index, sboms = sys.argv[1], sys.argv[2], sys.argv[3:]
package = lambda i, name: {"name": name, "SPDXID": "SPDXRef-Package-%d" % i, "downloadLocation": "NOASSERTION",
                           "primaryPackagePurpose": "CONTAINER", "filesAnalyzed": False}
packages = [package(0, index)] + [package(i + 1, json.load(open(s))["packages"][0]["name"]) for i, s in enumerate(sboms)]
json.dump({
    "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT", "name": "erp-analytics-index",
    "documentNamespace": "https://github.com/mustie7iy/agentic-erp/sbom/erp-analytics/" + index,
    "creationInfo": {"created": "1970-01-01T00:00:00Z", "creators": ["Tool: scripts/images.sh"]},
    "packages": packages,
    "relationships": [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": "SPDXRef-Package-0"}]
    + [{"spdxElementId": "SPDXRef-Package-0", "relationshipType": "CONTAINS", "relatedSpdxElement": p["SPDXID"]} for p in packages[1:]],
}, open(out, "w"), indent=1)
PY
cat "$OUT/images.txt"
finished
