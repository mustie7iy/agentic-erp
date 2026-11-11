#!/usr/bin/env bash
# Tests scripts/ci-shards.sh on a package list and a tree made here: that
# every package lands in exactly one shard without being named anywhere,
# that a heavy package is split by test function with each function in
# exactly one part, and that the checks fail when one would be left out
# (06 §6.5). The plan of the real tree is checked where Go is at hand:
# `ci-shards.sh check` in ci.yml's shards job and in scripts/ci-local.sh.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0

tree="$work/tree"
mkdir -p "$tree/internal/heavy" "$tree/internal/huge" "$tree/internal/light" "$tree/.github/workflows"
# heavy: 500 s a run, 1500 s for three, so two parts; huge: three parts.
printf '%s\n' "# seconds package" "500 internal/heavy" "1100 internal/huge" "300 internal/medium" >"$work/weights"
cat >"$tree/internal/heavy/a_test.go" <<'EOF2'
package heavy

func TestMain(m *testing.M) {}
func TestAlpha(t *testing.T) {}
func TestCharlie_x(t *testing.T) {}
func helper() {}
func (s *suite) TestMethod(t *testing.T) {}
EOF2
cat >"$tree/internal/heavy/b_test.go" <<'EOF2'
package heavy_test

func TestBravo(t *testing.T) {}
func FuzzDelta(f *testing.F) {}
func ExampleEcho() {}
func BenchmarkFoxtrot(b *testing.B) {}
EOF2
printf 'package huge\n\nfunc TestOne(t *testing.T) {}\nfunc TestTwo(t *testing.T) {}\nfunc TestThree(t *testing.T) {}\nfunc TestFour(t *testing.T) {}\n' >"$tree/internal/huge/a_test.go"
printf '%s\n' api internal/heavy internal/huge internal/light internal/medium cmd/tool >"$work/packages"
# The workflow's coverage jobs: one save, and one restore per shard.
workflow() { # workflow N... writes a ci.yml restoring the shards N...
  local n
  {
    echo '          key: coverage-${{ github.run_id }}-shard${{ matrix.shard }}-${{ github.run_attempt }}'
    for n in "$@"; do
      echo "          key: coverage-\${{ github.run_id }}-shard$n-\${{ github.run_attempt }}"
      echo "          restore-keys: coverage-\${{ github.run_id }}-shard$n-"
    done
  } >"$tree/.github/workflows/ci.yml"
}
workflow 1 2 3 4

shards() {
  env CI_SHARDS_ROOT="$tree" CI_SHARDS_PACKAGES="$work/packages" CI_SHARDS_WEIGHTS="$work/weights" "$root/scripts/ci-shards.sh" "$@"
}
# is NAME GOT WANT
is() {
  if [ "$2" != "$3" ]; then
    echo "FAIL $1: got $2, want $3" >&2
    failed=1
  else
    echo "ok   $1"
  fi
}
# refuses NAME PATTERN CMD... expects CMD to fail with a line matching PATTERN.
refuses() {
  local name="$1" pattern="$2" out status=0
  shift 2
  out="$("$@" 2>&1)" || status=$?
  if [ "$status" = 0 ]; then
    echo "FAIL $name: it passed: $out" >&2
    failed=1
  elif ! grep -Eq -- "$pattern" <<<"$out"; then
    echo "FAIL $name: got $out, want a line matching $pattern" >&2
    failed=1
  else
    echo "ok   $name"
  fi
}

plan="$(shards plan race)"
is "every package is in the race plan, none named in the script" "$(awk '{print $2}' <<<"$plan" | sort -u | paste -sd' ' -)" \
  "api cmd/tool internal/heavy internal/huge internal/light internal/medium"
is "a package over the split limit is split, the others whole" "$(awk '{print $2 ":" $3 "/" $4}' <<<"$plan" | sort | paste -sd' ' -)" \
  "api:1/1 cmd/tool:1/1 internal/heavy:1/2 internal/heavy:2/2 internal/huge:1/3 internal/huge:2/3 internal/huge:3/3 internal/light:1/1 internal/medium:1/1"
# Costs: heavy 2 x 1125, huge 3 x 1650, medium 900, three at the default 30:
# 8190 in all, so four shards of 2400.
is "the shards are as many as the cost needs" "$(awk '{print $1}' <<<"$plan" | sort -un | paste -sd' ' -)" "1 2 3 4"
is "the heaviest pieces go to different shards" "$(awk '$2 == "internal/huge" {print $1}' <<<"$plan" | sort -u | wc -l | tr -d ' ')" "3"
is "the plan is the same every time" "$(shards plan race)" "$plan"
is "the matrices" "$(shards matrix | paste -sd' ' -)" "race=[1,2,3,4] coverage=[1,2,3,4]"
is "the coverage plan holds each package once, whole" "$(shards plan coverage | awk '{print $2 ":" $3 "/" $4}' | sort | paste -sd' ' -)" \
  "api:1/1 cmd/tool:1/1 internal/heavy:1/1 internal/huge:1/1 internal/light:1/1 internal/medium:1/1"
