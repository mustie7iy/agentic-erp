#!/usr/bin/env bash
# Tests scripts/ci-changes.sh on a throwaway repository: when CI's jobs run
# and when they are skipped. What only GitHub can show (the skipped job
# counting as passed) is left to the first runs there.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# A stand-in for gh: the last green commit on main is FAKE_GREEN, given
# only to a query for successful pushes to main, so a query that lost one
# of those filters finds nothing and the push runs.
mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'EOF2'
#!/usr/bin/env bash
[ "$1 $2" = "run list" ] || exit 2
args=" $* "
for want in "--status success" "--branch main" "--event push"; do
  [[ "$args" == *" $want "* ]] || exit 0
done
echo "${FAKE_GREEN:-}"
EOF2
chmod +x "$work/bin/gh"

repo="$work/repo"
git init -q -b main "$repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
change() { # change adds a commit touching the files $@ and prints it
  local f
  for f in "$@"; do
    mkdir -p "$(dirname "$repo/$f")"
    echo "$RANDOM" >>"$repo/$f"
    git -C "$repo" add "$f"
  done
  git -C "$repo" commit -q -m "change $*"
  git -C "$repo" rev-parse HEAD
}
base="$(change main.go)"

# expect runs ci-changes.sh with the environment given and checks its run=.
expect() {
  local name="$1" want="$2" got
  shift 2
  got="$(cd "$repo" && env PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r "$@" "$root/scripts/ci-changes.sh")"
  if [ "$(sed -n 's/^run=//p' <<<"$got")" != "$want" ]; then
    echo "FAIL $name: got $(tr '\n' ' ' <<<"$got"), want run=$want" >&2
    failed=1
  else
    echo "ok   $name: $(sed -n 's/^reason=//p' <<<"$got")"
  fi
}

docs="$(change docs/design/x.md README.md)"
expect "a pull request changing only documentation" false EVENT=pull_request BASE="$base" HEAD="$docs"
code="$(change internal/x.go)"
expect "a pull request changing code" true EVENT=pull_request BASE="$base" HEAD="$code"
tracker="$(change docs/tracker.json)"
expect "the tracker is documentation" false EVENT=pull_request BASE="$code" HEAD="$tracker"
expect "a push with no green run on main" true EVENT=push HEAD="$code"
expect "a push of documentation since the last green commit" false EVENT=push FAKE_GREEN="$code" HEAD="$tracker"
expect "documentation after code whose run was cancelled" true EVENT=push FAKE_GREEN="$docs" HEAD="$tracker"
expect "a last green commit that is not an ancestor" true EVENT=push FAKE_GREEN=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef HEAD="$tracker"
# A manual run (the full suite on main, PO 2026-10-27) runs everything, even
# when only documentation changed since the base it is given.
expect "a manual run never skips" true EVENT=workflow_dispatch BASE="$code" HEAD="$tracker"

