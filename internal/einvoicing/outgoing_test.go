package einvoicing_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/packs"
)

// The scene's UAE company and the TRNs of the seller and the buyer
// (illustrative: 15 digits, the TIN their first ten).
var uaeCompany = fixture.Company{Code: "5000", Name: "Maple Gulf Trading", Country: "AE", Currency: "AED"}

const (
	sellerTRN = "100234567800003"
	buyerTRN  = "100987654300003"
)

var outgoingPerms = []string{"receivables.invoice.read", "receivables.invoice.write", "receivables.invoice.post", "einvoicing.outgoing.read",
	"einvoicing.outgoing.render", "einvoicing.outgoing.send", "einvoicing.einvoice.read", "platform.operation.read", "platform.operation.cancel"}

// uae is a tenant with Maple Gulf Trading (5000, AE, AED) on the UAE
// pack's tax codes, its FTA registration and its postal address in
// Dubai, a customer in Dubai with its TRN, and the API with object
// storage.
type uae struct {
	f        fixture.Env
	le       uuid.UUID
	customer uuid.UUID
	revenue  uuid.UUID
	store    blobstore.Dir
}

func uaeCodes(t *testing.T) []config.TaxCode {
	t.Helper()
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.8")
	if ae == nil {
		t.Fatal("Get(AE, 2026.8): got nil, want the UAE pack")
	}
	accounts := map[string]string{"1300": "140000", "2300": "240000"}
	var codes []config.TaxCode
	for _, c := range ae.Tax.TaxCodes {
		c.Components = slices.Clone(c.Components)
		for i := range c.Components {
			c.Components[i].Account = accounts[c.Components[i].Account]
		}
		codes = append(codes, c)
	}
	return codes
}

func uaeSetup(t *testing.T, address *config.PostalAddress) *uae {
	t.Helper()
	receivables := config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true}
	f := fixture.NewWith(t, []fixture.Company{uaeCompany}, func(doc *config.Config) {
		co := *doc.Company("2000")
		co.Code, co.Name, co.Currency, co.Country = uaeCompany.Code, uaeCompany.Name, uaeCompany.Currency, uaeCompany.Country
		co.ChartOfAccounts = append(slices.Clone(co.ChartOfAccounts),
			config.Account{Number: "240000", Name: "VAT payable", Type: config.Liability},
			config.Account{Number: "219500", Name: "Customs clearing", Type: config.Liability},
			receivables)
		co.TaxCodes = uaeCodes(t)
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, receivables)
		co.ManualEntryApprovalLimit, co.AutoPostLimit = "50000.00", "500000.00"
		co.TaxRegistrations = []config.TaxRegistration{{Code: "VAT", Authority: "FTA", Number: sellerTRN, ReturnType: "VAT201",
			Frequency: config.TaxQuarterly, FirstPeriodStart: "2026-01-01"}}
		co.PostalAddress = address
		doc.Companies = append(doc.Companies, co)
		doc.Packs = append(doc.Packs, config.PackAdoption{Company: "5000", Country: "AE", Version: "2026.8", AppliedOn: "2026-01-01"})
		// Holidays on business days (illustrative), which the deadlines of
		// documents not transmitted leave out (As built E3): one before the
		// deadlines the tests meet and one the day after them.
		doc.Calendars = append(doc.Calendars, config.Calendar{Company: "5000", Holidays: []config.Holiday{{Date: "2026-03-20", Name: "Holiday"},
			{Date: "2026-04-01", Name: "Holiday after"}}})
		for _, y := range fixture.Years() {
			for _, dt := range []string{"journal_dr", "journal_dg", "journal_dd"} {
				for _, co := range []string{"5000", "2000"} {
					doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: co, DocumentType: dt, FiscalYear: y, First: 100000, Last: 999999})
				}
			}
		}
	}, outgoingPerms...)
	s := &uae{f: f, store: blobstore.Dir{Root: t.TempDir()}}
	s.f.API = apitest.NewWith(t, func(c *app.Config) { c.Objects = s.store })
	pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = '5000'`, []any{f.Tenant.ID}, &s.le)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'AED', 'EUR', '2026-01-01', 0.25, 'CBUAE', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'AED', '2026-01-01', 3.6725, 'CBUAE', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.9, 'CBUAE', $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	s.revenue = f.Account(t, "5000", "400000")
	s.customer = s.newCustomer(t, "BP80000001", "Desert Rose Retail LLC", buyerTRN, "AE-DU")
	return s
}

// newCustomer inserts an active customer of 5000 on 120000 with NT30, its
// legal address in the region (none for ""), and its UAE TRN (none for
// "").
func (s *uae) newCustomer(t *testing.T, number, name, trn, region string) uuid.UUID {
	t.Helper()
	ten, by := s.f.Tenant.ID, s.f.Tenant.Human.ID
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'AE', 'active', $5, $5)`, ten, id, number, name, by)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, ten, id, by)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, ten, id, s.le, s.f.Account(t, "5000", "120000"), by)
	if region != "" {
		pgtest.Exec(t, `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, city, region, country_code, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, 'legal', 'Shop 4, Al Wasl Road', 'Dubai', $3, 'AE', '2026-01-01', $4)`, ten, id, region, by)
	}
	if trn != "" {
		s.taxID(t, id, trn)
	}
	return id
}

