package einvoicing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// sale is a posted USD invoice of a UAE company: a standard-rated and a
// zero-rated item.
func sale() Sale {
	due := time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)
	return Sale{ID: uuid.New(), Number: "CI100001", Kind: saleInvoice, Status: salePosted, IssueDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
		DueDate: &due, Currency: "USD", Exponent: 2, Net: 300000, Tax: 10000, Gross: 310000, LocalTax: 36725, LocalCurrency: "AED", Location: "AE",
		Lines: []SaleLine{{No: 1, Description: "Chairs", Net: 200000, Tax: 10000, TaxCode: "S5", Category: "S", Percent: "5.000000"},
			{No: 2, Description: "Export freight", Net: 100000, TaxCode: "Z0", Category: "Z", Percent: "0.000000"}}}
}

func seller() Seller {
	return Seller{Company: Company{Code: "5000", Name: "Maple Gulf Trading"}, TRN: "100234567800003",
		Address: &Address{Line1: "Gulf Tower", City: "Dubai", Subentity: "AE-DU", Country: "AE"}}
}

func buyer() Buyer {
	return Buyer{Number: "BP1", Name: "Desert Rose", TRN: "100987654300003", Address: &Address{Line1: "Al Wasl Road", City: "Dubai", Subentity: "dxb", Country: "AE"}}
}

// valid is sale's document, which passes every rule; its only finding is
// the terms not rendered yet of a document not in AED.
func valid(t *testing.T) Document {
	t.Helper()
	d, found := buildDocument(sale(), seller(), buyer(), nil)
	if f := Check(d); len(f) > 0 {
		t.Fatalf("Check of the valid document: got %v, want none", f)
	}
	if got := fmt.Sprint(found); !strings.Contains(got, "BTAE-08") || len(found) != 1 {
		t.Fatalf("its findings: got %s, want BTAE-08 alone", got)
	}
	if d.AccountingTax == nil {
		t.Fatal("its VAT in AED: got none, want 36725")
	}
	if got := words(d.TaxCurrency, int64(*d.AccountingTax)); got != "AED 36725" {
		t.Fatalf("its VAT in AED: got %s, want AED 36725", got)
	}
	return d
}

func words(v ...any) string { return strings.TrimSpace(fmt.Sprintln(v...)) }

