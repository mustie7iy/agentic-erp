#!/usr/bin/env bash
# The shards GitHub runs the race suite and the coverage gate in (06 §6.5,
# PO 2026-11-10): the suite takes too long for one runner, so ci.yml runs
# it as parallel jobs, each one shard. The shards are computed, never
# listed: the packages come from `go list ./...`, so a new package lands in
# a shard without anyone naming it, and `check` fails when a package or a
# test of a split package would land in none or in two.
#
#   ci-shards.sh plan race|coverage      one line per piece: shard, package
#                                        directory, piece, cost; the piece
#                                        is - (the whole package), I/K (part
#                                        I of K), TestName (a test run alone,
#                                        three times) or TestName#R (run R
#                                        of 3 of a test run alone)
#   ci-shards.sh matrix                  race=[1,…] and coverage=[1,…] for
#                                        GITHUB_OUTPUT (the jobs' matrices)
#   ci-shards.sh check                   every package in exactly one shard
#                                        (each part of a split one), every
#                                        test function of a split package in
#                                        exactly one piece, and ci.yml's
#                                        coverage jobs equal to the plan
#   ci-shards.sh tests DIR PART PARTS    the test functions of one part
#                                        (those not run alone)
#   ci-shards.sh verify DIR PARTS [FLAG] the parts hold every test the
#                                        compiled test binary lists (go test
#                                        FLAG... -list); run race does it
#   ci-shards.sh run race|coverage N     runs shard N (coverage: writes
#                                        COVERAGE_DIR/shard-N.profile)
#   ci-shards.sh gate                    the coverage gate on the profile of
#                                        every shard of the plan, each of
#                                        which must be in COVERAGE_DIR
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
# Tests run alone. A line "SECONDS DIR TestName" in the weights gives one
# race run of that test function, measured on its own: the test leaves its
# package's parts and becomes a piece of its own, and the package's other
# tests (its weight less the test's) are planned as above, as parts 1/1
# … K/K run with -run even when K is 1. A test whose three runs cost more
# than SPLIT is run as three pieces, TestName#1 to #3, each `-count=1
# -shuffle=on`, which the plan deals to different shards like any heavy
# piece; otherwise it is one piece of three runs. This is for a test too
# long to share a part: the walkthrough (cmd/erp's TestWalkthrough, which
# runs scripts/walkthrough.sh) took some 13 minutes a run on 2026-11-13
# with 78 steps and grows with every slice, so its three runs in one go
# test passed the 35-minute limit (PR #121, run 43451866743). Run apart,
# each run has the limit to itself: a run would have to grow to some 35
# minutes, about three times the walkthrough's size then, before the
# walkthrough itself needs splitting. Three runs in three processes still
# run every test three times shuffled; what they give up is a second run
# in the same process, which the walkthrough, a fresh database and a fresh
# script each run, does not use. A piece run alone costs half as much
# again as its seconds, as a part does.
#
# The coverage plan deals whole packages to COVERAGE_SHARDS shards the same
# way. ci.yml's coverage job restores one profile per shard, each in a step
# of its own, which `check` holds equal to the plan; and `gate` names the
# profiles itself, one per shard of the plan, so that no list in the
# workflow can fall short of it: a shard whose profile is not there fails
# the gate.
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

# plan MODE prints "shard dir piece cost", sorted by shard.
plan() {
  local mode="$1" pkgs
  case "$mode" in race | coverage) ;; *) echo "ci-shards: plan race|coverage" >&2; return 2 ;; esac
  pkgs="$(packages)" || return 1
  awk -v mode="$mode" -v def="$DEFAULT" -v lim="$SPLIT" '
    function count(cost) { return cost > lim ? int(cost / lim) + (int(cost / lim) * lim < cost) : 1 }
    FNR == NR {
      if ($0 ~ /^#/) next
      if (NF == 2) w[$2] = $1
      if (NF == 3) { alone[$2] = alone[$2] " " $3; t[$2 " " $3] = $1; tsum[$2] += $1 }
      next
    }
    {
      s = ($1 in w) ? w[$1] : def
      if (mode == "coverage") { printf "%.0f %s -\n", s, $1; next }
      if (!($1 in alone)) {
        k = count(3 * s)
        if (k == 1) printf "%.0f %s -\n", 3 * s, $1
        else for (i = 1; i <= k; i++) printf "%.0f %s %d/%d\n", 3 * s / k * 1.5, $1, i, k
        next
      }
      s -= tsum[$1]
      if (s < def) s = def
      k = count(3 * s)
      for (i = 1; i <= k; i++) printf "%.0f %s %d/%d\n", 3 * s / k * 1.5, $1, i, k
      n = split(substr(alone[$1], 2), names, " ")
      for (j = 1; j <= n; j++) {
        c = 3 * t[$1 " " names[j]]
        if (c > lim) for (i = 1; i <= 3; i++) printf "%.0f %s %s#%d\n", c / 3 * 1.5, $1, names[j], i
        else printf "%.0f %s %s\n", c * 1.5, $1, names[j]
      }
    }' "$weights" - <<<"$pkgs" | sort -k1,1nr -k2,2 -k3,3 |
    awk -v mode="$mode" -v cap="$CAP" -v fixed="$COVERAGE_SHARDS" '
      { cost[NR] = $1; line[NR] = $2 " " $3 " " $1; total += $1 }
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
      }' | sort -k1,1n -k2,2 -k3,3
}

