package payables

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// TestStatedOf: an e-invoice's stated local tax is kept with its currency;
// a credit memo keeps its reason, a code without words without them and
// words beyond 1,000 characters shortened; a code beyond 100 is not read;
// an invoice keeps no reason.
func TestStatedOf(t *testing.T) {
	lc := money.Minor(18363)
	got, err := statedOf(EInvoice{Kind: KindInvoice, LocalTax: &lc, LocalCurrency: "AED", Reason: &CreditReason{Code: "VD"}})
	if err != nil || got.localTax == nil || *got.localTax != lc || *got.localCurrency != "AED" || got.credit != nil {
		t.Errorf("an invoice: got %+v (%v), want its local tax and no reason", got, err)
	}
	got, _ = statedOf(EInvoice{Kind: KindCreditMemo, Reason: &CreditReason{Code: "VD"}})
	if code, text := got.reason(); code == nil || *code != "VD" || text != nil {
		t.Errorf("a code without words: got %v %v, want VD and no words", code, text)
	}
	got, _ = statedOf(EInvoice{Kind: KindCreditMemo, Reason: &CreditReason{Code: "DL", Text: strings.Repeat("w", 1200)}})
	if _, text := got.reason(); text == nil || len([]rune(*text)) != maxReasonText || !strings.HasSuffix(*text, clippedTextSuffix) {
		t.Errorf("long words: got %d characters, want %d ending in an ellipsis", len([]rune(*text)), maxReasonText)
	}
	if _, err := statedOf(EInvoice{Kind: KindCreditMemo, Reason: &CreditReason{Code: strings.Repeat("V", 101)}}); problem.From(err).Code != "payables.unreadable_document" {
		t.Errorf("a long code: got %v, want payables.unreadable_document", err)
	}
	if code, text := (stated{}).reason(); code != nil || text != nil {
		t.Errorf("no reason: got %v %v, want none", code, text)
	}
}

// TestStoredCitations: what invoiceSelect reads back: a reason's columns,
// and citations with and without their date and match; a date that is not
// one fails the read.
func TestStoredCitations(t *testing.T) {
	if r := (storedReason{}).reason(); r != nil {
		t.Errorf("no reason: got %+v, want nil", r)
	}
	code, text := "VD", "Volume"
	if r := (storedReason{code: &code, text: &text}).reason(); r == nil || r.Code != code || r.Text != text {
		t.Errorf("a reason: got %+v, want VD Volume", r)
	}
	var cs storedCitations
	if err := json.Unmarshal([]byte(`[{"number": "GOI-1", "date": "2026-03-01", "invoice_id": "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d61",
		"document_number": "BYHAND-1"}, {"number": "GOI-2", "date": null, "invoice_id": null, "document_number": null}]`), &cs); err != nil {
		t.Fatal(err)
	}
	got := cs.citations()
	if len(got) != 2 || got[0].Date == nil || got[0].Date.Format("2006-01-02") != "2026-03-01" || got[0].DocumentNumber != "BYHAND-1" ||
		got[1].Date != nil || got[1].InvoiceID != nil {
		t.Errorf("citations: got %+v, want GOI-1 dated and matched, GOI-2 neither", got)
	}
	if (storedCitations{}).citations() != nil {
		t.Errorf("no citations: got some, want nil")
	}
	for name, raw := range map[string]string{"not a date": `[{"number": "G", "date": "March"}]`, "not a string": `[{"number": "G", "date": 1}]`} {
		t.Run(name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(raw), &cs); err == nil {
				t.Errorf("got no error, want the date refused")
			}
		})
	}
}

// TestUnmatchedReason: the hold of a memo citing numbers that match no
// posted invoice names them.
func TestUnmatchedReason(t *testing.T) {
	for _, c := range []struct {
		numbers []string
		want    string
	}{
		{nil, ""},
		{[]string{"GOI-9"}, "names the invoice GOI-9, which is not a posted invoice"},
		{[]string{"GOI-8", "GOI-9"}, "names the invoices GOI-8, GOI-9, which are not posted invoices"},
	} {
		t.Run(strings.Join(c.numbers, ","), func(t *testing.T) {
			if got := unmatchedReason(c.numbers); (c.want == "" && got != "") || !strings.Contains(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// component is a calculated component of code with its account key and
// local tax and recovered part.
func component(key string, taxLC, recoverableLC money.Minor) tax.ComponentTax {
	return tax.ComponentTax{Code: "C", Key: key, AccountKey: key,
		Local: tax.Amounts{Tax: taxLC, Recoverable: recoverableLC, NonRecoverable: taxLC - recoverableLC}}
}

// TestStatedLocalTaxShares: the stated local tax replaces the charged
// components' converted local tax, its difference shared by their local
// tax: to the recovered part of one recovering all, to the part not
// recovered otherwise; a self-assessed component keeps its conversion; no
// charged tax to take a difference, or a share turning a component's
// sign, is refused; out of range is too large; none stated, or stated in
// another currency, leaves the calculation.
func TestStatedLocalTaxShares(t *testing.T) {
	stated := func(m money.Minor, currency string) Invoice {
		return Invoice{StatedLocalTax: &m, StatedLocalCurrency: &currency, StatedLocalExponent: 2}
	}
	items := []tax.ComponentTax{component(tax.KeyInput, 1000, 1000), component(tax.KeyInput, 1000, 500), component(tax.KeySelfInput, 300, 300)}
	got, err := statedLocalTax(stated(2200, "AED"), "AED", items)
	if err != nil {
		t.Fatal(err)
	}
	if s := []string{got[0].Local.Tax.Plain(0), got[0].Local.Recoverable.Plain(0), got[1].Local.Tax.Plain(0), got[1].Local.Recoverable.Plain(0),
		got[1].Local.NonRecoverable.Plain(0), got[2].Local.Tax.Plain(0)}; strings.Join(s, " ") != "1100 1100 1100 500 600 300" {
		t.Errorf("shares: got %v, want 1100 1100 1100 500 600 300", s)
	}
	for name, c := range map[string]struct {
		v     Invoice
		items []tax.ComponentTax
		code  string
	}{
		"nothing charged":   {stated(100, "AED"), []tax.ComponentTax{component(tax.KeyInput, 0, 0)}, "payables.local_tax_mismatch"},
		"a sign turned":     {stated(-10, "AED"), []tax.ComponentTax{component(tax.KeyInput, 1000, 1000)}, "payables.local_tax_mismatch"},
		"out of range":      {stated(money.Minor(1<<62), "AED"), []tax.ComponentTax{component(tax.KeyInput, money.Minor(1<<62), 0), component(tax.KeyInput, money.Minor(1<<62), 0)}, "payables.amount_too_large"},
		"a stated diff out": {stated(money.Minor(-(1 << 62)), "AED"), []tax.ComponentTax{component(tax.KeyInput, money.Minor(1<<62), 0)}, "payables.amount_too_large"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := statedLocalTax(c.v, "AED", c.items); problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
	for name, v := range map[string]Invoice{"none stated": {}, "stated in another currency": stated(1, "EUR"), "as converted": stated(2000, "AED")} {
		t.Run(name, func(t *testing.T) {
			if got, err := statedLocalTax(v, "AED", items); err != nil || got[0].Local.Tax != 1000 {
				t.Errorf("got %v (%v), want the calculation unchanged", got, err)
			}
		})
	}
}