// TestRoundTrip: what RenderPINTAE writes, ParsePINTAE reads back as the
// same document, and the rules accept it (ADR-0061 As built E2), an
// invoice and a credit note, in AED and in another currency.
func TestRoundTrip(t *testing.T) {
	credit := sale()
	credit.Kind, credit.Currency, credit.ReferenceNumber = saleCreditMemo, "AED", ptr("CI099999")
	for name, d := range map[string]Document{
		"a tax invoice in USD": valid(t),
		"a tax credit note in AED": func() Document {
			d, _ := buildDocument(credit, seller(), buyer(), &Reason{Code: "CR-01", Text: "Price reduced & <corrected>"})
			return d
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			back, err := ParsePINTAE(RenderPINTAE(d), currency.SeededExponent)
			if err != nil {
				t.Fatalf("ParsePINTAE: %v", err)
			}
			if f := Check(back); len(f) > 0 {
				t.Errorf("Check of the parsed document: got %v, want none", f)
			}
			if got, want := RenderPINTAE(back), RenderPINTAE(d); string(got) != string(want) {
				t.Errorf("rendered again after parsing: got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestRules: each mandatory field missing or malformed is named by its
// rule and business term.
func TestRules(t *testing.T) {
	cases := []struct {
		name, rule string
		change     func(d *Document)
	}{
		{"another specification", "IBT-024", func(d *Document) { d.Customization = "urn:other" }},
		{"another profile", "IBT-023", func(d *Document) { d.Profile = "" }},
		{"another kind", "IBT-003", func(d *Document) { d.Kind = "debit_note" }},
		{"a credit note's type code on an invoice", "IBT-003", func(d *Document) { d.TypeCode = TypeTaxCreditNote }},
		{"no number", "IBT-001", func(d *Document) { d.Number = "" }},
		{"seven transaction flags", "BTAE-02", func(d *Document) { d.TransactionType = "0000000" }},
		{"no VAT in AED", "IBT-006", func(d *Document) { d.AccountingTax = nil }},
		{"an AED document with a tax currency", "IBT-006", func(d *Document) { d.Currency = "AED" }},
		{"a credit note naming no invoice", "IBT-025", func(d *Document) {
			d.Kind, d.TypeCode, d.Reason = KindTaxCreditNote, TypeTaxCreditNote, &Reason{Code: "C", Text: "T"}
		}},
		{"a credit note without its reason", "BTAE-03", func(d *Document) { d.Kind, d.TypeCode, d.Reference = KindTaxCreditNote, TypeTaxCreditNote, "CI1" }},
		{"an invoice with a reason", "BTAE-03", func(d *Document) { d.Reason = &Reason{Code: "C", Text: "T"} }},
		{"a seller without TRN", "IBG-04", func(d *Document) { d.Seller.TRN = "" }},
		{"a seller's endpoint not its TRN's", "IBG-04", func(d *Document) { d.Seller.Endpoint = "1111111111" }},
		{"a buyer's endpoint of another scheme", "IBG-07", func(d *Document) { d.Buyer.EndpointScheme = "0088" }},
		{"a buyer without name, line 1 or city", "IBG-07", func(d *Document) { d.Buyer.Name, d.Buyer.Address.Line1, d.Buyer.Address.City = "", "", "" }},
		{"a buyer abroad", "IBG-07", func(d *Document) { d.Buyer.Address.Country = "" }},
		{"a seller's emirate unknown", "IBG-04", func(d *Document) { d.Seller.Address.Subentity = "DUBAI" }},
		{"no payment means", "IBT-081", func(d *Document) { d.PaymentMeans = "" }},
		{"no line", "IBG-25", func(d *Document) { d.Lines = nil }},
		{"a line without quantity", "IBG-25", func(d *Document) { d.Lines[0].Quantity = "0" }},
		{"a line without name", "IBG-25", func(d *Document) { d.Lines[0].Name = "" }},
		{"a line of an unknown category", "IBG-25", func(d *Document) { d.Lines[0].Category = "K" }},
		{"a standard line at zero", "IBG-25", func(d *Document) { d.Lines[0].Percent = "0" }},
		{"a zero-rated line at a rate", "IBG-25", func(d *Document) { d.Lines[1].Percent = "5" }},
		{"a line below zero", "IBG-25", func(d *Document) { d.Lines[1].Price = -1 }},
		{"lines not the net", "IBG-22", func(d *Document) { d.LineTotal++ }},
		{"net and VAT not the gross", "IBG-22", func(d *Document) { d.Payable++ }},
		{"a subtotal not its lines'", "IBG-23", func(d *Document) { d.Subtotals[0].Taxable++ }},
		{"a category without subtotal", "IBG-23", func(d *Document) { d.Subtotals = d.Subtotals[:1] }},
		{"subtotals not the VAT", "IBG-23", func(d *Document) { d.Subtotals[0].Tax++ }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := valid(t)
			d.Lines = append([]Line(nil), d.Lines...)
			d.Subtotals = append([]Subtotal(nil), d.Subtotals...)
			c.change(&d)
			var terms []string
			for _, f := range Check(d) {
				terms = append(terms, f.Term)
			}
			if len(terms) == 0 || !strings.Contains(strings.Join(terms, " "), c.rule) {
				t.Errorf("got %v, want %s among them", terms, c.rule)
			}
		})
	}
}

// TestBuildFindings: a seller whose TRN is its tax group's and a blocked
// buyer are named; a party without an address or TRN renders empty
// fields for the rules to name.
func TestBuildFindings(t *testing.T) {
	member := seller()
	member.Member = true
	blocked := Buyer{Name: "*****", Blocked: true}
	d, found := buildDocument(sale(), member, blocked, nil)
	var got []string
	for _, f := range found {
		got = append(got, f.Term)
	}
	if strings.Join(got, " ") != "IBT-034 IBG-07 BTAE-08" {
		t.Errorf("findings: got %v, want IBT-034 IBG-07 BTAE-08", got)
	}
	exempt := sale()
	exempt.Currency = "AED"
	exempt.Lines = append(exempt.Lines, SaleLine{No: 3, Description: "Bare land", Net: 1, TaxCode: "E0", Category: "E", Percent: "0"})
	_, found = buildDocument(exempt, seller(), buyer(), nil)
	if got := words(len(found), found[0].Term, strings.HasPrefix(found[0].Message, "line 3:")); got != "1 IBT-120 true" {
		t.Errorf("an exempt line: got %s, want 1 IBT-120 true", got)
	}
	if got := words(d.Seller.Endpoint == "", d.Buyer.Endpoint == "", d.Buyer.Address.Country == ""); got != "true true true" {
		t.Errorf("no seller endpoint, no buyer endpoint, no buyer address: got %s, want true true true", got)
	}
	if got := plainPercent("12"); got != "12" {
		t.Errorf("plainPercent(12): got %q, want 12", got)
	}
}

// TestParseRefusals: a document with a DTD, malformed XML, an empty one,
// unknown currencies, an amount of another currency or digits and dates
// that are not dates are not read.
func TestParseRefusals(t *testing.T) {
	good := string(RenderPINTAE(valid(t)))
	cases := map[string]string{
		"a DTD":                       strings.Replace(good, "<Invoice", `<!DOCTYPE Invoice [<!ENTITY x "y">]><Invoice`, 1),
		"malformed XML":               good[:len(good)-5],
		"nothing":                     "",
		"an unknown currency":         strings.ReplaceAll(good, "USD", "XXX"),
		"an amount in another":        strings.Replace(good, `<cbc:LineExtensionAmount currencyID="USD">`, `<cbc:LineExtensionAmount currencyID="EUR">`, 1),
		"an amount of three decimals": strings.Replace(good, `>3000.00<`, `>3000.000<`, 1),
		"an issue date":               strings.Replace(good, "<cbc:IssueDate>2026-03-10", "<cbc:IssueDate>10.03.2026", 1),
		"a due date":                  strings.Replace(good, "<cbc:DueDate>2026-04-09", "<cbc:DueDate>soon", 1),
		"an unknown tax currency":     strings.Replace(good, `currencyID="AED"`, `currencyID="ZZZ"`, 1),
		"a tax amount in AED of one digit": strings.Replace(good, `<cbc:TaxAmount currencyID="AED">367.25`,
			`<cbc:TaxAmount currencyID="AED">367.2`, 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if content == good {
				t.Fatal("the case changed nothing")
			}
			if _, err := ParsePINTAE([]byte(content), currency.SeededExponent); err == nil {
				t.Error("got nil error, want a refusal")
			}
		})
	}
	if _, err := ParsePINTAE([]byte(strings.Replace(good, "<cbc:DueDate>2026-04-09</cbc:DueDate>", "", 1)), currency.SeededExponent); err != nil {
		t.Errorf("without a due date: got %v, want none", err)
	}
	if !errors.Is(noDirectives([]byte(cases["a DTD"])), errDTD) {
		t.Error("a DTD: got another error, want errDTD")
	}
}

// failingStore refuses every write.
type failingStore struct{ blobstore.Dir }

func (failingStore) Put(context.Context, string, []byte, blobstore.PutOptions) error {
	return errors.New("store down")
}

// TestKeepRefusesADefect: a render whose XML does not parse back, or
// fails the rules once parsed back, is a defect and is never kept; a
// store that does not take the XML is the machinery's failure (503).
func TestKeepRefusesADefect(t *testing.T) {
	unknown := valid(t)
	unknown.Currency = "XXX"
	failing := valid(t)
	failing.Number = ""
	for name, d := range map[string]Document{"does not parse back": unknown, "fails the rules": failing} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{Objects: blobstore.Dir{Root: t.TempDir()}}
			o := OutgoingEInvoice{CustomerInvoiceNumber: "CI1"}
			err := e.keep(context.Background(), uuid.New(), d, &o)
			if err == nil {
				t.Error("got nil error, want the defect named")
			}
			if o.Status == StatusValid {
				t.Errorf("status: got %s, want not valid", o.Status)
			}
		})
	}
	e := &Endpoints{Objects: failingStore{}}
	o := OutgoingEInvoice{}
	if err := e.keep(context.Background(), uuid.New(), valid(t), &o); problem.From(err).Code != "platform.unavailable" {
		t.Errorf("a store down: got %v, want platform.unavailable", err)
	}
}

