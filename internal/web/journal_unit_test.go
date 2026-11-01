package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// TestDebits: an entry's debits are its positive amounts summed in their
// one currency; none for no lines, and nothing for amounts that do not
// sum (another currency, other digits, no decimal, beyond the minor
// unit's range).
func TestDebits(t *testing.T) {
	eur := func(a string) gen.Money { return gen.Money{Amount: a, Currency: "EUR"} }
	for name, tc := range map[string]struct {
		amounts []gen.Money
		want    string
	}{
		"summed":           {[]gen.Money{eur("10.50"), eur("-20.00"), eur("9.50")}, "20.00"},
		"none":             {nil, ""},
		"two currencies":   {[]gen.Money{eur("1.00"), {Amount: "1.00", Currency: "USD"}}, ""},
		"other digits":     {[]gen.Money{eur("1.00"), eur("1.0")}, ""},
		"no decimal":       {[]gen.Money{eur("x")}, ""},
		"beyond the range": {[]gen.Money{eur("9000000000000000000"), eur("9000000000000000000")}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := ""
			if m := debits(tc.amounts); m != nil {
				got = m.Amount
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCSVLimits: a file of more lines than an entry holds is refused, and
// a file's refused fields beyond the rows listed are counted.
func TestCSVLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString("account,amount\n")
	for range maxFileLines + 1 {
		b.WriteString("1,1\n")
	}
	if _, err := csvLines([]byte(b.String())); err != i18n.CSVTooManyRows {
		t.Errorf("too many rows: got %q", err)
	}
	lines := make([]entryLine, maxRowErrors+2)
	for i := range lines {
		lines[i].no, lines[i].refused = i+2, []refusedField{{api: "cost_center_id", reason: "not_allowed"}}
	}
	rows, more := rowErrors(lines)
	if len(rows) != maxRowErrors || more != 2 || rows[0].Column != "cost_center" || rows[0].Error != i18n.EntryFieldRefused {
		t.Errorf("refused rows: got %d listed, %d more, %+v", len(rows), more, rows[0])
	}
	marked := make([]entryLine, 2)
	markRefused(gen.Problem{Errors: []gen.FieldError{{Field: "lines[1].amount.amount", Code: "precision"}, {Field: "lines[7]", Code: "x"},
		{Field: "lines[x]", Code: "x"}, {Field: "currency", Code: "x"}, {Field: "lines[0]", Code: "unbalanced"}}}, marked)
	if len(marked[1].refused) != 1 || marked[1].refused[0].api != "amount" || len(marked[0].refused) != 1 || marked[0].refused[0].api != "" {
		t.Errorf("markRefused: got %+v", marked)
	}
	if ui.LineFieldLabel(marked[1].refused[0].api) != i18n.EntryLineAmount {
		t.Error("markRefused: the amount's field is not named")
	}
	unit := make([]entryLine, 1)
	markRefused(gen.Problem{Errors: []gen.FieldError{{Field: "lines[0].quantity.unit", Code: "unknown"}}}, unit)
	if got := unit[0].refused; len(got) != 1 || ui.LineFieldLabel(got[0].api) != i18n.EntryLineUnit || apiColumns[got[0].api] != "unit" {
		t.Errorf("a quantity's unit refused: got %+v, want it named the unit's, on the form and in a file", got)
	}
}

// TestKeepChosen: a ledgers' choice the form holds stays chosen when the
// options do not hold it, never silently every ledger in its place.
func TestKeepChosen(t *testing.T) {
	offered := []ui.Option{{Selected: false}, {Value: "ledger:a", Text: "0L Leading"}}
	for name, tc := range map[string]struct {
		options []ui.Option
		chosen  string
		want    []ui.Option
	}{
		"none chosen":   {offered, "", offered},
		"chosen there":  {[]ui.Option{{}, {Value: "ledger:a", Selected: true}}, "ledger:a", []ui.Option{{}, {Value: "ledger:a", Selected: true}}},
		"not offered":   {offered, "group:ALL", append(append([]ui.Option{}, offered...), ui.Option{Value: "group:ALL", Text: "ALL", Selected: true})},
		"none to offer": {nil, "ledger:b", []ui.Option{{}, {Value: "ledger:b", Text: "b", Selected: true}}},
		"no options":    {nil, "", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := keepChosen(tc.options, tc.chosen); !slices.Equal(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLookupsBounded: a send looks at most maxCodeLookups codes up one by
// one; the codes past it are refused before any call, a code a book
// holds or that is an id needs none, and a book that holds all its
// kind's codes says the others are not known.
func TestLookupsBounded(t *testing.T) {
	calls := 0
	find := func(string) (uuid.UUID, bool, i18n.Key) { calls++; return uuid.Nil, false, "" }
	lk := &lookups{left: 2}
	known := uuid.New()
	got := lk.lookUp([]string{"A", "B", "A", "C", "K", known.String(), ""}, book{ids: map[string]uuid.UUID{"K": known}, more: true},
		i18n.EntryPartnerUnknown, find)
	if calls != 2 {
		t.Errorf("calls: got %d, want 2", calls)
	}
	for code, want := range map[string]found{"A": {err: i18n.EntryPartnerUnknown}, "B": {err: i18n.EntryPartnerUnknown},
		"C": {err: i18n.EntryTooManyCodes}, "K": {id: known}, known.String(): {id: known}} {
		if got[code] != want {
			t.Errorf("code %s: got %+v, want %+v", code, got[code], want)
		}
	}
	if got := lk.lookUp([]string{"Z"}, book{}, i18n.EntryPartnerUnknown, find); got["Z"].err != i18n.EntryPartnerUnknown || calls != 2 {
		t.Errorf("a whole book: %d calls, got %+v", calls, got)
	}
}

// TestTotals: the running totals sum the amounts typed exactly, in the
// locale, with as many decimals as the most an amount has; an amount
// that cannot be read, or whose sum is out of range, is not counted, and
// a currency that is none is not shown.
func TestTotals(t *testing.T) {
	catalog, err := i18n.Load(false)
	if err != nil {
		t.Fatal(err)
	}
	en, fr := catalog.Match("en"), catalog.Match("fr-CA")
	lines := func(amounts ...string) []entryLine {
		out := make([]entryLine, len(amounts))
		for i, a := range amounts {
			out[i] = entryLine{amount: a}
		}
		return out
	}
	for name, tc := range map[string]struct {
		loc      i18n.Locale
		amounts  []string
		currency string
		want     ui.EntryTotals
	}{
		"balanced":      {en, []string{"1200.50", "-1200.5", ""}, "EUR", ui.EntryTotals{Debits: "1200.50", Credits: "1200.50", Difference: "0.00", Currency: "EUR"}},
		"in French":     {fr, []string{"12,5", "-2,25"}, "CAD", ui.EntryTotals{Debits: "12.50", Credits: "2.25", Difference: "10.25", Currency: "CAD"}},
		"whole amounts": {en, []string{"100", "-40"}, "JPY", ui.EntryTotals{Debits: "100", Credits: "40", Difference: "60", Currency: "JPY"}},
		"four decimals": {en, []string{"0.0001", "-1"}, "BHD", ui.EntryTotals{Debits: "0.0001", Credits: "1.0000", Difference: "-0.9999", Currency: "BHD"}},
		"none":          {en, nil, "", ui.EntryTotals{Debits: "0", Credits: "0", Difference: "0"}},
		"not read":      {en, []string{"1,5", "x", "5"}, "eur", ui.EntryTotals{Debits: "5", Credits: "0", Difference: "5", Uncounted: 2}},
		"out of range": {en, []string{"900000000000000", "900000000000000", "999999999999999999"}, "EUR",
			ui.EntryTotals{Debits: "900000000000000", Credits: "0", Difference: "900000000000000", Currency: "EUR", Uncounted: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := totalsOf(tc.loc, lines(tc.amounts...), tc.currency); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
