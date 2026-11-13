package watchers

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

func eur(units money.Minor) analytics.Value {
	return analytics.Value{Units: units, Scale: 2, Currency: "EUR"}
}

// TestQuery: a metric with a currency dimension is broken down by it; the
// year grain reads fourteen years back.
func TestQuery(t *testing.T) {
	m, err := analytics.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		w    Watcher
		want string
	}{
		"spend by month":  {Watcher{Metric: "spend", Grain: Month}, "[company currency fiscal_year fiscal_month] [{fiscal_year >= [2024]}]"},
		"revenue by year": {Watcher{Metric: "revenue", Grain: Year}, "[company fiscal_year] [{fiscal_year >= [2012]}]"},
	} {
		t.Run(name, func(t *testing.T) {
			q := c.w.query(m, 2026)
			if got := fmt.Sprint(q.By, " ", q.Where); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestFiguresOf: a row without a value, or with a period that is not a
// number, is left out.
func TestFiguresOf(t *testing.T) {
	res := &analytics.Result{Rows: []analytics.Row{
		{"2000", "2026", "3", nil}, {"2000", "x", "3", eur(1)}, {"2000", "2026", "y", eur(1)}, {"2000", "2026", "2", eur(7)},
	}}
	f := Watcher{Grain: Month}.figuresOf(res)
	if got := fmt.Sprint(f); got != "map[{2000 EUR}:map[{2026 2}:0.07]]" {
		t.Errorf("got %s, want February's figure alone", got)
	}
}

// TestBefore: periods before one cross fiscal years by month and by year.
func TestBefore(t *testing.T) {
	for _, c := range []struct {
		p    period
		n    int
		want period
	}{
		{period{2026, 3}, 1, period{2026, 2}}, {period{2026, 1}, 1, period{2025, 12}}, {period{2026, 2}, 14, period{2024, 12}},
		{period{2026, 0}, 3, period{2023, 0}},
	} {
		if got := c.p.before(c.n); got != c.want {
			t.Errorf("%v before %d: got %v, want %v", c.p, c.n, got, c.want)
		}
	}
}

// TestChange: a change from zero judges nothing; a percentage out of an
// amount's range is compared, not shown; a fall is a change too.
func TestChange(t *testing.T) {
	w := Watcher{Grain: Month, Condition: Condition{Kind: Change, Percent: "50"}}
	for name, c := range map[string]struct {
		before, now money.Minor
		want        string
	}{
		"from zero":    {0, 100, "{2000 EUR 1.00   false}"},
		"a fall":       {200, 50, "{2000 EUR 0.50 2.00 -75.00 true}"},
		"out of range": {1, money.Minor(9e15), "{2000 EUR 90000000000000.00 0.01  true}"},
	} {
		t.Run(name, func(t *testing.T) {
			f := figures{{"2000", "EUR"}: {{2026, 2}: eur(c.before), {2026, 3}: eur(c.now)}}
			if got := fmt.Sprint(w.judge(f, period{2026, 3})); got != "["+c.want+"]" {
				t.Errorf("got %s, want [%s]", got, c.want)
			}
		})
	}
}
