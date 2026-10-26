#!/usr/bin/env bash
# The full gates on this machine, before a pull request's light GitHub check
# (06 §6.5): every check GitHub runs on main (on a push and nightly),
# plus the shuffled race runs of 07 §7.7, at the versions CI pins
# (scripts/ci-tools.sh, scripts/ci-local-tools.sh). The path-conditional
# ones (the images, the signing round trip, the release scripts, the
# deploy workflow, the promotion self-test) run on the classes
# scripts/ci-changes.sh gives GitHub, so a change skips here exactly what
# main's push run skips (the image reproducibility gate, which only the
# nightly and manual runs hold, on the same classes); a skipped step is
# listed as skip with its reason.
# Every step runs even after one fails, so one run shows every failure;
# each step's output is kept in its own log.
#
# What only a Linux runner does differently is recorded in 06 §6.5: the
# images are built for linux/amd64 alone, as pull requests always built
# them (the weekly run builds both platforms), and the registry, PostgreSQL
# and Temporal are local rather than service containers.
#
# It writes a report named after the tree it tested to
# $(git rev-parse --git-common-dir)/ci-local/<tree>.md, shared by every
# worktree. scripts/merge-pr.sh merges only a pull request whose head tree
# has a passing report, and posts it there. The tree must be committed and
# should be rebased on the latest main: the report vouches for exactly this
# tree, and the breaking-change check compares with main.
#
# Needs PostgreSQL (make db-up, max_connections 300), the Temporal CLI and,
# for the image steps, Docker with buildx.
set -euo pipefail
cd "$(dirname "$0")/.."
export DATABASE_URL="${DATABASE_URL:-postgres://postgres:dev@localhost:55432/postgres?sslmode=disable}"
# The latency budgets at CI's scale (docs/testing.md): this machine runs
# the lanes' suites beside this one, so it is no quieter than a runner.
# The budgets at scale 1 are checked in a cell-sized environment (06 §6.5).
export ERP_BUDGET_SCALE="${ERP_BUDGET_SCALE:-2}"
# The web workspace's browser suite runs, never skips (internal/web/e2e):
# ERP_E2E_BROWSER names the Chromium when it is not in a usual place.
export ERP_E2E_REQUIRE_BROWSER="${ERP_E2E_REQUIRE_BROWSER:-true}"
# Kept equal to GITLEAKS_VERSION in .github/workflows/ci.yml.
GITLEAKS_MODULE=github.com/zricethezav/gitleaks/v8@v8.30.1

if [ -n "$(git status --porcelain)" ]; then
  echo "ci-local: commit or stash your changes first; the report vouches for a committed tree" >&2
  exit 2
fi
command -v temporal >/dev/null || {
  echo "ci-local: the Temporal CLI is not on the path; without it the workflow tests skip" >&2
  exit 2
}
head="$(git rev-parse HEAD)"
tree="$(git rev-parse "HEAD^{tree}")"
tools="$(./scripts/ci-tools.sh)"
./scripts/ci-local-tools.sh "$tools" >/dev/null
if [ "$(cat "$tools/.gitleaks.version" 2>/dev/null || true)" != "$GITLEAKS_MODULE" ]; then
  GOBIN="$tools" go install "$GITLEAKS_MODULE"
  echo "$GITLEAKS_MODULE" >"$tools/.gitleaks.version"
fi
export PATH="$tools:$PATH"
git fetch -q origin main
base="$(git merge-base origin/main HEAD)"
# The classes GitHub's jobs and workflows are skipped by, for this tree as
# a pull request against main.
classes="$(EVENT=pull_request BASE="$base" HEAD="$head" ./scripts/ci-changes.sh)"
class() { sed -n "s/^$1=//p" <<<"$classes"; }

logs="$(CDPATH= cd "$(git rev-parse --git-common-dir)" && pwd)/ci-local/$tree"
rm -rf "$logs" && mkdir -p "$logs"
results=""
failed=0

# step runs one gate, its output in its own log, and records the outcome.
# bash ignores set -e inside an if, so a function it runs must return its
# failures itself rather than rely on stopping at the first.
step() {
  local name="$1" start status
  shift
  start=$(date +%s)
  if "$@" >"$logs/$name.log" 2>&1; then status=ok; else status=FAIL; failed=1; fi
  printf '%-5s %-11s %5ss\n' "$status" "$name" $(($(date +%s) - start)) | tee -a "$logs/summary"
  results+="| $name | $status | $(($(date +%s) - start)) s |"$'\n'
}

# when CLASS NAME CMD... runs step NAME when scripts/ci-changes.sh puts
# this change in CLASS, and lists it as skipped otherwise.
when() {
  local cls="$1" name="$2"
  shift 2
  if [ "$(class "$cls")" = false ]; then
    printf '%-5s %-11s %5s\n' skip "$name" "" | tee -a "$logs/summary"
    results+="| $name | skip | no $cls change |"$'\n'
  else
    step "$name" "$@"
  fi
}

breaking() {
  local spec rc=0
  for spec in openapi control directory; do
    if git cat-file -e "origin/main:api/$spec.yaml" 2>/dev/null; then
      git show "origin/main:api/$spec.yaml" >"$logs/base-$spec.yaml" || return 1
      oasdiff breaking --fail-on ERR --deprecation-days-stable=365 --err-ignore api/breaking-accepted.txt "$logs/base-$spec.yaml" "api/$spec.yaml" || rc=1
    fi
  done
  # Events change only additively within a .vN (ADR-0041).
  if git cat-file -e "origin/main:api/asyncapi.yaml" 2>/dev/null; then
    git show "origin/main:api/asyncapi.yaml" >"$logs/base-asyncapi.yaml" || return 1
    go run ./cmd/eventcompat "$logs/base-asyncapi.yaml" api/asyncapi.yaml || rc=1
  fi
  return "$rc"
}

