package analytics

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// amt is an amount as its currency writes it, its digits the exponent:
// amt("USD", "1000.00"), amt("JPY", "5000").
func amt(cur, s string) Value {
	exp := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		exp = len(s) - i - 1
	}
	m, err := money.ParseDecimal(s, exp)
	if err != nil {
		panic(err)
	}
	return Value{Units: m, Scale: exp, Currency: cur}
}

// usd is an amount in USD; ratio a ratio at ratioScale; count a count.
func usd(s string) Value { return amt("USD", s) }

func ratio(s string) Value {
	m, err := money.ParseDecimal(s, ratioScale)
	if err != nil {
		panic(err)
	}
	return Value{Units: m, Scale: ratioScale}
}

func count(n int64) Value { return Value{Units: money.Minor(n)} }

// exponentFacts holds revenue of a BHD company (3 decimals) and a JPY
// company (none): 1,234.567 BHD over two lines and 5,000 JPY.
func exponentFacts() *Facts {
	line := func(id, company, cur string, amount int64) Fact {
		return Fact{ID: id, Amount: -money.Minor(amount), Currency: cur, Dims: map[string]string{
			"company": company, "account_type": "revenue", "fiscal_year": "2026", "fiscal_month": "1", "posting_date": "2026-01-05"}}
	}
	return &Facts{
		Companies: map[string]string{"4000": "BHD", "3000": "JPY"},
		Exponents: map[string]int{"BHD": 3, "JPY": 0},
		Tables: map[string][]Fact{"gl_lines": {
			line("B1", "4000", "BHD", 1_000_123), line("B2", "4000", "BHD", 234_444), line("J1", "3000", "JPY", 5_000)}},
	}
}

