#!/usr/bin/env bash
# Tests scripts/vulncheck.sh, the govulncheck gate, on the repository itself
# with a fake govulncheck printing a scan in which GO-2026-6974 is called,
# and exceptions files of the test's own, so the cases do not hang on the
# real file's expiring entries. cmd/vulngate's tests cover the gate's rules
# (expired, unused, other module); these, the wiring: the JSON flags, the
# module named from the directory, and govulncheck's own failure. It needs
# Go (it builds cmd/vulngate), so it runs in ci.yml's static checks job
# and in scripts/ci-local.sh, not in the Go-less changes job.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0

check() { # check NAME GOT WANT
  if [ "$2" != "$3" ]; then
    echo "FAIL $1: got $(tr '\n' ' ' <<<"$2"), want $(tr '\n' ' ' <<<"$3")" >&2
    failed=1
  else
    echo "ok   $1"
  fi
}

cat >"$work/fake-govulncheck" <<'EOF'
#!/usr/bin/env bash
[ "$*" = "-format json ./..." ] || { echo "fake govulncheck: got $*" >&2; exit 5; }
[ -z "${FAKE_FAIL:-}" ] || exit 4
echo '{"config":{"scan_level":"symbol"}}'
echo '{"finding":{"osv":"GO-2026-6974","fixed_version":"v1.47.0","trace":[{"module":"o","version":"v1","package":"o/p","function":"F"},{"module":"m","package":"m/c","function":"main"}]}}'
EOF
chmod +x "$work/fake-govulncheck"
printf 'exceptions:\n  - {id: GO-2026-6974, module: temporal, expires: "2999-12-31", reason: test}\n' >"$work/accepts.yaml"
: >"$work/none.yaml"

vulncheck() { # vulncheck DIR EXCEPTIONS: the exit status and the gate's summary line
  local rc=0 out
  out="$(cd "$root/$1" && GOVULNCHECK_EXCEPTIONS="$work/$2" "$root/scripts/vulncheck.sh" "$work/fake-govulncheck" 2>&1)" || rc=$?
  echo "$rc $(grep '^vulngate:' <<<"$out" || true)"
}
check "an accepted vulnerability passes in its module" "$(vulncheck temporal accepts.yaml)" \
  "0 vulngate: temporal: 1 called vulnerabilities, 1 accepted by an exception, 0 not"
check "a called vulnerability without an exception fails" "$(vulncheck temporal none.yaml)" \
  "1 vulngate: temporal: 1 called vulnerabilities, 0 accepted by an exception, 1 not"
check "the root's module is \".\", which the exception does not name" "$(vulncheck . accepts.yaml)" \
  "1 vulngate: .: 1 called vulnerabilities, 0 accepted by an exception, 1 not"
check "govulncheck's own failure is the gate's, and no gate runs" "$(FAKE_FAIL=1 vulncheck temporal accepts.yaml)" "4 "
rc=0
(cd "$work" && "$root/scripts/vulncheck.sh" "$work/fake-govulncheck") >/dev/null 2>&1 || rc=$?
check "a directory outside the repository is refused" "$rc" 2
rc=0
"$root/scripts/vulncheck.sh" >/dev/null 2>&1 || rc=$?
check "no command is refused" "$rc" 2
finished
exit "$failed"