# expect_out checks one more output line ($3=$4) for the change $2.
expect_out() {
  local name="$1" commit="$2" key="$3" want="$4" got
  got="$(cd "$repo" && env PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r EVENT=pull_request BASE="$prev" HEAD="$commit" \
    "$root/scripts/ci-changes.sh" | sed -n "s/^$key=//p")"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: got $key=$got, want $want" >&2
    failed=1
  else
    echo "ok   $name: $key=$got"
  fi
}
prev="$(git -C "$repo" rev-parse HEAD)"
c="$(change internal/z.go)"
expect_out "Go code alone leaves the image checks out" "$c" images false
expect_out "Go code alone leaves the release scripts out" "$c" release false
prev="$c"
c="$(change .ko.yaml)"
expect_out "the image build's configuration runs the image checks" "$c" images true
prev="$c"
c="$(change scripts/sign-images.sh)"
expect_out "a signing script runs the image checks" "$c" images true
prev="$c"
c="$(change deploy/images/erp-analytics.Dockerfile)"
expect_out "erp-analytics's Dockerfile runs the image checks" "$c" images true
prev="$c"
c="$(change scripts/test-images.sh)"
expect_out "the image scripts' tests run the image checks" "$c" images true
prev="$c"
c="$(change scripts/free-disk.sh)"
expect_out "the runner's disk clean-up runs the image checks" "$c" images true
prev="$c"
c="$(change temporal/cmd/erp-temporal/main.go)"
expect_out "erp-temporal's command runs the image checks" "$c" images true
prev="$c"
c="$(change temporal/go.sum)"
expect_out "erp-temporal's modules run the image checks" "$c" images true
prev="$c"
c="$(change temporal/spiffeauth/x.go)"
expect_out "erp-temporal's packages alone leave the image checks out, as the platform's do" "$c" images false
prev="$c"
c="$(change cliff.toml)"
expect_out "the changelog's configuration runs the release scripts" "$c" release true
prev="$c"
c="$(change docs/x.md)"
expect_out "documentation runs neither" "$c" images false
expect_out "documentation outside deploy/ leaves the deploy checks out" "$c" deploy false
prev="$c"
c="$(change internal/y.go)"
expect_out "Go code alone leaves the deploy checks out" "$c" deploy false
expect_out "Go code alone leaves the promotion self-test out" "$c" promotion false
prev="$c"
c="$(change deploy/helm/erp-cell/values.yaml)"
expect_out "a chart runs the deploy checks" "$c" deploy true
prev="$c"
c="$(change deploy/environments/dev/dev1/release.json)"
expect_out "a promotion is not the deploy workflow's" "$c" deploy false
prev="$c"
c="$(change deploy/terraform/README.md)"
expect_out "documentation in deploy/ runs the deploy checks, as its path filter does" "$c" deploy true
prev="$c"
c="$(change temporal/main.go)"
expect_out "the workflow engine's module runs the deploy checks" "$c" deploy true
prev="$c"
c="$(change scripts/nats-checks.sh)"
expect_out "a deploy check's script runs the deploy checks" "$c" deploy true
prev="$c"
c="$(change scripts/lib/registry.sh)"
expect_out "the checks' shared library runs the deploy checks" "$c" deploy true
prev="$c"
c="$(change .github/workflows/weekly.yml)"
expect_out "any workflow runs the deploy checks (actionlint)" "$c" deploy true
prev="$c"
c="$(change scripts/test-rollback-drill.sh)"
expect_out "a promotion check's test runs the promotion self-test" "$c" promotion true
expect_out "a promotion check's test is not the deploy workflow's" "$c" deploy false
prev="$c"
c="$(change .github/approvers)"
expect_out "the approvers run the promotion self-test" "$c" promotion true

# The deploy and promotion classes are deploy.yml's and
# promotion-self-test.yml's path filters: each path a filter lists is in
# the class, and one it excludes is not.
filter() { # filter WORKFLOW CLASS
  local pattern file want
  while read -r pattern; do
    file="${pattern#!}"
    file="${file//\*\*/x/y.tf}"
    want=true
    [ "$pattern" = "${pattern#!}" ] || want=false
    prev="$(git -C "$repo" rev-parse HEAD)"
    c="$(change "$file")"
    expect_out "$1's path $pattern ($file)" "$c" "$2" "$want"
  done < <(sed -n '/^    paths:/,/^[^ ]/s/^      - "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$root/.github/workflows/$1")
}
filter deploy.yml deploy
filter promotion-self-test.yml promotion

# A comparison git cannot make fails the script, so the step fails and the
# gate (run != 'false') does not skip anything.
if (cd "$repo" && EVENT=pull_request BASE="$base" HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef "$root/scripts/ci-changes.sh") >/dev/null 2>&1; then
  echo "FAIL a comparison git cannot make: the script succeeded" >&2
  failed=1
else
  echo "ok   a comparison git cannot make fails the script"
fi
exit "$failed"