// TestWriteFindsItValid: a render that finds the row made valid by
// another request since it read it answers that row, unchanged and not
// new (the upsert's guard).
func TestWriteFindsItValid(t *testing.T) {
	ctx := context.Background()
	tenant, actor := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // the test's transaction is never committed; rolling it back cannot lose anything
	d := valid(t)
	o := OutgoingEInvoice{ID: uuid.Must(uuid.NewV7()), LegalEntityID: uuid.New(), CustomerInvoiceID: uuid.New(), CustomerInvoiceNumber: "CI1",
		CustomerID: uuid.New(), Kind: d.Kind, TypeCode: d.TypeCode, IssueDate: d.IssueDate, Currency: "AED", Exponent: 2,
		Specification: PINTAECustomization, Status: StatusValid, SHA256: make([]byte, 32), Size: ptr(int64(10)), ContentRef: ptr("k")}
	p := reqctx.Principal{ID: actor, TenantID: tenant}
	if _, err := tx.Exec(ctx, `INSERT INTO einvoicing.outgoing_einvoice (tenant_id, id, legal_entity_id, customer_invoice_id, customer_invoice_number,
			customer_id, kind, type_code, issue_date, currency, net_amount, tax_amount, gross_amount, specification, status, sha256, size, content_ref,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'CI1', $5, 'invoice', '380', '2026-03-10', 'AED', 0, 0, 0, $6, 'valid', $7, 10, 'k', $8, $8)`,
		tenant, o.ID, o.LegalEntityID, o.CustomerInvoiceID, o.CustomerID, PINTAECustomization, o.SHA256, actor); err != nil {
		t.Fatal(err)
	}
	again := o
	again.ID, again.Status, again.Problems, again.SHA256, again.Size, again.ContentRef = uuid.Must(uuid.NewV7()), StatusInvalid,
		[]Finding{{Rule: "r"}}, nil, nil, nil
	got, created, err := (&Endpoints{}).write(ctx, tx, p, again, Company{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := words(got.ID, created, got.Status); w != words(o.ID, false, StatusValid) {
		t.Errorf("got %s, want the valid row %v, not new", w, o.ID)
	}
}