# functions DIR prints a package's Test, Fuzz and Example functions (what
# -run selects), sorted.
functions() {
  # grep exits 1 on a package without test functions: an empty part, which
  # check refuses.
  { grep -hoE '^func (Test|Fuzz|Example)[A-Za-z0-9_]*\(' "$root/$1"/*_test.go 2>/dev/null || true; } |
    sed -e 's/^func //' -e 's/($//' | { grep -vx TestMain || true; } | sort -u
}

# alone DIR prints the test functions of a package the weights run alone.
alone() {
  awk -v dir="$1" '$0 !~ /^#/ && NF == 3 && $2 == dir {print $3}' "$weights" | sort -u
}

# tests DIR PART PARTS prints the test functions of one part of a package:
# its functions not run alone, dealt round robin.
tests() {
  local dir="$1" part="$2" parts="$3"
  functions "$dir" | { grep -vxF -f <(alone "$dir"; echo TestMain) || true; } |
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
# per piece, and every test function of a package in pieces in exactly one
# of them, a test the weights run alone included.
check_plan() {
  local mode="$1" planned pkgs rc=0 dir parts name
  planned="$(plan "$mode")" || return 1
  pkgs="$(packages)" || return 1
  if ! diff <(awk '{print $2}' <<<"$planned" | sort -u) <(echo "$pkgs") >/dev/null; then
    echo "ci-shards: the $mode plan does not hold exactly the packages go list names:" >&2
    diff <(awk '{print $2}' <<<"$planned" | sort -u) <(echo "$pkgs") >&2 || true
    rc=1
  fi
  # Each (package, piece) once; a package whole or in pieces, not both;
  # its parts numbered 1..K; a test run alone in one piece or in runs 1..3.
  if ! awk '{ seen[$2 " " $3]++ }
      $3 == "-" { whole[$2] = 1; next }
      $3 ~ /^[0-9]+\/[0-9]+$/ {
        split($3, f, "/"); n[$2]++
        if (($2 in k) && k[$2] != f[2] || f[1] < 1 || f[1] > f[2]) { print "part out of order: " $2 " " $3; bad = 1 }
        k[$2] = f[2]; next
      }
      { name = $3; sub(/#.*/, "", name); pieced[$2] = 1
        if ($3 == name) once[$2 " " name]++
        else if ($3 ~ /#[123]$/) runs[$2 " " name]++
        else { print "run out of order: " $2 " " $3; bad = 1 } }
      END { for (x in seen) if (seen[x] != 1) { print "twice: " x; bad = 1 }
            for (p in whole) if ((p in n) || (p in pieced)) { print "whole and in pieces: " p; bad = 1 }
            for (p in n) if (n[p] != k[p]) { print "parts missing: " p; bad = 1 }
            for (p in pieced) if (!(p in n)) { print "no part for the other tests of: " p; bad = 1 }
            for (x in once) if (x in runs) { print "run alone both ways: " x; bad = 1 }
            for (x in runs) if (runs[x] != 3) { print "runs missing: " x; bad = 1 }
            exit bad }' <<<"$planned" >&2; then
    echo "ci-shards: the $mode plan repeats or misses a piece" >&2
    rc=1
  fi
  while read -r dir parts; do
    local all="" dealt="" i
    all="$(functions "$dir")"
    while read -r name; do
      if ! grep -qxF "$name" <<<"$all"; then
        echo "ci-shards: $dir has no test $name, which the weights run alone" >&2
        rc=1
      fi
      dealt+="$name"$'\n'
    done < <(alone "$dir")
    for i in $(seq 1 "$parts"); do
      pattern "$dir" "$i" "$parts" >/dev/null || rc=1
      dealt+="$(tests "$dir" "$i" "$parts")"$'\n'
    done
    if [ "$(grep -v '^$' <<<"$dealt" | sort)" != "$all" ]; then
      echo "ci-shards: the pieces of $dir do not hold each of its test functions once" >&2
      rc=1
    fi
  done < <(awk '$3 ~ /^1\// {split($3, f, "/"); print $2, f[2]}' <<<"$planned")
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
  dealt="$(alone "$dir")"$'\n'
  for i in $(seq 1 "$parts"); do dealt+="$(tests "$dir" "$i" "$parts")"$'\n'; done
  missing="$(comm -23 <(echo "$listed") <(grep -v '^$' <<<"$dealt" | sort -u))"
  if [ -n "$missing" ]; then
    echo "ci-shards: tests of $dir that no part runs:" >&2
    echo "$missing" >&2
    return 1
  fi
}

