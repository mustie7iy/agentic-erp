package api

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

// acceptedReason is the comment an accepted breaking change sits under:
// the tracker item that accepted it and why, "# T27 (ADR-0012): reason".
var acceptedReason = regexp.MustCompile(`^# (T[0-9]+[a-z]?)\b[^:]*: *\S.{9,}$`)

// checkAccepted reports every entry of a breaking-accepted list that does
// not sit under a comment naming its T-number and a reason (ADR-0041).
func checkAccepted(r io.Reader) []string {
	var problems []string
	reason := ""
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			reason = line
		case !acceptedReason.MatchString(reason):
			problems = append(problems, fmt.Sprintf("line %d: %q is not under a comment \"# T<n>: <reason>\"", n, line))
		}
	}
	return problems
}

// TestBreakingAccepted: every breaking change oasdiff is told to ignore
// names the tracker item that accepted it and the reason.
func TestBreakingAccepted(t *testing.T) {
	f, err := os.Open("breaking-accepted.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read only: a close error loses nothing
	for _, p := range checkAccepted(f) {
		t.Error(p)
	}
}

func TestCheckAccepted(t *testing.T) {
	const entry = "POST /v1/x removed the success response with the status `200`\n"
	for _, c := range []struct {
		name, src string
		problems  int
	}{
		{"empty", "# header only\n", 0},
		{"accepted", "# header\n# T27 (ADR-0012 accepted): the release answers 202\n" + entry + entry, 0},
		{"lettered item", "# T50a: the operation moved to /v2 after its sunset\n" + entry, 0},
		{"no comment", entry, 1},
		{"header only", "# Breaking changes accepted before v1 has consumers\n\n" + entry, 1},
		{"no reason", "# T27:\n" + entry, 1},
		{"short reason", "# T27: because\n" + entry, 1},
		{"no T-number", "# ADR-0012: the release answers 202\n" + entry, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := checkAccepted(strings.NewReader(c.src)); len(got) != c.problems {
				t.Errorf("got %d problems %v, want %d", len(got), got, c.problems)
			}
		})
	}
}
