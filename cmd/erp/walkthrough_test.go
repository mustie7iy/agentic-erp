package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestWalkthrough runs the pilot walkthrough (scripts/walkthrough.sh, make
// walkthrough) as CI would show it to the product owner: without pauses,
// against binaries built from this tree, on a database of its own, so the
// story cannot drift from what the API does. It is the slowest test of the
// package (about a minute and a half, two builds of the binaries
// included), so -short skips it.
func TestWalkthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("the walkthrough builds the binaries and tells the whole story; it runs without -short")
	}
	db := pgtest.EmptyDatabase(t)
	bin := t.TempDir()
	for _, cmd := range []string{"erp", "erp-server"} {
		build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(bin, cmd), "../"+cmd)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", cmd, err, out)
		}
	}
	run := exec.CommandContext(t.Context(), "bash", "../../scripts/walkthrough.sh")
	addrs := freeAddrs(t, 5)
	// The last value of a variable wins: a developer's own walkthrough
	// settings (keep the server, stop early, a work directory) do not apply.
	run.Env = append(os.Environ(), "DATABASE_URL="+db, "WALKTHROUGH_PAUSE=0", "WALKTHROUGH_BIN="+bin,
		"WALKTHROUGH_API_ADDR="+addrs[0], "WALKTHROUGH_ADMIN_ADDR="+addrs[1], "WALKTHROUGH_MODEL_ADDR="+addrs[2],
		"WALKTHROUGH_NATS_ADDR="+addrs[3], "WALKTHROUGH_HOOK_ADDR="+addrs[4], "WALKTHROUGH_KEEP=0", "WALKTHROUGH_UNTIL=", "WALKTHROUGH_DIR=")
	start := time.Now()
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("walkthrough: %v after %s\n%s", err, time.Since(start).Round(time.Second), tail(string(out), 120))
	}
	for _, want := range []string{"Step 15 — GR/IR maintenance", "Step 16 — SAP's open down payment", "Step 17 — The audit trail", "Step 18 — A scanned invoice above the request limit is captured by upload",
		"Step 19 — Asset accounting", "Step 20 — The asset master", "Step 21 — The liquidity forecast by liquidity item", "Step 22 — Asset acquisitions", "Step 23 — The liquidity forecast opened, converted and drilled down",
		"Step 24 — Asset corrections", "Step 25 — Depreciation schedule and preview", "Step 26 — Snapshots of the liquidity forecast", "Step 27 — Cash actuals", "Step 28 — Forecast variance", "Step 29 — The depreciation run", "Step 30 — Variance comments", "Step 31 — Asset retirements", "Step 32 — Asset transfers", "Step 33 — Purchase orders", "Step 34 — The asset history sheet", "Step 35 — The treasury connector", "verified true, a changed body refused true", "Step 36 — The group liquidity forecast", "Step 37 — Cash actuals in analytics", "Step 38 — The forecast in analytics", "Final step — Financial close", "none failed"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("walkthrough output: got\n%s\nwant it to contain %q", tail(string(out), 40), want)
		}
	}
	if strings.Contains(string(out), "UNEXPECTED") {
		t.Errorf("walkthrough output: got an unexpected result\n%s", tail(string(out), 60))
	}
	t.Logf("walkthrough passed in %s", time.Since(start).Round(time.Second))
}