func (s *uae) taxID(t *testing.T, partner uuid.UUID, trn string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
		VALUES ($1, $2, 'AE_TRN', $3, $4, right($5, 4), $6, $6)`, s.f.Tenant.ID, partner, apitest.Seal(t, s.f.Tenant.ID, trn),
		apitest.Fingerprint(t, s.f.Tenant.ID, trn), trn, s.f.Tenant.Human.ID)
}

// dubai is the company's postal address in Dubai.
var dubai = &config.PostalAddress{Line1: "Office 1201, Gulf Tower", City: "Dubai", Region: "AE-DU", CountryCode: "AE"}

// item is a customer invoice item on 400000: "amount code[@emirate]".
func (s *uae) item(amount, currency, code string) string {
	tax := ""
	if code != "" {
		c, emirate, onLine := strings.Cut(code, "@")
		tax = fmt.Sprintf(`, "tax_code": %q`, c)
		if onLine {
			tax += fmt.Sprintf(`, "tax_jurisdiction": %q`, emirate)
		}
	}
	return fmt.Sprintf(`{"account_id": %q, "description": "Office chairs", "net_amount": {"amount": %q, "currency": %q}%s}`, s.revenue, amount, currency, tax)
}

// posted enters and posts a customer document of 5000 to the customer:
// extra its kind's fields, in the currency, dated 2026-03-10; it returns
// its id.
func (s *uae) posted(t *testing.T, customer uuid.UUID, extra, currency string, items ...string) string {
	t.Helper()
	doc, tag := s.f.Must(201, "POST", "/v1/customer-invoices", fmt.Sprintf(`{%s"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-03-10",
		"currency": %q, "items": [%s]}`, extra, s.le, customer, currency, strings.Join(items, ", ")), "")
	s.f.Must(200, "POST", "/v1/customer-invoices/"+doc["id"].(string)+":post", `{"posting_date": "2026-03-10"}`, tag)
	return doc["id"].(string)
}

func (s *uae) render(t *testing.T, code int, body string) map[string]any {
	t.Helper()
	out, _ := s.f.Must(code, "POST", "/v1/outgoing-e-invoices", body, "")
	return out
}

// content downloads the e-invoice's XML and parses it back.
func (s *uae) content(t *testing.T, id string) einvoicing.Document {
	t.Helper()
	rec := s.f.API.Do(apitest.Req{Method: "GET", Path: "/v1/outgoing-e-invoices/" + id + "/content", Tenant: s.f.Tenant.ID, As: s.f.Tenant.Human})
	if rec.Code != 200 {
		t.Fatalf("content: got %d %s, want 200", rec.Code, rec.Body)
	}
	d, err := einvoicing.ParsePINTAE(rec.Body.Bytes(), currency.SeededExponent)
	if err != nil {
		t.Fatalf("ParsePINTAE: %v", err)
	}
	if found := einvoicing.Check(d); len(found) > 0 {
		t.Errorf("the rendered XML parsed back: got findings %v, want none", found)
	}
	return d
}

// words is the values separated by spaces.
func words(v ...any) string { return strings.TrimSpace(fmt.Sprintln(v...)) }

func check(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// TestRenderTaxInvoice: a posted AED invoice of standard-rated (S5 in
// Dubai) and zero-rated items is rendered as a valid tax invoice 380,
// its XML kept and read back with the seller's and buyer's TRNs, their
// endpoints and emirates, the VAT per category and each line's VAT and
// amount in AED (BTAE-08, a zero-rated line's 0, and BTAE-10); a second render
// answers it unchanged; the render is audited and published.
func TestRenderTaxInvoice(t *testing.T) {
	s := uaeSetup(t, dubai)
	invoice := s.posted(t, s.customer, "", "AED", s.item("10000.00", "AED", "S5@AE-DU"), s.item("2000.00", "AED", "Z0"))
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	check(t, "status, kind, type code", words(got["status"], got["kind"], got["type_code"]), "valid invoice 380")
	check(t, "company and customer", words(got["company_code"], got["company_name"], got["customer_number"], got["customer_name"]),
		"5000 Maple Gulf Trading BP80000001 Desert Rose Retail LLC")
	check(t, "amounts", words(got["net_amount"], got["tax_amount"], got["gross_amount"]),
		"map[amount:12000.00 currency:AED] map[amount:500.00 currency:AED] map[amount:12500.00 currency:AED]")
	check(t, "specification", got["specification"], einvoicing.PINTAECustomization)
	d := s.content(t, got["id"].(string))
	check(t, "seller", words(d.Seller.TRN, d.Seller.EndpointScheme, d.Seller.Endpoint, d.Seller.Address.Subentity, d.Seller.Address.Country),
		sellerTRN+" 0235 1002345678 DXB AE")
	check(t, "buyer", words(d.Buyer.Name, d.Buyer.TRN, d.Buyer.Endpoint, d.Buyer.Address.Subentity), "Desert Rose Retail LLC "+buyerTRN+" 1009876543 DXB")
	var breakdown []string
	for _, b := range d.Subtotals {
		breakdown = append(breakdown, words(b.Category, b.Percent, int64(b.Taxable), int64(b.Tax)))
	}
	check(t, "VAT breakdown", strings.Join(breakdown, ", "), "S 5 1000000 50000, Z 0 200000 0")
	check(t, "lines", words(len(d.Lines), d.Lines[0].Category, d.Lines[0].Percent, d.Lines[0].Quantity, d.Lines[0].Unit), "2 S 5 1 C62")
	check(t, "no AED total on an AED invoice", words(d.TaxCurrency == "", d.AccountingTax == nil), "true true")
	var inAED []string
	for _, l := range d.Lines {
		inAED = append(inAED, words(l.Category, int64(*l.AccountingTax), int64(*l.AccountingAmount))) // BTAE-08, BTAE-10 on every line (IBR-194-AE)
	}
	check(t, "each line's VAT and amount in AED", strings.Join(inAED, ", "), "S 50000 1050000, Z 0 200000")
	again := s.render(t, 200, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	check(t, "a second render", words(again["id"], again["version"], again["sha256"]), words(got["id"], got["version"], got["sha256"]))
	var events, audits int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM einvoicing.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.einvoicing.outgoing_einvoice_rendered.v1'),
		(SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.outgoing.rendered')`, []any{s.f.Tenant.ID}, &events, &audits)
	check(t, "events and audit records", words(events, audits), "1 1")
	read, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+got["id"].(string), "", "")
	check(t, "read back", words(read["status"], read["customer_invoice_number"], read["sha256"]), words("valid", got["customer_invoice_number"], got["sha256"]))
	other := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	second := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, other))
	list := func(query string) string {
		t.Helper()
		page, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?"+query, "", "")
		var ids []string
		for _, it := range page["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		if next, ok := page["next_page_token"].(string); ok {
			ids = append(ids, "next:"+fmt.Sprint(next != ""))
		}
		return strings.Join(ids, " ")
	}
	for _, c := range []struct{ name, query, want string }{
		{"newest first", "", words(second["id"], got["id"])},
		{"by customer document", "customer_invoice_id=" + invoice, words(got["id"])},
		{"by company and status", "legal_entity_id=" + s.le.String() + "&status=valid&page_size=1", words(second["id"], "next:true")},
		{"another company's", "legal_entity_id=" + s.f.US.String(), ""},
		{"by status invalid", "status=invalid", ""},
	} {
		t.Run(c.name, func(t *testing.T) { check(t, c.name, list(c.query), c.want) })
	}
}

