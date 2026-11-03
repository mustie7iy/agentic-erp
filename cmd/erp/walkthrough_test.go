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
	addrs := freeAddrs(t, 3)
	// The last value of a variable wins: a developer's own walkthrough
	// settings (keep the server, stop early, a work directory) do not apply.
	run.Env = append(os.Environ(), "DATABASE_URL="+db, "WALKTHROUGH_PAUSE=0", "WALKTHROUGH_BIN="+bin,
		"WALKTHROUGH_API_ADDR="+addrs[0], "WALKTHROUGH_ADMIN_ADDR="+addrs[1], "WALKTHROUGH_MODEL_ADDR="+addrs[2], "WALKTHROUGH_KEEP=0", "WALKTHROUGH_UNTIL=", "WALKTHROUGH_DIR=")
	start := time.Now()
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("walkthrough: %v after %s\n%s", err, time.Since(start).Round(time.Second), tail(string(out), 120))
	}
	for _, want := range []string{"Step 15 — GR/IR maintenance", "Step 16 — SAP's open down payment", "Step 17 — The audit trail", "Step 18 — A scanned invoice above the request limit is captured by upload",
		"Step 19 — Asset accounting", "Step 20 — The asset master", "Step 21 — The liquidity forecast by liquidity item", "Step 22 — Asset acquisitions", "none failed"} {
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
