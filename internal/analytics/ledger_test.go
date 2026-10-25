package analytics

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
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
		"income":               {{Dims: by("2000"), Amount: -50000, Currency: "EUR"}},
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
	if got := m.BaseMetrics("operating_margin"); !slices.Equal(got, []string{"income", "expenses", "revenue"}) {
		t.Errorf("operating_margin: got %v, want income, expenses and revenue once each", got)
	}
	if got := m.BaseMetrics("nope"); got != nil {
		t.Errorf("unknown metric: got %v, want nil", got)
	}
}

// TestRevenueIsSales: revenue is net sales, the revenue accounts no account
// determination names; the cash discounts received and exchange gains that
// automatic postings book are income, which operating profit counts
// (walkthrough, 2026-10-22: 4800 Purchase discounts received counted as
// revenue).
func TestRevenueIsSales(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	line := func(account, determination string, amount money.Minor) Fact {
		return Fact{ID: account, Amount: amount, Currency: "CAD", Dims: map[string]string{"company": "1000", "account": account,
			"account_type": map[bool]string{true: "expense", false: "revenue"}[amount > 0], "determination": determination,
			"fiscal_year": "2026", "fiscal_month": "10", "posting_date": "2026-10-22"}}
	}
	f := &Facts{Companies: map[string]string{"1000": "CAD"}, Exponents: map[string]int{"CAD": 2}, Tables: map[string][]Fact{"gl_lines": {
		line("4000", "none", -100000), line("4800", "cash_discount", -9400), line("4900", "fx_gain", -600), line("6000", "none", 40000)}}}
	for metric, want := range map[string]string{"revenue": "1000.00", "income": "1100.00", "operating_profit": "700.00", "expenses": "400.00"} {
		t.Run(metric, func(t *testing.T) {
			res, err := Execute(m, f, Principal{}, Query{Measures: []string{metric}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != want {
				t.Errorf("%s: got %v, want %s", metric, res.Rows, want)
			}
		})
	}
}