// TestRenderTaxCreditNote: a posted credit memo naming the invoice is a
// tax credit note 381 with its reason; without the reason it is
// refused, and an invoice with one is refused.
func TestRenderTaxCreditNote(t *testing.T) {
	s := uaeSetup(t, dubai)
	invoice := s.posted(t, s.customer, "", "AED", s.item("1000.00", "AED", "S5@AE-DU"))
	memo := s.posted(t, s.customer, fmt.Sprintf(`"kind": "credit_memo", "reference_invoice_id": %q, `, invoice), "AED", s.item("100.00", "AED", "S5@AE-DU"))
	s.f.Problem(422, "einvoicing.credit_note_reason", "POST", "/v1/outgoing-e-invoices", fmt.Sprintf(`{"customer_invoice_id": %q}`, memo), "")
	s.f.Problem(422, "einvoicing.credit_note_reason", "POST", "/v1/outgoing-e-invoices",
		fmt.Sprintf(`{"customer_invoice_id": %q, "credit_note_reason": {"code": "CR-01", "text": "Price reduced"}}`, invoice), "")
	s.f.Problem(422, "request.validation_failed", "POST", "/v1/outgoing-e-invoices",
		fmt.Sprintf(`{"customer_invoice_id": %q, "credit_note_reason": {"code": "CR-01", "text": "  "}}`, memo), "")
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q, "credit_note_reason": {"code": "CR-01", "text": "Price reduced"}}`, memo))
	check(t, "status, kind, type code, reason", words(got["status"], got["kind"], got["type_code"], got["credit_note_reason"]),
		"valid credit_note 381 map[code:CR-01 text:Price reduced]")
	d := s.content(t, got["id"].(string))
	invoiceNumber, _ := s.f.Must(200, "GET", "/v1/customer-invoices/"+invoice, "", "")
	check(t, "the credited invoice", d.Reference, invoiceNumber["document_number"])
	check(t, "the reason", words(d.Reason.Code, d.Reason.Text), "CR-01 Price reduced")
	check(t, "credit note amounts", words(int64(d.TaxExclusive), int64(d.Tax), int64(d.Payable)), "10000 500 10500")
}

// TestRenderInvalidThenValid: a customer without TRN or address renders
// an invalid e-invoice naming what is missing, with no XML; once the
// customer's TRN and address are recorded, rendering again makes it
// valid in the same row.
func TestRenderInvalidThenValid(t *testing.T) {
	s := uaeSetup(t, dubai)
	bare := s.newCustomer(t, "BP80000002", "Oasis Trading", "", "")
	invoice := s.posted(t, bare, "", "AED", s.item("500.00", "AED", "Z0"))
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	check(t, "status and XML", words(got["status"], got["sha256"], got["size"]), "invalid <nil> <nil>")
	var rules []string
	for _, p := range got["problems"].([]any) {
		rules = append(rules, p.(map[string]any)["rule"].(string)+" "+p.(map[string]any)["term"].(string))
	}
	check(t, "problems", rules, "[einvoicing.pint_ae.buyer IBG-07 einvoicing.pint_ae.buyer IBG-07 einvoicing.pint_ae.buyer IBG-07 einvoicing.pint_ae.buyer IBG-07 einvoicing.pint_ae.buyer IBG-07]")
	s.f.Problem(409, "einvoicing.document_invalid", "GET", "/v1/outgoing-e-invoices/"+got["id"].(string)+"/content", "", "")
	s.taxID(t, bare, buyerTRN)
	pgtest.Exec(t, `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, city, region, country_code, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, 'legal', 'Unit 7, Corniche Road', 'Abu Dhabi', 'AUH', 'AE', '2026-01-01', $3)`, s.f.Tenant.ID, bare, s.f.Tenant.Human.ID)
	again := s.render(t, 200, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	check(t, "rendered again", words(again["id"] == got["id"], again["status"], again["version"], len(again["problems"].([]any))), "true valid 2 0")
	check(t, "the buyer's emirate as given", s.content(t, again["id"].(string)).Buyer.Address.Subentity, "AUH")
	var before string
	pgtest.Scan(t, `SELECT coalesce(payload->>'status_before', 'null') FROM einvoicing.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.einvoicing.outgoing_einvoice_rendered.v1'
		ORDER BY occurred_at DESC, id DESC LIMIT 1`, []any{s.f.Tenant.ID}, &before)
	check(t, "the second event's status before", before, "invalid")
}

// TestRenderForeignCurrency: an invoice in USD is a valid tax invoice
// whose line states its VAT in AED (BTAE-08), the entry's, which is the
// document's VAT in AED (IBT-111), and its amount with VAT in AED
// (BTAE-10), the entry's local base (1,000.00 USD at 3.6725) and that VAT
// (ADR-0061 As built batch 88); its e-invoice names the billing document
// it posted from.
func TestRenderForeignCurrency(t *testing.T) {
	s := uaeSetup(t, dubai)
	invoice, billing := s.posted(t, s.customer, "", "USD", s.item("1000.00", "USD", "S5@AE-DU")), uuid.New()
	// As a billing document's posting names it (T79b); the invoice's
	// trigger, which keeps a posted invoice's source, is skipped here.
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pgtest.Admin(t), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE receivables.customer_invoice SET source_document_id = $2, source_document_number = 'BD100001' WHERE id = $1`,
			invoice, billing)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	check(t, "status and source", words(got["status"], got["source_document_id"], got["source_document_number"]), words("valid", billing, "BD100001"))
	d := s.content(t, got["id"].(string))
	if d.AccountingTax == nil || d.Lines[0].AccountingTax == nil || d.Lines[0].AccountingAmount == nil {
		t.Fatalf("got IBT-111 %v, BTAE-08 %v, BTAE-10 %v, want all three", d.AccountingTax, d.Lines[0].AccountingTax, d.Lines[0].AccountingAmount)
	}
	check(t, "the line's VAT in AED, the document's", words(*d.Lines[0].AccountingTax), words(*d.AccountingTax))
	check(t, "the line's amount in AED", words(*d.Lines[0].AccountingAmount), words(367250+*d.AccountingTax))
}

