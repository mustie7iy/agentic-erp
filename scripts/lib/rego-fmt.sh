# shellcheck shell=bash
# The Rego formatting gate, sourced by scripts/deploy-checks.sh (deploy/policy)
# and scripts/helm-checks.sh (deploy/helm/policy), so every policy in the
# repository is held to `conftest fmt` at the pinned conftest (ADR-0035).
#
# check_rego_fmt DIR: fails unless every .rego file under DIR is as
# `conftest fmt` writes it. It first proves the check bites: a copy of DIR
# with one rule written `x:=1` (fmt always writes `x := 1`, so the copy
# differs from formatted whatever DIR holds) must be refused. CONFTEST
# names the binary.
check_rego_fmt() {
  local dir="$1" probe
  probe="$(mktemp -d)"
  cp "$dir"/*.rego "$probe"/
  printf '\nrego_fmt_probe:=1\n' >>"$(find "$probe" -name '*.rego' | head -n 1)"
  if "${CONFTEST:-conftest}" fmt --check "$probe" >/dev/null 2>&1; then
    rm -rf "$probe"
    echo "rego-fmt: conftest fmt --check accepted an unformatted copy of $dir" >&2
    return 1
  fi
  rm -rf "$probe"
  if ! "${CONFTEST:-conftest}" fmt --check "$dir"; then
    echo "rego-fmt: $dir is not formatted; run conftest fmt $dir" >&2
    return 1
  fi
}