# builder makes sure of the buildx builder that exports OCI archives
# (erp-analytics), which CI's jobs create as erp-images.
builder() {
  docker buildx inspect erp-images >/dev/null 2>&1 || docker buildx create --name erp-images --driver docker-container >/dev/null
}

# images builds the images as CI's test job does: twice with private
# caches and builders when their inputs changed, once otherwise.
images() {
  if [ "$(class images)" = false ]; then
    builder && KO_PLATFORMS=linux/amd64 BUILDX_BUILDER=erp-images make images
  else
    KO_PLATFORMS=linux/amd64 make images-reproducible
  fi
}

# signing runs the signing round trip against a registry:2 container, the
# image CI's job runs as a service, on a free port of this machine.
signing() {
  local id port rc=0
  builder || return 1
  id="$(docker run -d --rm -p 127.0.0.1::5000 registry:2.8.3)" || return 1
  port="$(docker port "$id" 5000/tcp | head -1 | sed 's/.*://')"
  for _ in $(seq 1 50); do curl -sf "http://localhost:$port/v2/" >/dev/null && break; sleep 0.2; done
  KO_PLATFORMS=linux/amd64 BUILDX_BUILDER=erp-images COSIGN="$tools/cosign" make signing-roundtrip SIGNING_REGISTRY="localhost:$port/erp" || rc=1
  # A container left behind costs memory, not a wrong result; --rm
  # removes it once stopped.
  docker rm -f "$id" >/dev/null || true
  return "$rc"
}

step format bash -c 'test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }'
step vet go vet ./...
# Another lint on this machine (an editor hook, another worktree) holds
# golangci-lint's lock: wait for it rather than fail.
step lint-all make lint-all GOLANGCI=golangci-lint LINT_FLAGS=--allow-serial-runners
step staticcheck staticcheck ./...
step govulncheck govulncheck ./...
step gosec gosec -quiet -exclude=G104,G115,G304,G107 -exclude-dir=api/gen ./...
step secrets gitleaks git --redact --no-banner --log-opts="--no-merges $base..HEAD" .
step generate bash -c 'make generate && git diff --exit-code -- api/gen internal/web && test -z "$(git status --porcelain -- api/gen internal/web)"'
step contract vacuum lint -r api/vacuum-ruleset.yaml -b -q -d api/openapi.yaml api/control.yaml api/directory.yaml api/model.yaml
step breaking breaking
step schema go test -count=1 ./db
# Every tracker item names its source (07 §7.5): a documentation-only change
# skips GitHub's suite, so the check runs here by name as well.
step tracker go test -count=1 ./docs
# Three shuffled runs with the race detector: CI's single race run and the
# order dependence 07 §7.7 asks for, in one pass. Three runs of a package
# take three times go test's default limit of 10 minutes: the schema
# tests, which migrate up and down again and again, took longer than
# that on a busy machine without hanging. By batch 6 (2026-10-22) the
# ledger and payments packages took over 30 minutes for their three runs on
# a loaded machine, so the limit, which only guards against a hang, is 60.
step race go test -race -shuffle=on -count=3 -timeout 60m ./...
step budgets ./scripts/budgets.sh
step coverage ./scripts/coverage.sh
step fuzz make fuzz FUZZTIME=30s
step build make build
step skip-rules ./scripts/test-ci-changes.sh
# The images (ADR-0032): built twice and compared when their inputs
# changed, otherwise once, then scanned; linux/amd64, as pull requests
# built them on GitHub.
when run image-tools ./scripts/test-images.sh
when run images images
when run image-scan make image-scan
when images signing signing
when release releases env GIT_CLIFF="$tools/git-cliff" ./scripts/test-release-scripts.sh
# The deploy workflow's jobs (deploy.yml), with its pinned tools.
when deploy infra ./scripts/deploy-checks.sh
when deploy helm ./scripts/helm-checks.sh
when deploy mirror ./scripts/test-mirror-images.sh
when deploy nats ./scripts/nats-checks.sh
when deploy temporal env GOLANGCI=golangci-lint ./scripts/temporal-checks.sh
when promotion promotion bash -c './scripts/test-promotion-checks.sh && ./scripts/test-promotion-approvals.sh && ./scripts/test-rollback-drill.sh && ./scripts/test-restore-drill.sh'

# A commit, checkout or edit during the run would leave gates having tested
# something else than the tree the report names.
if [ -n "$(git status --porcelain)" ] || [ "$(git rev-parse "HEAD^{tree}")" != "$tree" ]; then
  echo "FAIL  the worktree changed during the run" | tee -a "$logs/summary"
  results+="| unchanged tree | FAIL | |"$'\n'
  failed=1
fi
verdict=PASS
[ "$failed" = 0 ] || verdict=FAIL
report="$logs.md"
{
  echo "ci-local $verdict tree $tree"
  echo
  echo "Commit $head on $(go env GOOS)/$(go env GOARCH), $(go env GOVERSION), compared with main at $(git rev-parse --short origin/main), budgets at scale $ERP_BUDGET_SCALE; path classes (scripts/ci-changes.sh): $(grep -E '^(run|images|release|deploy|promotion)=' <<<"$classes" | tr '\n' ' ' | sed 's/ $//')."
  echo
  echo "| Gate | Result | Time |"
  echo "|---|---|---|"
  printf '%s' "$results"
} >"$report"
echo "ci-local $verdict tree $tree (report $report, logs $logs)"
exit "$failed"
