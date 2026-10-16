#!/usr/bin/env bash
# Writes the SLSA v1 build provenance of this build (ADR-0032) as JSON: the
# predicate cosign attests to each image (scripts/sign-images.sh). It names
# the commit the images were built from, the workflow run that built them,
# the base image and the ko version, so a verifier can tell which source and
# which build an image came from. In GitHub Actions the GITHUB_* variables
# describe the run; outside it, the local checkout and "local" do.
set -euo pipefail
server="${GITHUB_SERVER_URL:-https://github.com}"
repo="${GITHUB_REPOSITORY:-mustie7iy/agentic-erp}"
commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"
ref="${GITHUB_REF:-$(git symbolic-ref -q HEAD || echo detached)}"
workflow="${GITHUB_WORKFLOW_REF:-local}"
run="${GITHUB_RUN_ID:+$server/$repo/actions/runs/$GITHUB_RUN_ID}"
base="$(sed -n 's/^defaultBaseImage: *//p' .ko.yaml)"
jq -n \
  --arg repo "$server/$repo" --arg commit "$commit" --arg ref "$ref" --arg workflow "$workflow" \
  --arg run "${run:-local}" --arg base "$base" --arg version "${ERP_VERSION:-}" \
  --arg started "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
  buildDefinition: {
    buildType: "https://github.com/mustie7iy/agentic-erp/ko-images@v1",
    externalParameters: {repository: $repo, ref: $ref, workflow: $workflow, version: $version},
    internalParameters: {koConfig: ".ko.yaml", ko: "github.com/google/ko@v0.19.1"},
    resolvedDependencies: [
      {uri: ("git+" + $repo + "@" + $ref), digest: {gitCommit: $commit}},
      {uri: ("oci://" + ($base | split("@")[0])), digest: {sha256: ($base | split("@sha256:")[1])}}
    ]
  },
  runDetails: {
    builder: {id: ($repo + "/" + $workflow)},
    metadata: {invocationId: $run, startedOn: $started}
  }
}'
