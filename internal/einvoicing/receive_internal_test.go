package einvoicing

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// creditNote is sale's credit memo in AED, crediting CI099999, as a
// rendered tax credit note.
func creditNote(t *testing.T) Document {
	t.Helper()
	credit := sale()
	credit.Kind, credit.Currency, credit.ReferenceNumber = saleCreditMemo, "AED", ptr("CI099999")
	d, _ := buildDocument(credit, seller(), buyer(), &Reason{Code: "CR-01", Text: "Price reduced"})
	return d
}

// TestReceivedRoundTrip: what RenderPINTAE writes, the receiving side's
// validation reads and accepts (ADR-0061 §5, As built E2 and T65c): a tax
// invoice in USD with its VAT in AED and a tax credit note in AED, each
// read back with its number, kind, date, currency, gross, specification,
// parties and lines.
func TestReceivedRoundTrip(t *testing.T) {
	for name, d := range map[string]Document{"a tax invoice in USD": valid(t), "a tax credit note in AED": creditNote(t)} {
		t.Run(name, func(t *testing.T) {
			got, refusal := Received(RenderPINTAE(d), currency.SeededExponent)
			if refusal != nil {
				t.Fatalf("got %v, want it accepted", *refusal)
			}
			if a, b := words(got.Number, got.Kind, got.IssueDate.Format("2006-01-02"), got.Currency, int64(got.TaxInclusive), got.Customization),
				words(d.Number, d.Kind, d.IssueDate.Format("2006-01-02"), d.Currency, int64(d.TaxInclusive), d.Customization); a != b {
				t.Errorf("read back: got %s, want %s", a, b)
			}
			if a, b := words(got.Buyer.EndpointScheme, got.Buyer.Endpoint, got.Buyer.TRN, len(got.Lines)), words(d.Buyer.EndpointScheme,
				d.Buyer.Endpoint, d.Buyer.TRN, len(d.Lines)); a != b {
				t.Errorf("its buyer and lines: got %s, want %s", a, b)
			}
		})
	}
}

