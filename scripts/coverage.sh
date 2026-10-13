#!/usr/bin/env bash
# Whole-suite statement coverage of production code, with a minimum,
# compared on exact statement counts.
# Excluded from the figure: test-only packages (nfr, testkit, llmtest, and the
# platform's pgtest, authntest, natstest, apitest, blobstoretest, vaulttest, kmstest, fixture) and code generated from the
# API contract (api/gen). Database-backed packages need DATABASE_URL; without
# it their tests skip and the gate fails, so run `make db-up` first.
set -euo pipefail
if [ -z "${DATABASE_URL:-}" ]; then
  echo "coverage: DATABASE_URL is not set (make db-up, then make coverage)" >&2
  exit 1
fi
MIN="${COVERAGE_MIN:-100.0}"
excluded='/internal/(nfr|testkit|llm/llmtest)/|/internal/platform/(pgtest|authn/authntest|natsbus/natstest|httpapi/apitest|blobstore/blobstoretest|secrets/vaulttest|fieldcrypt/kmstest|workflow/workflowtest|fixture)/|/api/gen/'
# Instrument only the packages the figure counts. With -coverpkg=./... every
# one of the ~70 test binaries carried an instrumented copy of api/gen
# (650,000 generated lines, 16 times the rest of the code), which filled a
# CI runner's disk; instrumenting the measured packages keeps the build
# proportional to the code under test.
coverpkg="$(go list ./... | sed 's#$#/#' | grep -v -E "$excluded" | sed 's#/$##' | paste -sd, -)"
profile="$(mktemp)"
if ! go test -short -count=1 -coverpkg="$coverpkg" -coverprofile="$profile" ./... > "$profile.log" 2>&1; then
  echo "coverage: tests failed:" >&2
  grep -E "^(--- FAIL|FAIL|panic:)" "$profile.log" >&2
  # Everything the failing packages printed: build errors ("# pkg" blocks),
  # TestMain failures and test output alike, without the passing packages.
  echo "--- output of the failing packages:" >&2
  grep -v -E "^(ok |\?|---  *PASS|=== RUN|coverage: )" "$profile.log" | head -400 >&2
  exit 1
fi
grep -v -E "$excluded" "$profile" > "$profile.prod"
# Count statements, not the rounded percentage go tool cover prints: at this
# size a handful of uncovered statements still rounds to 100.0%. Every test
# binary reports each block, so a block counts as covered if any run hit it.
read -r covered statements < <(awk 'NR > 1 { split($0, f, " "); k = f[1]; n[k] = f[2]; if (f[3] > 0) hit[k] = 1 }
  END { for (k in n) { s += n[k]; if (k in hit) c += n[k] }; print c + 0, s + 0 }' "$profile.prod")
echo "statement coverage: ${covered} of ${statements} statements (minimum ${MIN}%)"
if [ "$statements" -eq 0 ]; then
  echo "coverage: the profile has no production statements; check the excluded paths" >&2
  exit 1
fi
if awk -v c="$covered" -v s="$statements" -v m="$MIN" 'BEGIN { exit !(c * 100 < m * s) }'; then
  echo "uncovered blocks:"
  awk 'NR > 1 { split($0, f, " "); k = f[1]; n[k] = f[2]; if (f[3] > 0) hit[k] = 1 }
    END { for (k in n) if (!(k in hit) && n[k] > 0) print "  " k " (" n[k] " statements)" }' "$profile.prod" | sort
  exit 1
fi