// TestWalkthroughDates: the walkthrough dates what it sends as the pilot
// company's day on the company's date in its zone, not UTC's
// (scripts/walkthrough/dates.sh). At 23:30 in Toronto UTC is already
// tomorrow; a return filed on UTC's date was refused as filed in the
// future (tax.invalid_filed_on), so the walkthrough failed every evening.
// The payment runs are dated on the company's day too: the migration
// loads the suppliers' bank accounts valid from the day before UTC's
// (partner.Migration, ADR-0051 As built; PO 2026-11-02), which is never
// after the company's day, in winter, in summer and on New Year's Eve,
// when the walkthrough used to stop. The instants are read with the
// system's date, GNU's or BSD's.
func TestWalkthroughDates(t *testing.T) {
	for name, c := range map[string]struct {
		at        time.Time
		want, utc string
	}{
		"23:30 in Toronto, winter": {time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC), "2026-11-01", "2026-11-02"},
		"23:30 in Toronto, summer": {time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC), "2026-06-30", "2026-07-01"},
		"00:30 in Toronto":         {time.Date(2026, 11, 2, 5, 30, 0, 0, time.UTC), "2026-11-02", "2026-11-02"},
		"New Year's Eve, 23:30":    {time.Date(2027, 1, 1, 4, 30, 0, 0, time.UTC), "2026-12-31", "2027-01-01"},
	} {
		t.Run(name, func(t *testing.T) {
			script := `. ../../scripts/walkthrough/dates.sh && company_day "$1" && utc_day "$1" && date_add "$(utc_day "$1")" -1`
			out, err := exec.CommandContext(t.Context(), "bash", "-c", script, "dates", strconv.FormatInt(c.at.Unix(), 10)).CombinedOutput()
			if err != nil {
				t.Fatalf("dates: %v\n%s", err, out)
			}
			days := strings.Fields(string(out))
			if len(days) != 3 {
				t.Fatalf("dates at %s: got %q, want the company's day, UTC's and the migrated bank accounts' first", c.at.Format(time.RFC3339), out)
			}
			runDay, utcDay, bankFrom := days[0], days[1], days[2]
			if runDay != c.want {
				t.Errorf("company_day at %s: got %s, want %s", c.at.Format(time.RFC3339), runDay, c.want)
			}
			if utcDay != c.utc {
				t.Errorf("utc_day at %s: got %s, want %s", c.at.Format(time.RFC3339), utcDay, c.utc)
			}
			// ISO dates compare as strings.
			if bankFrom > runDay {
				t.Errorf("at %s: the migrated bank accounts start %s, after the payment run's date %s (no_bank_account)", c.at.Format(time.RFC3339), bankFrom, runDay)
			}
		})
	}
	out, err := exec.CommandContext(t.Context(), "bash", "-c", `. ../../scripts/walkthrough/dates.sh && date_add 2026-12-31 1 && date_add 2026-03-01 -1`).CombinedOutput()
	if err != nil {
		t.Fatalf("date_add: %v\n%s", err, out)
	}
	if got, want := strings.Fields(string(out)), []string{"2027-01-01", "2026-02-28"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("date_add: got %v, want %v", got, want)
	}
}

// freeAddrs are n distinct loopback addresses no one listens on at the
// moment: each listener stays open until all are picked.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	addrs := make([]string, 0, n)
	for range n {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		}()
		addrs = append(addrs, l.Addr().String())
	}
	return addrs
}

