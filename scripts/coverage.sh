#!/usr/bin/env bash
# Whole-suite statement coverage of production code, with a minimum,
# compared on exact statement counts.
# Excluded from the figure: test-only packages (nfr, llmtest, app's apitest, einvoicing's einvoicetest, and the
# platform's pgtest, authntest, natstest, tlstest, blobstoretest, sftptest, webhooktest, vaulttest, kmstest, workflowtest, fixture;
# internal/layout's testSupport lists the same packages) and code generated from the
# API contract (api/gen). Database-backed packages need DATABASE_URL; without
# it their tests skip and the gate fails, so run `make db-up` first.
#
#   coverage.sh                    the whole suite, then the gate
#   coverage.sh run OUT PKG...     the tests of PKG... alone, instrumented
#                                  as the whole suite is; OUT gets their
#                                  profile, reduced to one line per block of
#                                  production code (a shard of ci.yml's
#                                  coverage, scripts/ci-shards.sh)
#   coverage.sh gate PROFILE...    the gate on the profiles together: a block
#                                  counts as covered if any of them hit it
set -euo pipefail
MIN="${COVERAGE_MIN:-100.0}"
excluded='/internal/(nfr|llm/llmtest|app/apitest|einvoicing/einvoicetest)/|/internal/platform/(pgtest|authn/authntest|natsbus/natstest|tlsconf/tlstest|blobstore/blobstoretest|sftpclient/sftptest|webhook/webhooktest|secrets/vaulttest|fieldcrypt/kmstest|workflow/workflowtest|fixture)/|/api/gen/'

# run OUT PKG... tests the packages and writes their reduced profile.
run() {
  local out="$1" coverpkg profile
  shift
  if [ -z "${DATABASE_URL:-}" ]; then
    echo "coverage: DATABASE_URL is not set (make db-up, then make coverage)" >&2
    return 1
  fi
  # Instrument only the packages the figure counts. With -coverpkg=./... every
  # one of the ~70 test binaries carried an instrumented copy of api/gen
  # (~620,000 generated lines, over four times the rest of the code), which filled a
  # CI runner's disk; instrumenting the measured packages keeps the build
  # proportional to the code under test. Always every measured package,
  # whichever packages' tests run: a shard's tests cover other shards' code.
  coverpkg="$(go list ./... | sed 's#$#/#' | grep -v -E "$excluded" | sed 's#/$##' | paste -sd, -)"
  profile="$(mktemp)"
  # -timeout 60m, as the single race run in ci.yml's test job: the limit only
  # guards against a hang, and under load internal/analytics/extract and
  # internal/web, instrumented, took longer than go test's default of 10
  # minutes per package without hanging (T72b).
  if ! go test -short -count=1 -timeout 60m -coverpkg="$coverpkg" -coverprofile="$profile" "$@" > "$profile.log" 2>&1; then
    echo "coverage: tests failed:" >&2
    grep -E "^(--- FAIL|FAIL|panic:)" "$profile.log" >&2
    # Everything the failing packages printed: build errors ("# pkg" blocks),
    # TestMain failures and test output alike, without the passing packages.
    echo "--- output of the failing packages:" >&2
    grep -v -E "^(ok |\?|---  *PASS|=== RUN|coverage: )" "$profile.log" | head -400 >&2
    return 1
  fi
  # Every test binary reports each block, so a block counts as covered if any
  # run hit it: one line per block, its count 1 or 0.
  { echo "mode: set"
    grep -v -E "$excluded" "$profile" | awk '!/^mode:/ { split($0, f, " "); k = f[1]; n[k] = f[2]; if (f[3] > 0) hit[k] = 1 }
      END { for (k in n) print k, n[k], ((k in hit) ? 1 : 0) }' | sort
  } > "$out"
  rm -f "$profile" "$profile.log"
}

# gate PROFILE... applies the minimum to the profiles together.
gate() {
  local covered statements blocks
  # The exclusions again: a profile handed over by a shard is not trusted
  # to have applied them.
  blocks="$(cat "$@" | grep -v -E "$excluded" | grep -v '^mode:' || true)"
  # Count statements, not the rounded percentage go tool cover prints: at this
  # size a handful of uncovered statements still rounds to 100.0%.
  read -r covered statements < <(awk 'NF { split($0, f, " "); k = f[1]; n[k] = f[2]; if (f[3] > 0) hit[k] = 1 }
    END { for (k in n) { s += n[k]; if (k in hit) c += n[k] }; print c + 0, s + 0 }' <<<"$blocks")
  echo "statement coverage: ${covered} of ${statements} statements (minimum ${MIN}%)"
  if [ "$statements" -eq 0 ]; then
    echo "coverage: the profile has no production statements; check the excluded paths" >&2
    return 1
  fi
  if awk -v c="$covered" -v s="$statements" -v m="$MIN" 'BEGIN { exit !(c * 100 < m * s) }'; then
    echo "uncovered blocks:"
    awk 'NF { split($0, f, " "); k = f[1]; n[k] = f[2]; if (f[3] > 0) hit[k] = 1 }
      END { for (k in n) if (!(k in hit) && n[k] > 0) print "  " k " (" n[k] " statements)" }' <<<"$blocks" | sort
    return 1
  fi
}

case "${1:-}" in
run)
  shift
  [ "$#" -ge 2 ] || { echo "usage: coverage.sh run OUT PKG..." >&2; exit 2; }
  run "$@"
  ;;
gate)
  shift
  [ "$#" -ge 1 ] || { echo "usage: coverage.sh gate PROFILE..." >&2; exit 2; }
  for p in "$@"; do
    # A missing or empty shard profile would only shrink the figure's base.
    [ -s "$p" ] || { echo "coverage: no profile at $p" >&2; exit 1; }
  done
  gate "$@"
  ;;
"")
  all="$(mktemp)"
  run "$all" ./...
  gate "$all"
  rm -f "$all"
  ;;
*)
  echo "usage: coverage.sh [run OUT PKG... | gate PROFILE...]" >&2
  exit 2
  ;;
esac
