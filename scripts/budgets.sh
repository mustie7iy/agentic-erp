#!/usr/bin/env bash
# budgets.sh [go test flags]: every performance budget test (a Test...Budget
# or Test...Perf function) in every package that has one, without the race
# detector, one package at a time so their timings do not load each other.
# The list is found, not kept: TestGoodsReceiptBudget broke on 2026-10-16
# and no gate saw it for ten days, because the gates named their packages
# (ledger and nfr) and the budget tests skip under -race (06 §6.5).
set -euo pipefail
cd "$(dirname "$0")/.."
pkgs=()
while IFS= read -r p; do pkgs+=("./$p"); done < <(grep -rlE --include='*_test.go' 'func Test[A-Za-z0-9]*(Budget|Perf)[A-Za-z0-9]*\(' internal cmd |
  xargs -n1 dirname | sort -u)
[ "${#pkgs[@]}" -gt 0 ] || { echo "budgets.sh: no budget tests found" >&2; exit 1; }
echo "budgets in: ${pkgs[*]}"
go test -count=1 -p 1 -run 'Test[A-Za-z0-9]*(Budget|Perf)' "$@" "${pkgs[@]}"