// tail is the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestWalkthroughExitStatus: the walkthrough exits 0 only when every step
// of its story ran and passed or skipped, since its run is a slice's
// acceptance evidence (ADR-0053). A lane saw it stop on an unbound
// variable and exit 0 (review:T61j): macOS's bash 3.2 runs the EXIT trap
// with status 0 after an expansion error (scripts/lib/on-exit.sh), and a
// failed command or an unbound variable inside a command substitution
// stopped nothing when the substitution was an argument, a local's value
// or printed text (stop_unseen). The script runs here with its story
// replaced by three small steps, the failure injected into one, and its
// own helpers, cleanup and summary.
func TestWalkthroughExitStatus(t *testing.T) {
	for name, c := range map[string]struct {
		start, two, until string
		ok                bool
		want, unwanted    string
	}{
		"every step passes":                        {two: `passed`, ok: true, want: "3 steps passed, 0 skipped, none failed."},
		"a step skips":                             {two: `skipped "not built"`, ok: true, want: "2 steps passed, 1 skipped, none failed."},
		"an unbound variable":                      {two: `echo "$CA_PACK_NOT_SET"; passed`, unwanted: "none failed"},
		"an unbound variable at start":             {start: `echo "$CA_PACK_NOT_SET"`, two: `passed`, unwanted: "Step 1"},
		"a failed command":                         {two: `false; passed`, unwanted: "none failed"},
		"a check failed in a command substitution": {two: `expect "the account" "$(fail "no account 4711")" ""; passed`, want: "no account 4711", unwanted: "none failed"},
		"an unbound variable in a captured helper used as an argument": {two: `h() { echo "$CA_PACK_NOT_SET"; }; expect "the pack" "$(h)" ""; passed`, want: "a command substitution stopped with status 1", unwanted: "none failed"},
		"an unbound variable in a captured helper assigned to a local": {two: `h() { echo "$CA_PACK_NOT_SET"; }; local pack=$(h); passed`, want: "a command substitution stopped with status 1", unwanted: "none failed"},
		"a failed command in a captured helper only printed":           {two: `h() { false; echo 4711; }; say "the account: $(h)"; passed`, want: "the command `false` failed with status 1", unwanted: "none failed"},
		"a failed command in a captured helper assigned":               {two: `h() { false; echo 4711; }; account=$(h); say "$account"; passed`, want: "the command `false` failed with status 1", unwanted: "none failed"},
		"a failure tolerated inside a substitution":                    {two: `x=$(false || true); say "$x$(grep -c x /dev/null || true)"; passed`, ok: true, want: "3 steps passed, 0 skipped, none failed."},
		"an unbound variable in a subshell":                            {two: `( echo "$CA_PACK_NOT_SET" ); passed`, unwanted: "none failed"},
		"an unbound variable in a pipeline's function":                 {two: `h() { echo "$CA_PACK_NOT_SET"; }; h | cat; passed`, unwanted: "none failed"},
		"an unbound variable in a subshell inside a substitution":      {two: `h() { echo "$CA_PACK_NOT_SET"; }; say "$( ( h ) )"; passed`, want: "a command substitution stopped with status 1", unwanted: "none failed"},
		"a failure tolerated outside a substitution stops the run":     {two: `x=$(false) || true; passed`, unwanted: "none failed"},
		"a step neither passes nor skips":                              {two: `:`, want: "step_two ended having recorded 0 results", unwanted: "none failed"},
		"a step passes twice":                                          {two: `passed; passed`, want: "step_two ended having recorded 2 results", unwanted: "none failed"},
		"WALKTHROUGH_UNTIL stops early":                                {two: `passed`, until: "step_one", ok: true, want: "Stopped after step_one (WALKTHROUGH_UNTIL): 1 of the 3 steps ran", unwanted: "none failed"},
		"WALKTHROUGH_UNTIL names the last step":                        {two: `passed`, until: "step_three", ok: true, want: "3 steps passed, 0 skipped, none failed."},
	} {
		t.Run(name, func(t *testing.T) {
			script := stubWalkthrough(t, "start() {\nheading start\n"+c.start+"\n}\n"+
				"step_one() { heading \"Step 1\"; passed; }\n"+
				"step_two() {\nheading \"Step 2\"\n"+c.two+"\n}\n"+
				"step_three() { heading \"Step 3\"; passed; }\n"+
				"STORY='step_one step_two step_three'\n")
			run := exec.CommandContext(t.Context(), "bash", script)
			run.Env = append(os.Environ(), "TMPDIR="+t.TempDir(), "WALKTHROUGH_PAUSE=0", "WALKTHROUGH_KEEP=0", "WALKTHROUGH_DIR=", "WALKTHROUGH_UNTIL="+c.until)
			out, err := run.CombinedOutput()
			if got := err == nil; got != c.ok {
				t.Errorf("walkthrough exited 0: got %v, want %v (%v)\n%s", got, c.ok, err, out)
			}
			if c.want != "" && !strings.Contains(string(out), c.want) {
				t.Errorf("walkthrough output: got\n%s\nwant it to contain %q", out, c.want)
			}
			if c.unwanted != "" && strings.Contains(string(out), c.unwanted) {
				t.Errorf("walkthrough output: got\n%s\nwant it without %q", out, c.unwanted)
			}
		})
	}
}

// stubWalkthrough is scripts/walkthrough.sh with story put before its last
// line, main "$@", in a copy of the scripts it reads.
func stubWalkthrough(t *testing.T, story string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"walkthrough.sh", "walkthrough/dates.sh", "lib/on-exit.sh"} {
		b, err := os.ReadFile(filepath.Join("../../scripts", f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "walkthrough.sh" {
			const last = "\nmain \"$@\"\n"
			if !strings.HasSuffix(string(b), last) {
				t.Fatalf("scripts/walkthrough.sh: got a last line other than %q", strings.TrimSpace(last))
			}
			b = []byte(strings.TrimSuffix(string(b), last) + "\n" + story + last)
		}
		dst := filepath.Join(root, "scripts", f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "scripts", "walkthrough.sh")
}