// TestReceivedRefusals: each ground of ADR-0061 §5 is refused with its
// code, and its detail names what was found.
func TestReceivedRefusals(t *testing.T) {
	invoice, credit := string(RenderPINTAE(valid(t))), string(RenderPINTAE(creditNote(t)))
	cases := []struct {
		name, content, code, detail string
	}{
		{"malformed XML", invoice[:len(invoice)-10], RefusedUnreadable, "not well-formed XML"},
		{"a DTD", strings.Replace(invoice, "<Invoice", `<!DOCTYPE Invoice [<!ENTITY x SYSTEM "file:///etc/passwd">]><Invoice`, 1), RefusedUnreadable,
			"declares a DTD"},
		{"an entity never declared", strings.Replace(invoice, "<cbc:ID>CI100001", "<cbc:ID>&x;", 1), RefusedUnreadable, "entity"},
		{"another document", `<Order xmlns="urn:oasis:names:specification:ubl:schema:xsd:Order-2"/>`, RefusedUnsupported, `"Order"`},
		{"an Invoice of no namespace", `<Invoice/>`, RefusedUnsupported, "not a UBL 2.1 Invoice"},
		{"a self-billed invoice", strings.Replace(invoice, ">380<", ">389<", 1), RefusedUnsupported, `"389"`},
		{"a credit note's type on an invoice", strings.Replace(invoice, ">380<", ">381<", 1), RefusedUnsupported, `"381"`},
		{"another specification", strings.Replace(invoice, PINTAECustomization, "urn:cen.eu:en16931:2017", 1), RefusedUnsupported,
			"urn:cen.eu:en16931:2017"},
		{"no number", strings.Replace(invoice, "<cbc:ID>CI100001</cbc:ID>", "", 1), RefusedMissingField, "IBT-001"},
		{"a number too long", strings.Replace(invoice, "<cbc:ID>CI100001", "<cbc:ID>"+strings.Repeat("9", 201), 1), RefusedMissingField, "IBT-001"},
		{"no buyer's TRN", strings.Replace(invoice, "<cbc:CompanyID>100987654300003", "<cbc:CompanyID>1009876543", 1), RefusedMissingField, "IBT-048"},
		{"no buyer's address", strings.Replace(invoice, `<cbc:EndpointID schemeID="0235">1009876543</cbc:EndpointID>`, "", 1), RefusedMissingField,
			"IBT-049"},
		{"a line without description", strings.Replace(invoice, "<cbc:Description>Chairs</cbc:Description>", "", 1), RefusedMissingField,
			"line 1 IBT-154"},
		{"a credit note line without quantity", strings.Replace(credit, `<cbc:CreditedQuantity unitCode="C62">1</cbc:CreditedQuantity>`, "", 1),
			RefusedMissingField, "line 1 IBT-129"},
		{"a rate not a number", strings.Replace(invoice, "<cbc:Percent>5</cbc:Percent>", "<cbc:Percent>five</cbc:Percent>", 1), RefusedMissingField,
			"IBT-119"},
		{"no VAT in AED", strings.Replace(invoice, `<cbc:TaxCurrencyCode>AED</cbc:TaxCurrencyCode>`, "", 1), RefusedMissingField, "IBT-006"},
		{"no totals", cut(invoice, "<cac:LegalMonetaryTotal>", "</cac:LegalMonetaryTotal>"), RefusedMissingField, "IBT-106 (the sum of the lines' net amounts), IBT-109"},
		{"no VAT total", cut(invoice, `<cac:TaxTotal><cbc:TaxAmount currencyID="USD">`, "</cac:TaxTotal>"), RefusedMissingField, "IBT-110"},
		{"no line", cut(invoice, "<cac:InvoiceLine>", "</Invoice>"), RefusedMissingField, "IBG-25"},
		{"an unknown currency", strings.ReplaceAll(invoice, "USD", "XXX"), RefusedMissingField, "IBT-005 (unknown currency"},
		{"an issue date not a date", strings.Replace(invoice, "<cbc:IssueDate>2026-03-10", "<cbc:IssueDate>10.03.2026", 1), RefusedMissingField,
			"IBT-002"},
		{"an amount in another currency", strings.Replace(invoice, `<cbc:PayableAmount currencyID="USD">`, `<cbc:PayableAmount currencyID="EUR">`, 1),
			RefusedMissingField, "IBT-115"},
		{"three decimals", strings.Replace(invoice, `<cbc:PayableAmount currencyID="USD">3100.00`, `<cbc:PayableAmount currencyID="USD">3100.000`, 1),
			RefusedPrecision, "IBT-115 has more decimals than USD's"},
		{"VAT in AED of three decimals", strings.Replace(invoice, `<cbc:TaxAmount currencyID="AED">367.25`, `<cbc:TaxAmount currencyID="AED">367.250`, 1),
			RefusedPrecision, "IBT-111 has more decimals than AED's"},
		{"lines not their total", strings.Replace(invoice, `<cbc:LineExtensionAmount currencyID="USD">3000.00`,
			`<cbc:LineExtensionAmount currencyID="USD">3000.01`, 1), RefusedInconsistent, "The lines' net amounts sum to 3,000.00 USD, not the 3,000.01 USD"},
		{"net and VAT not the gross", strings.Replace(invoice, `<cbc:TaxInclusiveAmount currencyID="USD">3100.00`,
			`<cbc:TaxInclusiveAmount currencyID="USD">3100.50`, 1), RefusedInconsistent, "sum to 3,100.00 USD, not the 3,100.50 USD stated (IBT-112)"},
		{"subtotals not the VAT", strings.Replace(invoice, `<cbc:TaxAmount currencyID="USD">100.00</cbc:TaxAmount><cac:TaxCategory>`,
			`<cbc:TaxAmount currencyID="USD">99.99</cbc:TaxAmount><cac:TaxCategory>`, 1), RefusedInconsistent,
			"The VAT subtotals sum to 99.99 USD, not the 100.00 USD stated (IBT-110)"},
		{"an amount not a decimal", strings.Replace(invoice, `<cbc:PayableAmount currencyID="USD">3100.00`, `<cbc:PayableAmount currencyID="USD">31.00.00`, 1),
			RefusedMissingField, "IBT-115"},
		{"an amount with a trailing sign", strings.Replace(invoice, `<cbc:PayableAmount currencyID="USD">3100.00`,
			`<cbc:PayableAmount currencyID="USD">3100.00-`, 1), RefusedMissingField, "IBT-115"},
		{"an amount out of range", strings.Replace(invoice, `<cbc:LineExtensionAmount currencyID="USD">2000.00`,
			`<cbc:LineExtensionAmount currencyID="USD">92233720368547758.07`, 1), RefusedInconsistent, "An amount is out of range"},
		{"many fields missing", many(t), RefusedMissingField, "and 5 more."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.content == invoice || c.content == credit {
				t.Fatal("the case changed nothing")
			}
			_, refusal := Received([]byte(c.content), currency.SeededExponent)
			if refusal == nil {
				t.Fatalf("got it accepted, want %s", c.code)
			}
			if refusal.Code != c.code || !strings.Contains(refusal.Detail, c.detail) {
				t.Errorf("got %s %q, want %s naming %q", refusal.Code, refusal.Detail, c.code, c.detail)
			}
		})
	}
}

// cut removes content from the first from to the next to, to included,
// keeping the end tag when to is one of the document's root.
func cut(s, from, to string) string {
	i := strings.Index(s, from)
	j := strings.Index(s[i:], to)
	if strings.HasPrefix(to, "</Invoice") {
		return s[:i] + s[i+j:]
	}
	return s[:i] + s[i+j+len(to):]
}

// many is a tax invoice of 25 lines, none with its description.
func many(t *testing.T) string {
	t.Helper()
	d := valid(t)
	line := d.Lines[1]
	line.Description = ""
	d.Lines = nil
	for range 25 {
		d.Lines = append(d.Lines, line)
	}
	return string(RenderPINTAE(d))
}

// FuzzReceived: whatever the bytes, reading a received document never
// panics, refuses it with one of §5's codes or accepts it with its number
// read, and a detail never exceeds what the row keeps (2,000 characters).
func FuzzReceived(f *testing.F) {
	d, _ := buildDocument(sale(), seller(), buyer(), nil)
	f.Add(RenderPINTAE(d))
	f.Add([]byte(`<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"><ID>1</ID></Invoice>`))
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY a "b">]><x>&a;</x>`))
	codes := map[string]bool{RefusedUnreadable: true, RefusedUnsupported: true, RefusedMissingField: true, RefusedPrecision: true,
		RefusedInconsistent: true}
	f.Fuzz(func(t *testing.T, content []byte) {
		d, refusal := Received(content, currency.SeededExponent)
		switch {
		case refusal == nil && (d.Number == "" || len(d.Number) > 200):
			t.Errorf("accepted with the number %q, want one of 1 to 200 characters", d.Number)
		case refusal != nil && !codes[refusal.Code]:
			t.Errorf("refused with %s, want one of §5's codes", refusal.Code)
		case refusal != nil && len([]rune(refusal.Detail)) > 2000:
			t.Errorf("a detail of %d characters, want at most 2000", len([]rune(refusal.Detail)))
		}
	})
}
