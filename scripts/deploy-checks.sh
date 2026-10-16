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
#     pass, and to deploy/policy/testdata/unencrypted, which must be refused
#     on each of its resources.
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
  $TERRAFORM -chdir="$dir" validate -no-color
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
want=11
if out="$($CONFTEST test --parser hcl2 --combine --policy deploy/policy --no-color deploy/policy/testdata/unencrypted/*.tf 2>&1)"; then
  echo "$out"
  echo "deploy-checks: the encryption policy accepted deploy/policy/testdata/unencrypted" >&2
  exit 1
fi
got="$(grep -c '^FAIL' <<<"$out" || true)"
if [ "$got" -ne "$want" ]; then
  echo "$out"
  echo "deploy-checks: the encryption policy refused $got resources of the fixture, want $want" >&2
  exit 1
fi
echo "deploy-checks: the encryption policy refused all $want unencrypted resources of the fixture"
