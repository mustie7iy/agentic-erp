#!/usr/bin/env bash
# budgets.sh [go test flags]: every performance budget test (a Test...Budget
# or Test...Perf function) in every package that has one, without the race
# detector, one package at a time so their timings do not load each other.
# The list is found, not kept: TestGoodsReceiptBudget broke on 2026-10-16
# and no gate saw it for ten days, because the gates named their packages
# (ledger and nfr) and the budget tests skip under -race (06 §6.5).
# Names select them, so the agents' spend budgets (ADR-0036: TestBudgets,
# TestAgentBudget and others), which are functional tests, run here too;
# they take seconds and also run in the race suite.
set -euo pipefail
cd "$(dirname "$0")/.."
# The budget tests measure only here (nfr.Measure): measured in a plain go
# test beside other packages, a wall-clock budget fails on the machine's
# load, not on the code (batch 52).
export ERP_MEASURE_BUDGETS=1
pkgs=()
while IFS= read -r p; do pkgs+=("./$p"); done < <(grep -rlE --include='*_test.go' 'func Test[A-Za-z0-9]*(Budget|Perf)[A-Za-z0-9]*\(' internal cmd |
  xargs -n1 dirname | sort -u)
[ "${#pkgs[@]}" -gt 0 ] || { echo "budgets.sh: no budget tests found" >&2; exit 1; }
echo "budgets in: ${pkgs[*]}"
# Compile the test binaries in parallel first (-exec true runs none), so
# only the measured runs are one package at a time.
go test -exec true "$@" "${pkgs[@]}" >/dev/null
# The limit only guards against a hang: go test's default 10 minutes per
# package stopped main's run after batch 61, when workflows' consolidation
# and depreciation budgets, seeding their scaled data, passed but together
# took over 10 minutes on the runner (run 42685494844).
go test -count=1 -p 1 -timeout 30m -run 'Test[A-Za-z0-9]*(Budget|Perf)' "$@" "${pkgs[@]}"
