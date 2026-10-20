package analytics

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestLedgers: journal metrics read the leading ledger unless the query
// names another; facts of tables without ledgers are in every one.
func TestLedgers(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	leading, parallel := uuid.NewString(), uuid.NewString()
	expense := map[string]string{"company": "2000", "account_type": "expense"}
	f := &Facts{Exponents: map[string]int{"EUR": 2}, Tables: map[string][]Fact{
		"gl_lines": {{ID: "a", Dims: expense, Amount: 12000, Currency: "EUR", Ledger: leading, Leading: true},
			{ID: "b", Dims: expense, Amount: 99900, Currency: "EUR", Ledger: parallel}},
		"po_lines": {{ID: "p", Dims: map[string]string{"company": "2000"}, Amount: 500, Currency: "EUR"}},
	}}
	for name, tc := range map[string]struct {
		q    Query
		want string
	}{
		"leading by default":  {Query{Measures: []string{"expenses"}}, "120.00"},
		"named ledger":        {Query{Measures: []string{"expenses"}, Ledger: parallel}, "999.00"},
		"no ledger to orders": {Query{Measures: []string{"purchase_order_value"}, Ledger: parallel}, "5.00"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Execute(m, f, Principal{}, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != tc.want {
				t.Errorf("rows: got %v, want %s", res.Rows, tc.want)
			}
		})
	}
	for _, l := range []string{"0L", strings.ToUpper(parallel), "urn:uuid:" + parallel} {
		if _, err := Execute(m, f, Principal{}, Query{Measures: []string{"expenses"}, Ledger: l}); err == nil {
			t.Errorf("ledger %s: got no error, want one: a ledger is named by its canonical id", l)
		}
	}
}

// TestAggregated: groups the store aggregated are folded in as they are,
// and derived metrics, counts and currencies computed over them.
func TestAggregated(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	by := func(c string) map[string]string { return map[string]string{"company": c} }
	f := &Facts{Source: SourceStore, Exponents: map[string]int{"EUR": 2}, Aggregated: map[string][]Fact{
		"revenue":              {{Dims: by("2000"), Amount: -50000, Currency: "EUR"}},
		"expenses":             {{Dims: by("2000"), Amount: 12000, Currency: "EUR"}},
		"purchase_order_count": {{Dims: by("2000"), Count: 3}, {Dims: by("1000"), Count: 2}},
	}}
	res, err := Execute(m, f, Principal{}, Query{Measures: []string{"operating_profit"}, By: []string{"company"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceStore || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][1]) != "380.00" || res.Columns[1].Currency != "EUR" {
		t.Errorf("operating profit: got %v from %s (%+v), want 380.00 EUR from the store", res.Rows, res.Source, res.Columns)
	}
	res, err = Execute(m, f, Principal{}, Query{Measures: []string{"purchase_order_count"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != "5" {
		t.Errorf("orders: got %v, want 5", res.Rows)
	}
}

func TestBaseMetrics(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.BaseMetrics("operating_margin"); !slices.Equal(got, []string{"revenue", "expenses"}) {
		t.Errorf("operating_margin: got %v, want revenue and expenses once each", got)
	}
	if got := m.BaseMetrics("nope"); got != nil {
		t.Errorf("unknown metric: got %v, want nil", got)
	}
}
