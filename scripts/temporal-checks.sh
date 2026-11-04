#!/usr/bin/env bash
# Checks the cell's workflow engine, erp-temporal (ADR-0035), a module of
# its own under temporal/ that the repository's make targets do not reach:
# vet, the repository's lint rules (.golangci.yml), the tests with the race
# detector three times in a random order, and 100% statement coverage, as
# the platform's own code is held to (06 §6.3). The schema tests need the
# test PostgreSQL (DATABASE_URL, make db-up).
set -euo pipefail
cd "$(dirname "$0")/../temporal"
GOLANGCI="${GOLANGCI:-go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0}"
: "${DATABASE_URL:?DATABASE_URL: the test PostgreSQL (make db-up)}"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. ../scripts/lib/on-exit.sh
on_exit 'rm -rf "$work"'

go vet ./...
$GOLANGCI run --allow-parallel-runners --config ../.golangci.yml ./...
go test -race -count=3 -shuffle=on -coverprofile="$work/cover.out" ./...
total="$(go tool cover -func="$work/cover.out" | awk '/^total:/ {print $NF}')"
if [ "$total" != "100.0%" ]; then
  go tool cover -func="$work/cover.out" | grep -v '100.0%' >&2
  echo "temporal-checks: statement coverage $total, want 100.0%" >&2
  exit 1
fi
echo "temporal-checks: vet, lint and tests pass; statement coverage $total"
finished
