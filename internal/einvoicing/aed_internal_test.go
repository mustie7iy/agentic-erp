package einvoicing

import (
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// TestCitedRoundTrip (ADR-0061 As built batch 88): the invoices a credit
// note cites are rendered and read back each with its number and date
// (IBT-025, IBT-026), a repeat cited once with the date it states, a
// blank left out; a date that is not one is a malformed IBT-026.
func TestCitedRoundTrip(t *testing.T) {
	d := creditNote(t)
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	d.Cited = []Cited{{Number: "CI099999"}, {Number: " "}, {Number: "CI099999", Date: &day}, {Number: "CI099998", Date: &day}}
	got, refusal := Received(RenderPINTAE(d), currency.SeededExponent)
	if refusal != nil {
		t.Fatalf("got %v, want it accepted", *refusal)
	}
	var cited []string
	for _, c := range got.Cited {
		cited = append(cited, c.Number+" "+c.Date.Format(time.DateOnly))
	}
	if strings.Join(cited, ", ") != "CI099999 2026-03-01, CI099998 2026-03-01" || got.Reference != "CI099999" {
		t.Errorf("cited: got %v (reference %s), want CI099999 and CI099998 dated 2026-03-01", cited, got.Reference)
	}
	bad := strings.Replace(string(RenderPINTAE(d)), "<cbc:IssueDate>2026-03-01</cbc:IssueDate>", "<cbc:IssueDate>1 March</cbc:IssueDate>", 1)
	if _, refusal := Received([]byte(bad), currency.SeededExponent); refusal == nil || refusal.Code != RefusedMissingField ||
		!strings.Contains(refusal.Detail, "IBT-026") {
		t.Errorf("a malformed IBT-026: got %v, want %s naming IBT-026", refusal, RefusedMissingField)
	}
}

// TestLinesInAED: each line's VAT and amount in AED (BTAE-08, BTAE-10)
// are read in AED's digits; one stated in another currency is a malformed
// field; the lines' VAT in AED, when every line but an exempt one states
// it, must sum to the document's VAT in AED (IBT-111, or IBT-110 in AED),
// else the document is inconsistent.
func TestLinesInAED(t *testing.T) {
	d := valid(t)
	got, refusal := Received(RenderPINTAE(d), currency.SeededExponent)
	if refusal != nil {
		t.Fatalf("got %v, want it accepted", *refusal)
	}
	if w := words(int64(*got.Lines[0].AccountingTax), int64(*got.Lines[0].AccountingAmount)); w != "36725 771225" {
		t.Errorf("line 1 in AED: got %s, want 36725 771225", w)
	}
	in := string(RenderPINTAE(d))
	if _, refusal := Received([]byte(strings.Replace(in, `<cac:TaxTotal><cbc:TaxAmount currencyID="AED">367.25</cbc:TaxAmount></cac:TaxTotal><cac:Item>`,
		`<cac:TaxTotal><cbc:TaxAmount currencyID="USD">367.25</cbc:TaxAmount></cac:TaxTotal><cac:Item>`, 1)), currency.SeededExponent); refusal == nil ||
		!strings.Contains(refusal.Detail, "BTAE-08") {
		t.Errorf("BTAE-08 in USD: got %v, want a malformed BTAE-08", refusal)
	}
	for name, c := range map[string]struct {
		change func(*Document)
		want   string
	}{
		"not summing to IBT-111": {func(d *Document) { *d.Lines[1].AccountingTax = 1 }, "sums to 367.26 AED, not the 367.25 AED stated (IBT-111)"},
		"in AED, not summing to IBT-110": {func(d *Document) {
			d.Currency, d.TaxCurrency, d.AccountingTax, d.AccountingCurrency = "AED", "", nil, ""
		}, "sums to 367.25 AED, not the 100.00 AED stated (IBT-110)"},
		"out of range": {func(d *Document) {
			big := money.Minor(1 << 62)
			d.Lines[0].AccountingTax, d.Lines[1].AccountingTax = &big, &big
		}, "out of range"},
		"a line without it":     {func(d *Document) { d.Lines[1].AccountingTax = nil }, ""},
		"an exempt one without": {func(d *Document) { d.Lines[1].AccountingTax, d.Lines[1].Category = nil, "E" }, ""},
		"without IBT-111":       {func(d *Document) { d.AccountingTax = nil }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			x := valid(t)
			lc := *x.Lines[1].AccountingTax
			x.Lines[1].AccountingTax = &lc
			c.change(&x)
			got := strings.Join(inconsistentInAED(x), "; ")
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
