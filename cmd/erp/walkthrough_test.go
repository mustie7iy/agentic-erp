package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	addrs := freeAddrs(t, 2)
	// The last value of a variable wins: a developer's own walkthrough
	// settings (keep the server, stop early, a work directory) do not apply.
	run.Env = append(os.Environ(), "DATABASE_URL="+db, "WALKTHROUGH_PAUSE=0", "WALKTHROUGH_BIN="+bin,
		"WALKTHROUGH_API_ADDR="+addrs[0], "WALKTHROUGH_ADMIN_ADDR="+addrs[1], "WALKTHROUGH_KEEP=0", "WALKTHROUGH_UNTIL=", "WALKTHROUGH_DIR=")
	start := time.Now()
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("walkthrough: %v after %s\n%s", err, time.Since(start).Round(time.Second), tail(string(out), 120))
	}
	for _, want := range []string{"Step 15 — GR/IR maintenance", "Step 16 — The audit trail", "none failed"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("walkthrough output: got\n%s\nwant it to contain %q", tail(string(out), 40), want)
		}
	}
	if strings.Contains(string(out), "UNEXPECTED") {
		t.Errorf("walkthrough output: got an unexpected result\n%s", tail(string(out), 60))
	}
	t.Logf("walkthrough passed in %s", time.Since(start).Round(time.Second))
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
