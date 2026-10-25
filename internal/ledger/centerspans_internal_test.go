package ledger

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func d(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

// show renders spans as from..to=attr, "" for an open end.
func show(spans []span[int]) string {
	out := ""
	for _, s := range spans {
		out += fmt.Sprintf("%s..%s=%d ", dateText(s.From), dateText(s.To), s.Attr)
	}
	return out
}

func spansOf(parts ...any) []span[int] {
	var out []span[int]
	for i := 0; i < len(parts); i += 3 {
		out = append(out, span[int]{ID: uuid.New(), From: d(parts[i].(string)), To: d(parts[i+1].(string)), Attr: parts[i+2].(int)})
	}
	return out
}

// TestSetFrom: a range splits the spans it starts and ends in, and alike
// neighbours join, keeping the periods that stay as they were.
func TestSetFrom(t *testing.T) {
	two := spansOf("2026-01-01", "2026-07-01", 1, "2026-07-01", "", 2)
	set := func(v int) func(int) int { return func(int) int { return v } }
	for name, c := range map[string]struct {
		from, to string
		v        int
		want     string
		kept     int
	}{
		"inside the first":       {"2026-03-01", "2026-04-01", 9, "2026-01-01..2026-03-01=1 2026-03-01..2026-04-01=9 2026-04-01..2026-07-01=1 2026-07-01..=2 ", 1},
		"across both to the end": {"2026-03-01", "", 9, "2026-01-01..2026-03-01=1 2026-03-01..=9 ", 0},
		"joining the second":     {"2026-03-01", "2026-07-01", 2, "2026-01-01..2026-03-01=1 2026-03-01..=2 ", 0},
		"to the same":            {"2026-01-01", "2026-07-01", 1, "2026-01-01..2026-07-01=1 2026-07-01..=2 ", 2},
	} {
		t.Run(name, func(t *testing.T) {
			got := setFrom(two, d(c.from), d(c.to), set(c.v))
			if show(got) != c.want {
				t.Errorf("got %s, want %s", show(got), c.want)
			}
			kept := 0
			for _, s := range got {
				if s.ID != uuid.Nil {
					kept++
				}
			}
			if kept != c.kept {
				t.Errorf("kept periods: got %d, want %d", kept, c.kept)
			}
		})
	}
}

// TestRedated: a new validity drops, cuts and stretches spans.
func TestRedated(t *testing.T) {
	two := spansOf("2026-01-01", "2026-07-01", 1, "2026-07-01", "2027-01-01", 2)
	for name, c := range map[string]struct{ from, to, want string }{
		"stretched":         {"2025-01-01", "", "2025-01-01..2026-07-01=1 2026-07-01..=2 "},
		"cut to the second": {"2026-08-01", "2026-09-01", "2026-08-01..2026-09-01=2 "},
		"nothing left":      {"2027-02-01", "2027-03-01", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := redated(two, d(c.from), d(c.to))
			if show(got) != c.want || ok != (c.want != "") {
				t.Errorf("got %s (%v), want %s", show(got), ok, c.want)
			}
		})
	}
}

// TestDiffSpans: what a change retires and inserts.
func TestDiffSpans(t *testing.T) {
	old := spansOf("2026-01-01", "", 1)
	got := setFrom(old, d("2026-03-01"), time.Time{}, func(int) int { return 2 })
	retire, insert, differ := diffSpans(old, got)
	if !differ || len(retire) != 1 || retire[0] != old[0].ID || len(insert) != 2 {
		t.Errorf("got retire %v insert %s differ %v, want the old period retired and two inserted", retire, show(insert), differ)
	}
	if _, _, differ := diffSpans(old, keepIDs(old, spansOf("2026-01-01", "", 1))); differ {
		t.Error("the same spans: got a difference, want none")
	}
}

// TestChangedRanges: the ranges of a change say which lines read what
// differs, and neighbouring ranges join only when they name the same lines
// (T56d4): a cost lock range next to a revenue lock range stays two. The
// dates before the first period count as its own (a default's postings).
func TestChangedRanges(t *testing.T) {
	attrs := func(cost, revenue bool) CostCenterAttrs {
		return CostCenterAttrs{Category: "ADMIN", LockCost: cost, LockRevenue: revenue}
	}
	sp := func(from, to string, a CostCenterAttrs) span[CostCenterAttrs] {
		return span[CostCenterAttrs]{From: d(from), To: d(to), Attr: a}
	}
	old := []span[CostCenterAttrs]{sp("2026-01-01", "2026-02-01", attrs(false, false)), sp("2026-02-01", "", attrs(true, true))}
	show := func(rs []dateRange) string {
		out := ""
		for _, r := range rs {
			out += fmt.Sprintf("%s..%s=%d ", dateText(r.From), dateText(r.To), r.Lines)
		}
		return out
	}
	for name, c := range map[string]struct {
		after []span[CostCenterAttrs]
		want  string
	}{
		"a cost lock next to a revenue unlock": {[]span[CostCenterAttrs]{sp("2026-01-01", "2026-03-01", attrs(true, false)), sp("2026-03-01", "", attrs(true, true))},
			fmt.Sprintf("..2026-02-01=%d 2026-02-01..2026-03-01=%d ", costLines, revenueLines)},
		"a revenue unlock to the end": {[]span[CostCenterAttrs]{sp("2026-01-01", "2026-03-01", attrs(true, false)), sp("2026-03-01", "", attrs(true, false))},
			fmt.Sprintf("..2026-02-01=%d 2026-02-01..=%d ", costLines, revenueLines)},
		"a later start and a plan lock": {[]span[CostCenterAttrs]{sp("2026-01-15", "2026-02-01", attrs(false, false)), sp("2026-02-01", "", CostCenterAttrs{Category: "ADMIN",
			LockCost: true, LockRevenue: true, LockPlan: true})}, fmt.Sprintf("2026-01-01..2026-01-15=%d ", everyLine)},
		"another category": {[]span[CostCenterAttrs]{sp("2026-01-01", "2026-02-01", attrs(false, false)), sp("2026-02-01", "", CostCenterAttrs{Category: "SALES",
			LockCost: true, LockRevenue: true})}, fmt.Sprintf("2026-02-01..=%d ", everyLine)},
	} {
		t.Run(name, func(t *testing.T) {
			if got := show(changedRanges(old, c.after, costCenterReaders)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
