package web

import (
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
	if calls != 2 || got["A"].err != i18n.EntryPartnerUnknown || got["C"].err != i18n.EntryTooManyCodes || got["K"].id != known ||
		got[known.String()].id != known {
		t.Errorf("bounded: %d calls, got %+v", calls, got)
	}
	if got := lk.lookUp([]string{"Z"}, book{}, i18n.EntryPartnerUnknown, find); got["Z"].err != i18n.EntryPartnerUnknown || calls != 2 {
		t.Errorf("a whole book: %d calls, got %+v", calls, got)
	}
}