# run_race N runs shard N's pieces: the whole packages together, two at a
# time, then each other piece on its own. Every piece runs even after one
# fails, so one job shows every failure of its shard.
run_race() {
  local n="$1" planned mine whole=() rc=0 dir piece expr count
  planned="$(plan race)" || return 1
  mine="$(awk -v n="$n" '$1 == n' <<<"$planned")"
  [ -n "$mine" ] || { echo "ci-shards: the race plan has no shard $n" >&2; return 1; }
  echo "shard $n of $(awk 'END {print $1}' <<<"$planned"):"
  awk '{ p = $3; if (p ~ /\//) p = " part " p; else if (p ~ /#/) { sub(/#/, " run ", p); p = " " p " of 3" } else if (p != "-") p = " " p; else p = ""
         printf "  %s%s (%s)\n", $2, p, $4 }' <<<"$mine"
  cd "$root"
  while read -r dir; do whole+=("./$dir"); done < <(awk '$3 == "-" {print $2}' <<<"$mine")
  # -timeout 35m on each go test below: a shard is planned at about 20
  # minutes of tests in all, and no piece near 35 (a test whose three runs
  # would be is run as three pieces), so the limit only guards against a
  # hang, which go test then names before the job's 60 minutes stop the
  # runner.
  if [ "${#whole[@]}" -gt 0 ]; then
    go test -race -shuffle=on -count=3 -timeout 35m -p 2 "${whole[@]}" || rc=1
  fi
  while read -r dir piece; do
    count=3
    case "$piece" in
    */*)
      if [ "${piece%/*}" = 1 ]; then verify_tests "$dir" "${piece#*/}" -race || rc=1; fi
      expr="$(pattern "$dir" "${piece%/*}" "${piece#*/}")" || { rc=1; continue; }
      ;;
    *'#'*) expr="^${piece%#*}\$" count=1 ;;
    *) expr="^$piece\$" ;;
    esac
    go test -race -shuffle=on -count="$count" -timeout 35m -run "$expr" "./$dir" || rc=1
  done < <(awk '$3 != "-" {print $2, $3}' <<<"$mine")
  return "$rc"
}

# profile N is where shard N's coverage profile is kept.
profile() {
  echo "${COVERAGE_DIR:?COVERAGE_DIR: the directory of the coverage profiles}/shard-$1.profile"
}

# coverage_gate applies the gate to the profiles of all the plan's shards
# (scripts/coverage.sh gate, which refuses a missing or empty one).
coverage_gate() {
  local profiles=() n out
  for n in $(seq 1 "$COVERAGE_SHARDS"); do
    out="$(profile "$n")" || return 1
    profiles+=("$out")
  done
  "$here/coverage.sh" gate "${profiles[@]}"
}

# run_coverage N runs shard N's packages instrumented and leaves their
# reduced profile in COVERAGE_DIR (scripts/coverage.sh run).
run_coverage() {
  local n="$1" planned pkgs=() dir out
  out="$(profile "$n")" || return 1
  planned="$(plan coverage)" || return 1
  while read -r dir; do pkgs+=("./$dir"); done < <(awk -v n="$n" '$1 == n {print $2}' <<<"$planned")
  [ "${#pkgs[@]}" -gt 0 ] || { echo "ci-shards: the coverage plan has no shard $n" >&2; return 1; }
  echo "coverage shard $n of $COVERAGE_SHARDS: ${pkgs[*]}"
  cd "$root"
  mkdir -p "$(dirname "$out")"
  "$here/coverage.sh" run "$out" "${pkgs[@]}"
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
gate) coverage_gate ;;
run)
  case "${2:-}" in
  race) run_race "${3:?shard number}" ;;
  coverage) run_coverage "${3:?shard number}" ;;
  *) echo "ci-shards: run race|coverage N" >&2; exit 2 ;;
  esac
  ;;
*)
  echo "usage: ci-shards.sh plan race|coverage | matrix | check | tests DIR PART PARTS | verify DIR PARTS [FLAG...] | run race|coverage N | gate" >&2
  exit 2
  ;;
esac
