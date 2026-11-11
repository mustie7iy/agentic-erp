#!/usr/bin/env bash
# The shards GitHub runs the race suite and the coverage gate in (06 §6.5,
# PO 2026-11-10): the suite takes too long for one runner, so ci.yml runs
# it as parallel jobs, each one shard. The shards are computed, never
# listed: the packages come from `go list ./...`, so a new package lands in
# a shard without anyone naming it, and `check` fails when a package or a
# test of a split package would land in none or in two.
#
#   ci-shards.sh plan race|coverage      one line per piece: shard, package
#                                        directory, part, parts, cost
#   ci-shards.sh matrix                  race=[1,…] and coverage=[1,…] for
#                                        GITHUB_OUTPUT (the jobs' matrices)
#   ci-shards.sh check                   every package in exactly one shard
#                                        (each part of a split one), every
#                                        test function of a split package in
#                                        exactly one part, and ci.yml's
#                                        coverage jobs equal to the plan
#   ci-shards.sh tests DIR PART PARTS    the test functions of one part
#   ci-shards.sh verify DIR PARTS [FLAG] the parts hold every test the
#                                        compiled test binary lists (go test
#                                        FLAG... -list); run race does it
#   ci-shards.sh run race|coverage N     runs shard N (coverage: writes the
#                                        profile COVERAGE_OUT names)
#
# Weights. scripts/ci-shard-weights.txt holds the seconds one race run of a
# package took on a GitHub runner, two packages at a time (`-p 2`), for the
# packages that take 20 s or more; any other package counts as DEFAULT. A
# weight only balances the shards: a package without one is still tested.
#
# The race plan. A package's cost is three times its weight, for the three
# shuffled runs. One whose three runs cost more than SPLIT seconds is split
# into parts by test function (sorted by name, dealt round robin), each run
# with -run on its own, so a part costs half as much again as its share: it
# has the job to itself instead of sharing it two at a time. The pieces are
# dealt, heaviest first, to the lightest of ceil(total / CAP) shards. With
# CAP 2400 a shard is about 20 minutes of tests behind 5 of set-up and race
# build, which keeps a job near the 25 minutes the plan aims at.
#
# The coverage plan deals whole packages to COVERAGE_SHARDS shards the same
# way; ci.yml's coverage job restores exactly that many profiles, which
# `check` holds.
#
# CI_SHARDS_PACKAGES names a file of package directories to plan instead of
# `go list`'s, and CI_SHARDS_ROOT the tree they are in: the script's tests
# (scripts/test-ci-shards.sh) set both.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="${CI_SHARDS_ROOT:-$(dirname "$here")}"
weights="${CI_SHARDS_WEIGHTS:-$here/ci-shard-weights.txt}"
workflow="${CI_SHARDS_WORKFLOW:-$root/.github/workflows/ci.yml}"
DEFAULT="${CI_SHARDS_DEFAULT:-10}"
SPLIT="${CI_SHARDS_SPLIT:-1200}"
CAP="${CI_SHARDS_CAP:-2400}"
COVERAGE_SHARDS="${CI_SHARDS_COVERAGE:-4}"
export LC_ALL=C # one sort order on every machine: the plan must not differ between jobs

# packages prints the directory of every package, relative to the root.
packages() {
  if [ -n "${CI_SHARDS_PACKAGES:-}" ]; then
    grep -v '^$' "$CI_SHARDS_PACKAGES" | sort -u
    return
  fi
  local out
  # -find: the packages' directories, without resolving what they import.
  out="$(cd "$root" && go list -find -f '{{.Dir}}' ./...)" || return 1
  [ -n "$out" ] || { echo "ci-shards: go list found no package" >&2; return 1; }
  sed -e "s#^$(cd "$root" && pwd -P)/##" -e "s#^$(cd "$root" && pwd -P)\$#.#" <<<"$out" | sort -u
}