// TestRenderRefusals: what the format does not render is refused before
// anything is kept, and an e-invoice of a company the caller may not act
// in is not found.
func TestRenderRefusals(t *testing.T) {
	s := uaeSetup(t, nil)
	draft, _ := s.f.Must(201, "POST", "/v1/customer-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-03-10",
		"currency": "AED", "items": [%s]}`, s.le, s.customer, s.item("10.00", "AED", "Z0")), "")
	debit := s.posted(t, s.customer, `"kind": "debit_memo", `, "AED", s.item("10.00", "AED", "Z0"))
	for _, c := range []struct{ name, body, code string }{
		{"a draft", fmt.Sprintf(`{"customer_invoice_id": %q}`, draft["id"]), "einvoicing.document_not_posted"},
		{"a debit memo", fmt.Sprintf(`{"customer_invoice_id": %q}`, debit), "einvoicing.unsupported_document"},
		{"no such document", fmt.Sprintf(`{"customer_invoice_id": %q}`, uuid.New()), "request.reference_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status := map[string]int{"einvoicing.document_not_posted": 409}[c.code]
			if status == 0 {
				status = 422
			}
			s.f.Problem(status, c.code, "POST", "/v1/outgoing-e-invoices", c.body, "")
		})
	}
	invoice := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	check(t, "a company without its postal address", got["status"], "invalid")
	outsider := s.f.As(s.f.Reader(t, "outsider", s.f.US))
	if code, _, _ := outsider.Do("GET", "/v1/outgoing-e-invoices/"+got["id"].(string), "", ""); code != 404 {
		t.Errorf("another company's reader: got %d, want 404", code)
	}
	if code, _, _ := s.f.Do("GET", "/v1/outgoing-e-invoices/"+uuid.NewString()+"/content", "", ""); code != 404 {
		t.Errorf("no such e-invoice's content: got %d, want 404", code)
	}
}

// TestRenderNotAUAECompany: a posted invoice of company 2000 (DE, EUR)
// is not rendered as PINT AE.
func TestRenderNotAUAECompany(t *testing.T) {
	s := uaeSetup(t, dubai)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, s.f.Tenant.ID, s.customer, s.f.DE, s.f.Account(t, "2000", "120000"), s.f.Tenant.Human.ID)
	doc, tag := s.f.Must(201, "POST", "/v1/customer-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-03-10",
		"currency": "EUR", "items": [{"account_id": %q, "net_amount": {"amount": "10.00", "currency": "EUR"}}]}`, s.f.DE, s.customer,
		s.f.Account(t, "2000", "400000")), "")
	s.f.Must(200, "POST", "/v1/customer-invoices/"+doc["id"].(string)+":post", `{"posting_date": "2026-03-10"}`, tag)
	s.f.Problem(422, "einvoicing.not_a_uae_company", "POST", "/v1/outgoing-e-invoices", fmt.Sprintf(`{"customer_invoice_id": %q}`, doc["id"]), "")
}

