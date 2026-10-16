package apitest

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// TestLogsKeepTheRecentEnd: the API logs every request at debug level and a
// soak makes millions of them. What it keeps is bounded, starts on a whole
// line and ends with the latest one, so a test still finds what it just did.
func TestLogsKeepTheRecentEnd(t *testing.T) {
	a := New(t)
	line := strings.Repeat("x", 99) + "\n"
	for range (40 << 20) / len(line) {
		if _, err := a.logs.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := a.logs.Write([]byte("last\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	logs := a.Logs()
	if got := len(logs); got > 16<<20 {
		t.Errorf("logs kept: got %d MiB, want at most 16 MiB", got>>20)
	}
	if !strings.HasSuffix(logs, line+"last\n") {
		t.Errorf("logs end: got %q, want the latest lines", logs[max(0, len(logs)-20):])
	}
	if !strings.HasPrefix(logs, line) {
		t.Errorf("logs start: got %q, want a whole line", logs[:min(len(logs), 20)])
	}
}