// Money measures keep their currency's exponent from the query result to
// the page: a BHD total keeps its third decimal, a JPY total shows none.
func TestMoneyKeepsTheCurrencyExponent(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	f := exponentFacts()
	run := func(t *testing.T, company string) *Result {
		t.Helper()
		r, err := Execute(m, f, Principal{ID: "u", Companies: []string{company}}, Query{Measures: []string{"revenue"}})
		if err != nil {
			t.Fatalf("revenue of %s: %v", company, err)
		}
		return r
	}
	bhd, jpy := run(t, "4000"), run(t, "3000")
	t.Run("query rows", func(t *testing.T) {
		for _, c := range []struct {
			r    *Result
			want string
		}{{bhd, `[[1234.567]]`}, {jpy, `[[5000]]`}} {
			got, err := json.Marshal(c.r.Rows)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("rows: got %s, want %s", got, c.want)
			}
		}
	})
	t.Run("csv", func(t *testing.T) {
		var b bytes.Buffer
		if err := WriteCSV(&b, bhd); err != nil {
			t.Fatal(err)
		}
		if got, want := b.String(), "revenue\n1234.567\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("dashboard", func(t *testing.T) {
		spec := &Spec{Title: "Revenue",
			Tiles: []Tile{
				{Label: "BHD revenue", QueryID: bhd.ID, Measure: "revenue"},
				{Label: "JPY revenue", QueryID: jpy.ID, Measure: "revenue"}},
			Tables: []Table{{Title: "BHD", QueryID: bhd.ID}, {Title: "JPY", QueryID: jpy.ID}}}
		results := map[string]*Result{bhd.ID: bhd, jpy.ID: jpy}
		if problems := spec.Validate(results); len(problems) > 0 {
			t.Fatalf("spec: %v", problems)
		}
		var b bytes.Buffer
		if err := Render(&b, spec, results, Principal{ID: "u"}); err != nil {
			t.Fatal(err)
		}
		page := b.String()
		for _, want := range []string{
			`<span class="v">BHD 1,234.567</span>`, `<span class="v">JPY 5,000</span>`,
			`<td class="n ">1,234.567</td>`, `<td class="n ">5,000</td>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("page: got no %s", want)
			}
		}
	})
}

// byCompany is revenue by company and year in two currencies: a column
// whose rows may not be added together.
func byCompany() *Result {
	return &Result{ID: "q", Columns: []Column{
		{Name: "company", Kind: "dimension"}, {Name: "fiscal_year", Kind: "dimension"},
		{Name: "revenue", Label: "Revenue", Kind: "measure", Type: Money}},
		Rows: []Row{
			{"3000", "2025", amt("JPY", "4000")}, {"3000", "2026", amt("JPY", "5000")},
			{"4000", "2025", amt("BHD", "1.500")}, {"4000", "2026", amt("BHD", "1.234")},
		}}
}

// Amounts in two currencies are never added or subtracted: a tile, a
// tile's change and a chart point over both are refused, and a pivot shows
// no total.
func TestMixedCurrenciesAreNeverAdded(t *testing.T) {
	r := byCompany()
	results := map[string]*Result{"q": r}
	spec := Spec{Title: "t",
		Tiles: []Tile{
			{Label: "all", QueryID: "q", Measure: "revenue"},
			{Label: "vs", QueryID: "q", Measure: "revenue", Where: map[string]string{"company": "4000", "fiscal_year": "2026"},
				CompareWhere: map[string]string{"company": "3000", "fiscal_year": "2026"}},
		},
		Charts: []Chart{{Title: "by year", Type: "bar", QueryID: "q", X: "fiscal_year", Measure: "revenue"}},
	}
	got := spec.Validate(results)
	for i, want := range []string{
		"tile 1 (all): revenue: cannot combine amounts in JPY and amounts in BHD; narrow where to one company",
		"tile 2 (vs) compare: cannot combine amounts in BHD and amounts in JPY",
		`chart 1 (by year): the point "2025": cannot combine amounts in JPY and amounts in BHD; break the chart down by company`,
	} {
		if i >= len(got) || got[i] != want {
			t.Errorf("problem %d: got %q, want %q", i, got, want)
		}
	}
	pivot := tableView(Table{Compare: &Compare{Dim: "fiscal_year", Baseline: "2025", Current: "2026", Measure: "revenue"}}, r)
	if got, want := len(pivot.Rows), 2; got != want {
		t.Errorf("pivot rows: got %d %+v, want %d and no total", got, pivot.Rows, want)
	}
	if got, want := pivot.Rows[1][3].Text, "−0.266"; got != want {
		t.Errorf("BHD change: got %q, want %q", got, want)
	}
}

func TestValueArithmetic(t *testing.T) {
	const most = money.Minor(9_000_000_000_000_000_000)
	big := Value{Units: most, Scale: 2, Currency: "USD"}
	if _, _, err := sumValues([]Value{big, big}); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("sum out of range: got %v, want money.ErrOverflow", err)
	}
	if _, err := diff(big, Value{Units: -most, Scale: 2, Currency: "USD"}); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("change out of range: got %v, want money.ErrOverflow", err)
	}
	_, err := diff(count(1), ratio("0.5"))
	pgtest.CheckErr(t, err, "cannot combine values with 0 decimals and values with 4 decimals")
}

// A query refuses a currency whose digits the facts do not carry, and a
// value out of range, rather than guess or wrap.
func TestQueryValueErrors(t *testing.T) {
	m, err := ParseModel([]byte("dimensions: [{name: company}]\nmetrics:\n" +
		"  - {name: a, type: money, source: t, aggregate: sum, dimensions: [company]}\n" +
		"  - {name: d, type: count, source: t, aggregate: count_distinct, dimensions: [company]}\n" +
		"  - {name: per, type: ratio, derived: {op: divide, left: a, right: d}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := &Facts{Tables: map[string][]Fact{"t": {{ID: "1", Dims: map[string]string{"company": "1"}, Amount: 9_000_000_000_000_000_000, Currency: "USD"}}}}
	_, err = Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"a"}})
	pgtest.CheckErr(t, err, "a: the minor-unit digits of USD are unknown")
	f.Exponents = map[string]int{"USD": 2}
	_, err = Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"per"}})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("a ratio out of range: got %v, want money.ErrOverflow", err)
	}
}

// A difference needs one side: a group with neither has no value.
func TestDifferenceSides(t *testing.T) {
	m, err := ParseModel([]byte("dimensions: [{name: company}]\nmetrics:\n" +
		"  - {name: x, type: money, source: t, aggregate: sum, filter: {company: [\"1\"]}, dimensions: [company]}\n" +
		"  - {name: y, type: money, source: t, aggregate: sum, filter: {company: [\"2\"]}, dimensions: [company]}\n" +
		"  - {name: w, type: count, source: t, aggregate: count_distinct, dimensions: [company]}\n" +
		"  - {name: z, type: money, derived: {op: subtract, left: x, right: y}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	fact := func(company string) Fact {
		return Fact{ID: company, Dims: map[string]string{"company": company}, Amount: 1234, Currency: "BHD"}
	}
	f := &Facts{Exponents: map[string]int{"BHD": 3}, Tables: map[string][]Fact{"t": {fact("1"), fact("2"), fact("3")}}}
	r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"z", "w"}, By: []string{"company"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []any{amt("BHD", "1.234"), amt("BHD", "-1.234"), nil} {
		if got := r.Rows[i][1]; got != want {
			t.Errorf("company %d: got %v, want %v", i+1, got, want)
		}
	}
}

func TestFormatting(t *testing.T) {
	usdCol := Column{Type: Money, Currency: "USD"}
	cases := []struct {
		got, want string
	}{
		{formatValue(ratio("0.256"), Column{Type: Ratio}), "25.6%"},
		{formatValue(count(12345), Column{Type: Count}), "12,345"},
		{formatValue(usd("2500000000.00"), usdCol), "$2.50B"},
		{formatValue(usd("-3250000.00"), usdCol), "−$3.25M"},
		{formatValue(usd("45600.00"), usdCol), "$45.6K"},
		{formatValue(usd("-1234.50"), usdCol), "−$1,234.50"},
		{formatValue(amt("EUR", "10.00"), Column{Type: Money}), "€10.00"},
		{formatValue(amt("GBP", "10.00"), Column{Type: Money}), "£10.00"},
		{formatValue(amt("", "10.00"), Column{Type: Money}), "10.00"},
		{formatValue(amt("CHF", "10.00"), Column{Type: Money}), "CHF 10.00"},
		{formatValue(amt("KWD", "-9999.999"), Column{Type: Money}), "−KWD 9,999.999"},
		{formatValue(amt("JPY", "9999"), Column{Type: Money}), "JPY 9,999"},
		{formatFull(ratio("0.5"), Column{Type: Ratio}), "50.0%"},
		{formatFull(count(1234), Column{Type: Count}), "1,234"},
		{formatFull(usd("-1234567.89"), usdCol), "−1,234,567.89"},
		{formatFull(usd("12.00"), usdCol), "12.00"},
		{formatFull(amt("BHD", "1234567.891"), Column{Type: Money}), "1,234,567.891"},
		{groupThousands("-1234567"), "-1,234,567"},
		{groupThousands("12"), "12"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
	for _, c := range []struct {
		name        string
		base, delta Value
		col         Column
		text, pct   string
		up          bool
	}{
		{"change up", usd("100.00"), usd("10.00"), usdCol, "+$10.00", "+10.0%", true},
		{"change down", usd("100.00"), usd("-10.00"), usdCol, "−$10.00", "−10.0%", false},
		{"ratio change", ratio("0.25"), ratio("0.05"), Column{Type: Ratio}, "+5.0 pts", "", true},
		{"change from zero", usd("0.00"), usd("5.00"), usdCol, "+$5.00", "", true},
		{"a JPY change", amt("JPY", "1000"), amt("JPY", "-250"), Column{Type: Money}, "−JPY 250", "−25.0%", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, p, up := change(c.base, c.delta, c.col)
			if d != c.text {
				t.Errorf("delta: got %s, want %s", d, c.text)
			}
			if p != c.pct {
				t.Errorf("percent: got %q, want %q", p, c.pct)
			}
			if up != c.up {
				t.Errorf("up: got %v, want %v", up, c.up)
			}
		})
	}
	for _, c := range []struct {
		name        string
		base, delta Value
		col         Column
		want        string
	}{
		{"full change down", usd("100000.00"), usd("-10000.00"), usdCol, "−10,000.00"},
		{"full change up", usd("100000.00"), usd("500.00"), usdCol, "+500.00"},
		{"full ratio change", ratio("0.3"), ratio("-0.1"), Column{Type: Ratio}, "-10.0 pts"},
		{"full BHD change", amt("BHD", "1.000"), amt("BHD", "0.001"), Column{Type: Money}, "+0.001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if d, _, _ := changeFull(c.base, c.delta, c.col); d != c.want {
				t.Errorf("got %s, want %s", d, c.want)
			}
		})
	}
}
