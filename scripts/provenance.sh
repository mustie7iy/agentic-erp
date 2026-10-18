#!/usr/bin/env bash
# Writes the SLSA v1 build provenance of this build (ADR-0032) as JSON: the
# predicate cosign attests to each image (scripts/sign-images.sh). It names
# the commit the images were built from and whether the checkout had
# uncommitted changes, the workflow run that built them, the base image and
# the ko version, so a verifier can tell which source and which build an
# image came from. In GitHub Actions the GITHUB_* variables describe the run;
# outside it, the local checkout does. ERP_BUILD_STARTED (RFC 3339), when the
# caller recorded it before the build, is the build's start.
set -euo pipefail
cd "$(dirname "$0")/.."
server="${GITHUB_SERVER_URL:-https://github.com}"
repo="${GITHUB_REPOSITORY:-mustie7iy/agentic-erp}"
commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"
ref="${GITHUB_REF:-$(git symbolic-ref -q HEAD || echo detached)}"
modified=false
if [ -n "$(git status --porcelain)" ]; then # untracked .go files are compiled too
  modified=true
fi
# GITHUB_WORKFLOW_REF is owner/repo/.github/workflows/<file>@<ref>.
if [ -n "${GITHUB_WORKFLOW_REF:-}" ]; then
  builder="$server/$GITHUB_WORKFLOW_REF"
  run="$server/$repo/actions/runs/${GITHUB_RUN_ID:?}/attempts/${GITHUB_RUN_ATTEMPT:-1}"
else
  builder="$server/$repo/local"
  run="local"
fi
base="$(sed -n 's/^defaultBaseImage: *//p' .ko.yaml)"
if [[ "$base" != *@sha256:* ]]; then
  echo "provenance: .ko.yaml must name defaultBaseImage by digest" >&2
  exit 1
fi
jq -n \
  --arg repo "$server/$repo" --arg commit "$commit" --arg ref "$ref" --argjson modified "$modified" \
  --arg workflow "${GITHUB_WORKFLOW_REF:-local}" --arg builder "$builder" --arg run "$run" --arg base "$base" \
  --arg version "${ERP_VERSION:-}" --arg started "${ERP_BUILD_STARTED:-}" '{
  buildDefinition: {
    buildType: "https://github.com/mustie7iy/agentic-erp/ko-images@v1",
    externalParameters: {repository: $repo, ref: $ref, workflow: $workflow, version: $version},
    internalParameters: {koConfig: ".ko.yaml", ko: "github.com/google/ko@v0.19.1", sourceModified: $modified},
    resolvedDependencies: [
      {uri: ("git+" + $repo + "@" + $ref), digest: {gitCommit: $commit}},
      {uri: ("oci://" + ($base | split("@")[0])), digest: {sha256: ($base | split("@sha256:")[1])}}
    ]
  },
  runDetails: {
    builder: {id: $builder},
    metadata: ({invocationId: $run} + (if $started == "" then {} else {startedOn: $started} end))
  }
}'