// TestRenderWithoutStore: a server without object storage refuses to
// render (503) and to serve an e-invoice's XML.
func TestRenderWithoutStore(t *testing.T) {
	s := uaeSetup(t, dubai)
	invoice := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	s.f.API = apitest.New(t)
	s.f.Problem(503, "platform.unavailable", "POST", "/v1/outgoing-e-invoices", fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice), "")
	s.f.Problem(503, "platform.unavailable", "GET", "/v1/outgoing-e-invoices/"+got["id"].(string)+"/content", "", "")
}

// TestOutgoingContentChecked: XML whose bytes no longer match the hash
// recorded is refused (409 einvoicing.document_changed), and XML gone
// from the store is not found.
func TestOutgoingContentChecked(t *testing.T) {
	s := uaeSetup(t, dubai)
	invoice := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	var ref string
	pgtest.Scan(t, `SELECT content_ref FROM einvoicing.outgoing_einvoice WHERE tenant_id = $1 AND id = $2`, []any{s.f.Tenant.ID, got["id"]}, &ref)
	ctx := context.Background()
	if err := s.store.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	s.f.Problem(404, "request.not_found", "GET", "/v1/outgoing-e-invoices/"+got["id"].(string)+"/content", "", "")
	if err := s.store.Put(ctx, ref, []byte("<Invoice/>"), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	s.f.Problem(409, "einvoicing.document_changed", "GET", "/v1/outgoing-e-invoices/"+got["id"].(string)+"/content", "", "")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.f.API = apitest.NewWith(t, func(c *app.Config) { c.Objects = blobstore.Dir{Root: file} })
	s.f.Problem(503, "platform.unavailable", "GET", "/v1/outgoing-e-invoices/"+got["id"].(string)+"/content", "", "")
	other := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	s.f.Problem(503, "platform.unavailable", "POST", "/v1/outgoing-e-invoices", fmt.Sprintf(`{"customer_invoice_id": %q}`, other), "")
}

// TestRenderMasterData: what the master data lacks is named, not
// guessed: an item without description or tax code, a company without
// its FTA registration, a customer whose personal data is blocked; a
// sealed TRN that does not open fails the request.
func TestRenderMasterData(t *testing.T) {
	s := uaeSetup(t, dubai)
	bare := fmt.Sprintf(`{"account_id": %q, "net_amount": {"amount": "5.00", "currency": "AED"}}`, s.revenue)
	invoice := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "S5@AE-DU"), s.item("20.00", "AED", "S5@AE-DU"), bare)
	pgtest.Exec(t, `UPDATE tax.registration SET is_active = false, version = version + 1 WHERE tenant_id = $1`, s.f.Tenant.ID)
	got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	var messages []string
	for _, p := range got["problems"].([]any) {
		messages = append(messages, p.(map[string]any)["message"].(string))
	}
	check(t, "problems", strings.Join(messages, "; "), "the seller's TRN is missing or not 15 digits; the seller's electronic address is not scheme 0235 "+
		"with a ten-digit TIN; line 3: its item name or description is missing; line 3: its VAT category is missing, not one of S, Z, E, O, AE")
	pgtest.Exec(t, `UPDATE tax.registration SET is_active = true, number_cipher = '\x00', version = version + 1 WHERE tenant_id = $1`, s.f.Tenant.ID)
	if code, _, _ := s.f.Do("POST", "/v1/outgoing-e-invoices", fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice), ""); code != 500 {
		t.Errorf("a registration number that does not open: got %d, want 500", code)
	}
	other := s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))
	pgtest.Exec(t, `UPDATE tax.registration SET number_cipher = $2, version = version + 1 WHERE tenant_id = $1`, s.f.Tenant.ID,
		apitest.Seal(t, s.f.Tenant.ID, sellerTRN))
	pgtest.Exec(t, `UPDATE partner.tax_identifier SET value_cipher = '\x00', version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, s.f.Tenant.ID, s.customer)
	if code, _, _ := s.f.Do("POST", "/v1/outgoing-e-invoices", fmt.Sprintf(`{"customer_invoice_id": %q}`, other), ""); code != 500 {
		t.Errorf("a customer TRN that does not open: got %d, want 500", code)
	}
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', personal_data_blocked_at = now(), version = version + 1 WHERE tenant_id = $1 AND id = $2`,
		s.f.Tenant.ID, s.customer)
	blocked := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, other))
	check(t, "a blocked customer", words(blocked["status"], blocked["problems"].([]any)[0].(map[string]any)["message"]),
		"invalid the customer's personal data is blocked")
}