# plan MODE prints "shard dir part parts cost", sorted by shard.
plan() {
  local mode="$1" pkgs
  case "$mode" in race | coverage) ;; *) echo "ci-shards: plan race|coverage" >&2; return 2 ;; esac
  pkgs="$(packages)" || return 1
  awk -v mode="$mode" -v def="$DEFAULT" -v lim="$SPLIT" '
    FNR == NR { if ($0 !~ /^#/ && NF >= 2) w[$2] = $1; next }
    {
      cost = (($1 in w) ? w[$1] : def) * (mode == "race" ? 3 : 1)
      parts = 1
      if (mode == "race" && cost > lim) { parts = int(cost / lim); if (parts * lim < cost) parts++ }
      for (i = 1; i <= parts; i++) printf "%.0f %s %d %d\n", (parts > 1 ? cost / parts * 1.5 : cost), $1, i, parts
    }' "$weights" - <<<"$pkgs" | sort -k1,1nr -k2,2 -k3,3n |
    awk -v mode="$mode" -v cap="$CAP" -v fixed="$COVERAGE_SHARDS" '
      { cost[NR] = $1; line[NR] = $2 " " $3 " " $4 " " $1; total += $1 }
      END {
        k = fixed
        if (mode == "race") { k = int(total / cap); if (k * cap < total) k++ }
        if (k < 1) k = 1
        for (n = 1; n <= NR; n++) {
          best = 1
          for (s = 2; s <= k; s++) if (load[s] + 0 < load[best] + 0) best = s
          load[best] += cost[n]
          print best, line[n]
        }
      }' | sort -k1,1n -k2,2 -k3,3n
}

# tests DIR PART PARTS prints the test functions of one part of a package:
# its Test, Fuzz and Example functions (what -run selects), sorted, dealt
# round robin.
tests() {
  local dir="$1" part="$2" parts="$3"
  # grep exits 1 on a package without test functions: an empty part, which
  # check refuses.
  { grep -hoE '^func (Test|Fuzz|Example)[A-Za-z0-9_]*\(' "$root/$dir"/*_test.go 2>/dev/null || true; } |
    sed -e 's/^func //' -e 's/($//' | grep -vx TestMain | sort -u |
    awk -v part="$part" -v parts="$parts" '(NR - 1) % parts == part - 1'
}

# pattern DIR PART PARTS prints the -run expression of one part.
pattern() {
  local names
  names="$(tests "$@" | paste -sd'|' -)"
  [ -n "$names" ] || { echo "ci-shards: part $2 of $3 of $1 has no test" >&2; return 1; }
  echo "^($names)\$"
}

# check_plan MODE fails unless every package is in exactly one shard, once
# per part, and every test function of a split package in exactly one part.
check_plan() {
  local mode="$1" planned pkgs rc=0 dir parts
  planned="$(plan "$mode")" || return 1
  pkgs="$(packages)" || return 1
  if ! diff <(awk '{print $2}' <<<"$planned" | sort -u) <(echo "$pkgs") >/dev/null; then
    echo "ci-shards: the $mode plan does not hold exactly the packages go list names:" >&2
    diff <(awk '{print $2}' <<<"$planned" | sort -u) <(echo "$pkgs") >&2 || true
    rc=1
  fi
  # Each (package, part) once, and a package's parts numbered 1..parts.
  if ! awk '{ seen[$2 " " $3]++; n[$2]++; parts[$2] = $4 }
      END { for (k in seen) if (seen[k] != 1) { print "twice: " k; bad = 1 }
            for (p in n) if (n[p] != parts[p]) { print "parts missing: " p; bad = 1 }
            exit bad }' <<<"$planned" >&2; then
    echo "ci-shards: the $mode plan repeats or misses a piece" >&2
    rc=1
  fi
  while read -r dir parts; do
    local all="" dealt="" i
    all="$(tests "$dir" 1 1)"
    for i in $(seq 1 "$parts"); do
      pattern "$dir" "$i" "$parts" >/dev/null || rc=1
      dealt+="$(tests "$dir" "$i" "$parts")"$'\n'
    done
    if [ "$(grep -v '^$' <<<"$dealt" | sort)" != "$all" ]; then
      echo "ci-shards: the parts of $dir do not hold each of its test functions once" >&2
      rc=1
    fi
  done < <(awk '$4 > 1 && $3 == 1 {print $2, $4}' <<<"$planned")
  return "$rc"
}

# check_workflow fails unless ci.yml's coverage gate restores the profile
# of each shard of the plan, once: the shard jobs' matrix comes from
# `matrix`, but the gate restores each profile in a step of its own, which
# a workflow cannot loop over.
check_workflow() {
  local want got
  want="$(seq 1 "$COVERAGE_SHARDS" | paste -sd' ' -)"
  got="$(sed -n 's/^ *restore-keys: coverage-.*-shard\([0-9][0-9]*\)-$/\1/p' "$workflow" | sort -n | paste -sd' ' -)"
  if [ "$got" != "$want" ]; then
    echo "ci-shards: $workflow restores the coverage profiles of shards [$got], the plan has [$want]" >&2
    return 1
  fi
}

# verify_tests DIR PARTS fails unless every test go test would run in the
# package is in a part: the parts are found by reading the test files, and
# this asks the compiled test binary. Flags for go test come after.
verify_tests() {
  local dir="$1" parts="$2" listed dealt="" i missing
  shift 2
  listed="$(cd "$root" && go test "$@" -list '.*' "./$dir" | grep -E '^(Test|Fuzz|Example)' | sort -u)" || return 1
  for i in $(seq 1 "$parts"); do dealt+="$(tests "$dir" "$i" "$parts")"$'\n'; done
  missing="$(comm -23 <(echo "$listed") <(grep -v '^$' <<<"$dealt" | sort -u))"
  if [ -n "$missing" ]; then
    echo "ci-shards: tests of $dir that no part runs:" >&2
    echo "$missing" >&2
    return 1
  fi
}

# run_race N runs shard N's pieces: the whole packages together, two at a
# time, then each part of a split package on its own. Every piece runs even
# after one fails, so one job shows every failure of its shard.
run_race() {
  local n="$1" planned mine whole=() rc=0 dir part parts expr
  planned="$(plan race)" || return 1
  mine="$(awk -v n="$n" '$1 == n' <<<"$planned")"
  [ -n "$mine" ] || { echo "ci-shards: the race plan has no shard $n" >&2; return 1; }
  echo "shard $n of $(awk 'END {print $1}' <<<"$planned"):"
  awk '{printf "  %s%s (%s)\n", $2, ($4 > 1 ? " part " $3 "/" $4 : ""), $5}' <<<"$mine"
  cd "$root"
  while read -r dir; do whole+=("./$dir"); done < <(awk '$4 == 1 {print $2}' <<<"$mine")
  # -timeout 35m, under the job's limit, so go test names a hung package
  # before the runner stops the job.
  if [ "${#whole[@]}" -gt 0 ]; then
    go test -race -shuffle=on -count=3 -timeout 35m -p 2 "${whole[@]}" || rc=1
  fi
  while read -r dir part parts; do
    if [ "$part" = 1 ]; then verify_tests "$dir" "$parts" -race || rc=1; fi
    expr="$(pattern "$dir" "$part" "$parts")" || { rc=1; continue; }
    go test -race -shuffle=on -count=3 -timeout 35m -run "$expr" "./$dir" || rc=1
  done < <(awk '$4 > 1 {print $2, $3, $4}' <<<"$mine")
  return "$rc"
}

# run_coverage N runs shard N's packages instrumented and leaves their
# reduced profile in COVERAGE_OUT (scripts/coverage.sh run).
run_coverage() {
  local n="$1" planned pkgs=() dir
  planned="$(plan coverage)" || return 1
  while read -r dir; do pkgs+=("./$dir"); done < <(awk -v n="$n" '$1 == n {print $2}' <<<"$planned")
  [ "${#pkgs[@]}" -gt 0 ] || { echo "ci-shards: the coverage plan has no shard $n" >&2; return 1; }
  echo "coverage shard $n of $COVERAGE_SHARDS: ${pkgs[*]}"
  cd "$root"
  ./scripts/coverage.sh run "${COVERAGE_OUT:?COVERAGE_OUT: where to write the shard profile}" "${pkgs[@]}"
}

case "${1:-}" in
plan) plan "${2:-}" ;;
matrix)
  for mode in race coverage; do
    planned="$(plan "$mode")"
    echo "$mode=[$(awk '{print $1}' <<<"$planned" | sort -un | paste -sd, -)]"
  done
  ;;
check)
  rc=0
  check_plan race || rc=1
  check_plan coverage || rc=1
  check_workflow || rc=1
  [ "$rc" = 0 ] && echo "ci-shards: $(packages | wc -l | tr -d ' ') packages, each in one race shard of $(plan race | awk 'END {print $1}') and one coverage shard of $COVERAGE_SHARDS"
  exit "$rc"
  ;;
tests) tests "${2:?DIR}" "${3:?PART}" "${4:?PARTS}" ;;
verify)
  shift
  verify_tests "$@"
  ;;
run)
  case "${2:-}" in
  race) run_race "${3:?shard number}" ;;
  coverage) run_coverage "${3:?shard number}" ;;
  *) echo "ci-shards: run race|coverage N" >&2; exit 2 ;;
  esac
  ;;
*)
  echo "usage: ci-shards.sh plan race|coverage | matrix | check | tests DIR PART PARTS | verify DIR PARTS [FLAG...] | run race|coverage N" >&2
  exit 2
  ;;
esac