is "the coverage shards are as many as asked" "$(CI_SHARDS_COVERAGE=2 shards plan coverage | awk '{print $1}' | sort -un | paste -sd' ' -)" "1 2"

is "a part's tests: Test, Fuzz and Example functions, sorted, dealt in turn" "$(shards tests internal/heavy 1 2 | paste -sd' ' -)" "ExampleEcho TestAlpha TestCharlie_x"
is "the other part's" "$(shards tests internal/heavy 2 2 | paste -sd' ' -)" "FuzzDelta TestBravo"
is "the check passes" "$(shards check)" "ci-shards: 6 packages, each in one race shard of 4 and one coverage shard of 4"

echo internal/new >>"$work/packages"
is "a new package lands in a shard without being named" "$(shards plan race | awk '$2 == "internal/new" {print $3 "/" $4}')" "1/1"
is "and in a coverage shard" "$(shards plan coverage | awk '$2 == "internal/new"' | wc -l | tr -d ' ')" "1"

# A split package needs a test in every part.
printf 'package huge\n\nfunc TestOne(t *testing.T) {}\nfunc TestTwo(t *testing.T) {}\n' >"$tree/internal/huge/a_test.go"
refuses "a part without a test fails the check" "part 3 of 3 of internal/huge has no test" shards check
printf 'package huge\n\nfunc TestOne(t *testing.T) {}\nfunc TestTwo(t *testing.T) {}\nfunc TestThree(t *testing.T) {}\n' >"$tree/internal/huge/a_test.go"

# The workflow restores one profile per coverage shard.
workflow 1 2 3
refuses "a coverage shard the gate does not restore fails the check" "restores the coverage profiles of shards \[1 2 3\], the plan has \[1 2 3 4\]" shards check
workflow 1 2 3 4 5
refuses "a restore without a shard fails the check" "shards \[1 2 3 4 5\], the plan has \[1 2 3 4\]" shards check
workflow 1 2 3 3 4
refuses "a shard restored twice fails the check" "shards \[1 2 3 3 4\]" shards check
workflow 1 2 3 4
is "the check passes again" "$(shards check | wc -l | tr -d ' ')" "1"

# The compiled test binary has the last word on what a package's tests
# are: a stand-in for go lists one the files do not show.
mkdir -p "$work/bin"
cat >"$work/bin/go" <<'EOF2'
#!/usr/bin/env bash
[ "$1" = test ] || exit 2
printf '%s\n' TestAlpha TestBravo TestCharlie_x FuzzDelta ExampleEcho BenchmarkFoxtrot ${GHOST:-} "ok  	example.com/heavy	0.1s"
EOF2
chmod +x "$work/bin/go"
is "the parts hold every test the binary lists" "$(PATH="$work/bin:$PATH" shards verify internal/heavy 2 -race 2>&1)" ""
ghost() { PATH="$work/bin:$PATH" GHOST=TestGenerated shards verify internal/heavy 2 -race; }
refuses "a test the files do not show fails the shard that runs the package" "^TestGenerated\$" ghost

# The gate takes the profile of every shard of the plan, named by the plan.
m=github.com/mustie7iy/agentic-erp
mkdir -p "$work/profiles"
for n in 1 2 3 4; do printf '%s\n' "mode: set" "$m/internal/p$n/x.go:1.1,2.2 $n 1" >"$work/profiles/shard-$n.profile"; done
is "the gate counts the profiles of all the plan's shards" "$(COVERAGE_DIR="$work/profiles" shards gate)" "statement coverage: 10 of 10 statements (minimum 100.0%)"
gate() { COVERAGE_DIR="$work/profiles" shards gate; }
gate5() { CI_SHARDS_COVERAGE=5 gate; }
refuses "a plan of more shards than profiles fails the gate" "no profile at $work/profiles/shard-5.profile" gate5
mv "$work/profiles/shard-3.profile" "$work/profiles/kept"
refuses "a shard whose profile is missing fails the gate" "no profile at $work/profiles/shard-3.profile" gate
refuses "the gate needs the profiles' directory" "COVERAGE_DIR" shards gate

refuses "a mode the plan does not know" "plan race\|coverage" shards plan fast
refuses "no command" "usage: ci-shards.sh" shards
finished
exit "$failed"
