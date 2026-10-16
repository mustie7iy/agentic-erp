#!/usr/bin/env bash
# Checks the infrastructure code without credentials (ADR-0035), as CI's
# deploy workflow runs it:
#   - terraform fmt over deploy/terraform;
#   - every module and environment root initialised without a backend and
#     validated, and each module's `terraform test` run against a mock AWS
#     provider (no account is touched);
#   - tflint on every module and root;
#   - the encryption-at-rest policy (deploy/policy) unit-tested with
#     `conftest verify`, then applied to every root and module, which must
#     pass, and to the fixtures under deploy/policy/testdata, which must be refused
#     on each of their resources (encryption at rest, VPC endpoints).
# TERRAFORM, TFLINT and CONFTEST name the binaries.
set -euo pipefail
cd "$(dirname "$0")/.."
TERRAFORM="${TERRAFORM:-terraform}"
TFLINT="${TFLINT:-tflint}"
CONFTEST="${CONFTEST:-conftest}"
export TF_IN_AUTOMATION=1 TF_INPUT=0

echo "== terraform fmt"
$TERRAFORM fmt -check -recursive deploy/terraform

for dir in deploy/terraform/modules/* deploy/terraform/envs/*; do
  echo "== $dir"
  $TERRAFORM -chdir="$dir" init -backend=false -input=false >/dev/null
  # A module that takes an aliased provider (configuration_aliases) cannot
  # be validated alone: nothing configures the alias. Its tests pass it a
  # mock provider and the roots that call it are validated, so both check it.
  if grep -q configuration_aliases "$dir"/*.tf; then
    if [ ! -d "$dir/tests" ]; then
      echo "deploy-checks: $dir takes an aliased provider but has no tests to validate it" >&2
      exit 1
    fi
  else
    $TERRAFORM -chdir="$dir" validate -no-color
  fi
  if [ -d "$dir/tests" ]; then
    $TERRAFORM -chdir="$dir" test -no-color
  fi
  $TFLINT --chdir="$dir" --config="$PWD/deploy/terraform/.tflint.hcl" --minimum-failure-severity=notice --no-color
done

echo "== policy unit tests"
$CONFTEST verify --policy deploy/policy --no-color

echo "== policy on the infrastructure"
for dir in deploy/terraform/modules/* deploy/terraform/envs/*; do
  $CONFTEST test --parser hcl2 --combine --policy deploy/policy --no-color "$dir"/*.tf
done

echo "== policy on resources that must be refused"
# Each fixture and the number of refusals it must draw: one per resource
# left unencrypted, one per endpoint a VPC lacks.
for fixture in unencrypted:17 no-endpoints:3; do
  dir="deploy/policy/testdata/${fixture%%:*}"
  want="${fixture##*:}"
  if out="$($CONFTEST test --parser hcl2 --combine --policy deploy/policy --no-color "$dir"/*.tf 2>&1)"; then
    echo "$out"
    echo "deploy-checks: the policy accepted $dir" >&2
    exit 1
  fi
  got="$(grep -c '^FAIL' <<<"$out" || true)"
  if [ "$got" -ne "$want" ]; then
    echo "$out"
    echo "deploy-checks: the policy refused $got times in $dir, want $want" >&2
    exit 1
  fi
  echo "deploy-checks: the policy refused all $want in $dir"
done