// TestOutgoingDatabaseFailures: every statement of rendering and reading
// fails cleanly (CLAUDE.md: FailingTx on every database path).
func TestOutgoingDatabaseFailures(t *testing.T) {
	s := uaeSetup(t, dubai)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.EInvoicing(app.Config{Objects: s.store, FieldKeys: apitest.FieldKeys(), Pages: pages})
	invoice := uuid.MustParse(s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "S5@AE-DU")))
	memo := uuid.MustParse(s.posted(t, s.customer, fmt.Sprintf(`"kind": "credit_memo", "reference_invoice_id": %q, `, invoice), "AED",
		s.item("1.00", "AED", "S5@AE-DU")))
	valid := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, invoice))
	id := uuid.MustParse(valid["id"].(string))
	reason := gen.NewOptCreditNoteReason(gen.CreditNoteReason{Code: "CR-01", Text: "Returned"})
	calls := map[string]func(ctx context.Context) error{
		"render a credit note": func(ctx context.Context) error {
			_, err := h.CreateOutgoingEInvoice(ctx, &gen.OutgoingEInvoiceRender{CustomerInvoiceID: memo, CreditNoteReason: reason}, gen.CreateOutgoingEInvoiceParams{})
			return err
		},
		"render a valid one again": func(ctx context.Context) error {
			_, err := h.CreateOutgoingEInvoice(ctx, &gen.OutgoingEInvoiceRender{CustomerInvoiceID: invoice}, gen.CreateOutgoingEInvoiceParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetOutgoingEInvoice(ctx, gen.GetOutgoingEInvoiceParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListOutgoingEInvoices(ctx, gen.ListOutgoingEInvoicesParams{})
			return err
		},
		"content": func(ctx context.Context) error {
			_, err := h.GetOutgoingEInvoiceContent(ctx, gen.GetOutgoingEInvoiceContentParams{ID: id})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, name, call)
		})
	}
	err = apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListOutgoingEInvoices(ctx, gen.ListOutgoingEInvoicesParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	if p := problem.From(err); p.Code != "request.invalid_page_size" {
		t.Errorf("a page of none: got %v, want request.invalid_page_size", p)
	}
}
