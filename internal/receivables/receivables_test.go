package receivables_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"receivables.invoice.read", "receivables.invoice.write", "receivables.invoice.post", "receivables.payment.apply",
	"ledger.entry.read", "ledger.entry.post", "ledger.clearing.post"}

// env is a configured tenant whose company 2000 (EUR) has the output tax
// codes A1 (19% to 30 June 2026, 21% from 1 July), A0 (zero-rated), AJ
// (5%, levying its lines' subdivision, any of DE), AG (5%, levying it and
// accepting DE-BY and DE-BE alone) and A2 (two components of 60%) on
// account 230000, the customer reconciliation account 120000, number
// ranges for the entries customer invoices and credit memos post
// (journal_dr, journal_dg) and for
// incoming payments and their clearing entries (incoming_payment,
// journal_dz), house bank MAIN with the main bank account 102000 and the
// incoming payments clearing account 103000, and a customer set up in the
// company on 120000 with the payment term NT30.
type env struct {
	fixture.Env
	revenue, other  uuid.UUID // accounts 400000 and 530000 of company 2000
	receivable, tax uuid.UUID // accounts 120000 and 230000
	bank, incoming  uuid.UUID // accounts 102000 and 103000
	customer        uuid.UUID
}

// outputCodes gives company 2000 its output tax account and codes;
// rounding is the company's tax rounding ("" for the default).
func outputCodes(rounding string) func(*config.Config) {
	return func(doc *config.Config) {
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "230000", Name: "Output tax", Type: config.Liability},
			config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
		// House bank MAIN posts its statements to 102000 and its incoming
		// payments to 103000 (ADR-0047); its clearing account 100000 and the
		// incoming account are open-item managed.
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "102000", Name: "Bank - Deutsche Bank", Type: config.Asset},
			config.Account{Number: "103000", Name: "Bank incoming - Deutsche Bank", Type: config.Asset, OpenItems: true})
		for i := range de.ChartOfAccounts {
			if de.ChartOfAccounts[i].Number == "100000" {
				de.ChartOfAccounts[i].OpenItems = true
			}
		}
		de.HouseBank.MainGLAccount, de.HouseBank.IncomingGLAccount = "102000", "103000"
		for _, y := range fixture.Years() {
			doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_dr", FiscalYear: y, First: 100000, Last: 999999},
				config.NumberRange{Company: "2000", DocumentType: "journal_dz", FiscalYear: y, First: 100000, Last: 999999},
				config.NumberRange{Company: "2000", DocumentType: "journal_dg", FiscalYear: y, First: 100000, Last: 999999})
			first := int64(100000)
			if y != 2026 { // a range may not repeat another year's numbers (config.RangeRepeats)
				first = 1000000
			}
			doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "incoming_payment", FiscalYear: y, Prefix: "IP",
				First: first, Last: first + 899999})
		}
		output := func(code, kind string, onLine bool, rates ...config.TaxRate) config.TaxCode {
			c := config.TaxComponent{Key: "vat", AccountKey: config.KeyOutput, Account: "230000", Rates: rates}
			if onLine {
				c.Jurisdiction, c.JurisdictionOnLine = "DE", true
			}
			return config.TaxCode{Code: code, Description: "Output tax " + code, Direction: config.TaxOutput, Kind: kind, Components: []config.TaxComponent{c}}
		}
		de.TaxCodes = append(de.TaxCodes,
			output("A1", "", false, config.TaxRate{RatePercent: "19", ValidFrom: "2026-01-01", ValidTo: "2026-06-30"},
				config.TaxRate{RatePercent: "21", ValidFrom: "2026-07-01"}),
			output("A0", config.TaxZero, false, config.TaxRate{RatePercent: "0", ValidFrom: "2026-01-01"}),
			output("AJ", "", true, config.TaxRate{RatePercent: "5", ValidFrom: "2026-01-01"}),
			guarded(output("AG", "", true, config.TaxRate{RatePercent: "5", ValidFrom: "2026-01-01"}), "DE-BY", "DE-BE"),
			// Two components, 60% each: more than an amount near the range's end holds.
			config.TaxCode{Code: "A2", Description: "Output tax A2", Direction: config.TaxOutput, Components: []config.TaxComponent{
				{Key: "federal", AccountKey: config.KeyOutput, Account: "230000", Rates: []config.TaxRate{{RatePercent: "60", ValidFrom: "2026-01-01"}}},
				{Key: "state", AccountKey: config.KeyOutput, Account: "230000", Rates: []config.TaxRate{{RatePercent: "60", ValidFrom: "2026-01-01"}}}}})
		de.TaxSettings.Rounding = rounding
	}
}

// guarded is the code with its component accepting the subdivisions alone.
func guarded(code config.TaxCode, subdivisions ...string) config.TaxCode {
	code.Components[0].Subdivisions = subdivisions
	return code
}

func setup(t *testing.T) env { t.Helper(); return setupRounding(t, "") }

func setupRounding(t *testing.T, rounding string) env {
	t.Helper()
	f := fixture.NewWith(t, nil, outputCodes(rounding), perms...)
	e := env{Env: f, revenue: f.Account(t, "2000", "400000"), other: f.Account(t, "2000", "530000"), receivable: f.Account(t, "2000", "120000"),
		tax: f.Account(t, "2000", "230000"), bank: f.Account(t, "2000", "102000"), incoming: f.Account(t, "2000", "103000")}
	e.customer = e.partner(t, "BP70000001", "Northwind Traders", "active", "customer")
	e.companyData(t, e.customer, "NT30")
	return e
}

// companyData sets the customer up in company 2000 on its receivables
// account 120000 with the payment term, as an approved partner change
// would (T74a).
func (e env) companyData(t *testing.T, customer uuid.UUID, term string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, e.Tenant.ID, customer, e.DE, e.receivable, term, e.Tenant.Human.ID)
}

// partner inserts a business partner in the status with the roles.
func (e env) partner(t *testing.T, number, name, status string, roles ...string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'DE', $5, $6, $6)`, e.Tenant.ID, id, number, name, status, e.Tenant.Human.ID)
	for _, r := range roles {
		pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, $3, $4)`, e.Tenant.ID, id, r, e.Tenant.Human.ID)
	}
	return id
}

// item is the JSON of an item on the revenue account; code "" for none.
func (e env) item(amount, code string) string { return itemOn(e.revenue, amount, "EUR", code) }

func itemOn(account uuid.UUID, amount, currency, code string) string {
	tax := ""
	if code != "" {
		tax = fmt.Sprintf(`, "tax_code": %q`, code)
	}
	return fmt.Sprintf(`{"account_id": %q, "net_amount": {"amount": %q, "currency": %q}%s}`, account, amount, currency, tax)
}

// in is the item's JSON naming the jurisdiction its tax is levied in.
func in(item, jurisdiction string) string {
	return strings.TrimSuffix(item, "}") + fmt.Sprintf(`, "tax_jurisdiction": %q}`, jurisdiction)
}

// invoice is the JSON of an invoice of company 2000 in EUR to the customer.
func (e env) invoice(customer uuid.UUID, date string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": %q, "currency": "EUR", "items": [%s]}`,
		e.DE, customer, date, strings.Join(items, ", "))
}

func amount(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a, ok := m[field].(map[string]any)
	if !ok {
		t.Fatalf("%s: got %v, want an amount", field, m[field])
	}
	return fmt.Sprintf("%v %v", a["amount"], a["currency"])
}

func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

// field is the field the problem's first field error names.
func field(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	e, _ := errs[0].(map[string]any)
	return e["field"]
}

// TestEnterAndReadBack: a draft is entered with its number, its tax per
// item at the invoice date and totals that add up exactly; it reads back
// the same, with the names of what it references; nothing is posted; the
// audit record and CustomerInvoiceChanged are written (T75a acceptance).
func TestEnterAndReadBack(t *testing.T) {
	e := setup(t)
	// 19% of 100.10 + 33.33 = 133.43 is 25.3517: 25.35 once for the code,
	// shared out 19.02 and 6.33; the third item bears none.
	body := e.invoice(e.customer, "2026-03-10", e.item("100.10", "A1"), e.item("33.33", "A1"),
		strings.Replace(itemOn(e.other, "10.00", "EUR", ""), `}`, `}, "description": "Freight recharged"`, 1))
	body = strings.Replace(body, `"currency": "EUR", "items"`, `"posting_date": "2026-03-12", "currency": "EUR", "items"`, 1)
	got, tag := e.Must(201, "POST", "/v1/customer-invoices", body, "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	for f, want := range map[string]any{"status": "draft", "kind": "invoice", "company_code": "2000", "company_name": "Acme GmbH",
		"customer_number": "BP70000001", "customer_name": "Northwind Traders", "invoice_date": "2026-03-10", "posting_date": "2026-03-12",
		"currency": "EUR", "customer_id": e.customer.String(), "legal_entity_id": e.DE.String()} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "CI") {
		t.Errorf("document number: got %v, want one of the customer_invoice range (CI…)", got["document_number"])
	}
	for f, want := range map[string]string{"net_amount": "143.43 EUR", "tax_amount": "25.35 EUR", "gross_amount": "168.78 EUR"} {
		if a := amount(t, got, f); a != want {
			t.Errorf("%s: got %s, want %s", f, a, want)
		}
	}
	its := items(t, got)
	if len(its) != 3 {
		t.Fatalf("items: got %d, want 3", len(its))
	}
	for i, want := range []struct{ net, tax, gross string }{{"100.10 EUR", "19.02 EUR", "119.12 EUR"}, {"33.33 EUR", "6.33 EUR", "39.66 EUR"},
		{"10.00 EUR", "0.00 EUR", "10.00 EUR"}} {
		t.Run(fmt.Sprintf("item %d", i+1), func(t *testing.T) {
			if n := its[i]["item_no"]; n != float64(i+1) {
				t.Errorf("item number: got %v, want %d", n, i+1)
			}
			for f, w := range map[string]string{"net_amount": want.net, "tax_amount": want.tax, "gross_amount": want.gross} {
				if a := amount(t, its[i], f); a != w {
					t.Errorf("%s: got %s, want %s", f, a, w)
				}
			}
		})
	}
	if its[0]["account_number"] != "400000" || its[0]["account_name"] != "Sales Revenue" {
		t.Errorf("item 1's account: got %v %v, want 400000 Sales Revenue", its[0]["account_number"], its[0]["account_name"])
	}
	if its[2]["description"] != "Freight recharged" || its[2]["tax_code"] != nil {
		t.Errorf("item 3: got description %v and tax code %v, want Freight recharged and none", its[2]["description"], its[2]["tax_code"])
	}
	id, _ := got["id"].(string)
	read, readTag := e.Must(200, "GET", "/v1/customer-invoices/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b || readTag != tag {
		t.Errorf("read back: got %s (%s), want %s (%s)", a, readTag, b, tag)
	}
	e.recorded(t, id, got)
	var entries int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &entries)
	if entries != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", entries)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recorded checks the audit record and the event of an entered invoice.
func (e env) recorded(t *testing.T, id string, got map[string]any) {
	t.Helper()
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'receivables.customer_invoice.create' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the entry: got %d, want 1", audits)
	}
	var events int
	var payload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.receivables.customer_invoice_changed.v1'`, []any{e.Tenant.ID, id}, &events, &payload)
	if events != 1 {
		t.Fatalf("CustomerInvoiceChanged events: got %d, want 1", events)
	}
	want := map[string]any{"customer_invoice_id": id, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
		"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "kind": "invoice",
		"reference_invoice_id": nil, "reference_invoice_number": nil, "invoice_date": "2026-03-10", "net_amount": map[string]any{"amount": "143.43", "currency": "EUR"},
		"tax_amount":   map[string]any{"amount": "25.35", "currency": "EUR"},
		"gross_amount": map[string]any{"amount": "168.78", "currency": "EUR"}, "status": "draft", "status_before": nil, "version": float64(1)}
	if a, b := mustJSON(t, payload), mustJSON(t, want); a != b {
		t.Errorf("event payload: got %s, want %s", a, b)
	}
}

// TestIdempotentEntry: the same Idempotency-Key enters one invoice and
// answers it again.
func TestIdempotentEntry(t *testing.T) {
	e := setup(t)
	req := apitest.Req{Method: "POST", Path: "/v1/customer-invoices", Body: e.invoice(e.customer, "2026-03-10", e.item("50.00", "A1")),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || second.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice: %s", first.Code, second.Code, second.Body)
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	if a["id"] != b["id"] || a["document_number"] != b["document_number"] {
		t.Errorf("replay: got %v %v, want the first answer's %v %v", b["id"], b["document_number"], a["id"], a["document_number"])
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 1 {
		t.Errorf("invoices: got %d, want 1", n)
	}
}

// TestTaxAtTheInvoiceDate: an item's tax is what its code's rate in force
// on the invoice date gives; a zero-rated code gives none.
func TestTaxAtTheInvoiceDate(t *testing.T) {
	e := setup(t)
	for _, c := range []struct{ name, date, code, tax, gross string }{
		{"before the rate changes", "2026-06-30", "A1", "19.00 EUR", "119.00 EUR"},
		{"from the day it changes", "2026-07-01", "A1", "21.00 EUR", "121.00 EUR"},
		{"zero-rated", "2026-07-01", "A0", "0.00 EUR", "100.00 EUR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := e.In(t).Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, c.date, e.item("100.00", c.code)), "")
			if a := amount(t, got, "tax_amount"); a != c.tax {
				t.Errorf("tax: got %s, want %s", a, c.tax)
			}
			if a := amount(t, got, "gross_amount"); a != c.gross {
				t.Errorf("gross: got %s, want %s", a, c.gross)
			}
			if code := items(t, got)[0]["tax_code"]; code != c.code {
				t.Errorf("item's tax code: got %v, want %s", code, c.code)
			}
		})
	}
}

// TestRoundingFollowsTheCompany: tax is rounded once per code over the
// invoice by default and per item when the company's tax settings say
// line; either way the items' tax adds up to the invoice's.
func TestRoundingFollowsTheCompany(t *testing.T) {
	for _, c := range []struct{ rounding, tax, items string }{
		{"", "0.02 EUR", "0.01 EUR 0.01 EUR 0.00 EUR"}, // 19% of 0.09 is 0.0171
		{config.TaxRoundLine, "0.03 EUR", "0.01 EUR 0.01 EUR 0.01 EUR"},
	} {
		t.Run("rounding "+c.rounding, func(t *testing.T) {
			e := setupRounding(t, c.rounding)
			got, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("0.03", "A1"), e.item("0.03", "A1"),
				e.item("0.03", "A1")), "")
			if a := amount(t, got, "tax_amount"); a != c.tax {
				t.Errorf("tax: got %s, want %s", a, c.tax)
			}
			var each []string
			for _, it := range items(t, got) {
				each = append(each, amount(t, it, "tax_amount"))
			}
			if a := strings.Join(each, " "); a != c.items {
				t.Errorf("items' tax: got %s, want %s", a, c.items)
			}
		})
	}
}

// TestRefusals: each refusal names the request's own field.
func TestRefusals(t *testing.T) {
	e := setup(t)
	supplierOnly := e.Supplier
	blocked := e.partner(t, "BP70000002", "Blocked Ltd", "blocked", "customer")
	ok := e.item("100.00", "A1")
	swap := func(body, old, replacement string) string {
		if !strings.Contains(body, old) {
			t.Fatalf("the body does not contain %q", old)
		}
		return strings.Replace(body, old, replacement, 1)
	}
	base := e.invoice(e.customer, "2026-03-10", ok)
	for _, c := range []struct {
		name, body  string
		status      int
		code, field string
	}{
		{"an unknown customer", e.invoice(uuid.New(), "2026-03-10", ok), 422, "request.reference_not_found", "customer_id"},
		{"a partner without the customer role", e.invoice(supplierOnly, "2026-03-10", ok), 422, "receivables.customer_not_usable", "customer_id"},
		{"a blocked customer", e.invoice(blocked, "2026-03-10", ok), 422, "receivables.customer_not_usable", "customer_id"},
		{"an unknown legal entity", swap(base, e.DE.String(), uuid.NewString()), 422, "request.reference_not_found", "legal_entity_id"},
		{"an unknown account", e.invoice(e.customer, "2026-03-10", ok, itemOn(uuid.New(), "1.00", "EUR", "")), 422,
			"receivables.account_not_postable", "items[1].account_id"},
		{"a reconciliation account", e.invoice(e.customer, "2026-03-10", itemOn(e.Account(t, "2000", "211000"), "1.00", "EUR", "")), 422,
			"receivables.account_not_postable", "items[0].account_id"},
		{"an unknown tax code", e.invoice(e.customer, "2026-03-10", ok, e.item("1.00", "ZZ")), 422, "receivables.unknown_tax_code", "items[1].tax_code"},
		{"an input tax code", e.invoice(e.customer, "2026-03-10", e.item("1.00", "V1")), 422, "receivables.unknown_tax_code", "items[0].tax_code"},
		{"a code with no rate on the invoice date", e.invoice(e.customer, "2025-12-31", e.item("1.00", "A1")), 422, "receivables.unknown_tax_code",
			"items[0].tax_code"},
		{"a code levying its lines' subdivision without one", e.invoice(e.customer, "2026-03-10", ok, e.item("1.00", "AJ")), 422,
			"receivables.tax_jurisdiction_required", "items[1].tax_jurisdiction"},
		{"a jurisdiction with a code that levies no line's subdivision", e.invoice(e.customer, "2026-03-10", in(ok, "DE-BY")), 422,
			"receivables.tax_jurisdiction_not_levied", "items[0].tax_jurisdiction"},
		{"a jurisdiction without a tax code", e.invoice(e.customer, "2026-03-10", in(e.item("1.00", ""), "DE-BY")), 422,
			"receivables.tax_jurisdiction_not_levied", "items[0].tax_jurisdiction"},
		{"a jurisdiction of another country than the code's", e.invoice(e.customer, "2026-03-10", in(e.item("1.00", "AJ"), "FR-75")), 422,
			"receivables.tax_jurisdiction_not_levied", "items[0].tax_jurisdiction"},
		{"a jurisdiction the code does not accept", e.invoice(e.customer, "2026-03-10", in(e.item("1.00", "AG"), "DE-HH")), 422,
			"receivables.tax_jurisdiction_not_levied", "items[0].tax_jurisdiction"},
		{"a jurisdiction that is no subdivision's code", e.invoice(e.customer, "2026-03-10", in(e.item("1.00", "AJ"), "Bavaria")), 400,
			"request.invalid_body", ""},
		{"an unknown currency", swap(swap(base, `"currency": "EUR", "items"`, `"currency": "XXA", "items"`), `"currency": "EUR"}`, `"currency": "XXA"}`), 422,
			"receivables.unknown_currency", "currency"},
		{"an item in another currency", e.invoice(e.customer, "2026-03-10", itemOn(e.revenue, "1.00", "USD", "")), 422,
			"receivables.currency_mismatch", "items[0].net_amount.currency"},
		{"an amount with too many decimals", e.invoice(e.customer, "2026-03-10", e.item("1.001", "")), 400, "request.invalid_amount",
			"items[0].net_amount.amount"},
		{"an item of zero", e.invoice(e.customer, "2026-03-10", e.item("0.00", "")), 422, "receivables.amount_not_positive", "items[0].net_amount.amount"},
		{"a negative item", e.invoice(e.customer, "2026-03-10", e.item("-5.00", "")), 422, "receivables.amount_not_positive", "items[0].net_amount.amount"},
		{"no items", e.invoice(e.customer, "2026-03-10"), 400, "request.invalid_body", ""},
		{"amounts out of range", e.invoice(e.customer, "2026-03-10", e.item("90000000000000000.00", ""), e.item("90000000000000000.00", "")), 422,
			"receivables.amount_too_large", ""},
		{"tax out of range", e.invoice(e.customer, "2026-03-10", e.item("90000000000000000.00", "A1"), e.item("90000000000000000.00", "A1")), 422,
			"receivables.amount_too_large", ""},
		{"an item's tax out of range", e.invoice(e.customer, "2026-03-10", e.item("90000000000000000.00", "A2")), 422,
			"receivables.amount_too_large", ""},
		{"a date in no fiscal year", e.invoice(e.customer, "2019-03-10", e.item("1.00", "")), 422, "ledger.no_fiscal_period", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.In(t).Problem(c.status, c.code, "POST", "/v1/customer-invoices", c.body, "")
			if c.field != "" && field(m) != c.field {
				t.Errorf("field: got %v, want %s (%v)", field(m), c.field, m)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("invoices after the refusals: got %d, want none", n)
	}
}

// TestList: invoices are listed by customer and status, by date and
// document number, in pages, with their names.
func TestList(t *testing.T) {
	e := setup(t)
	second := e.partner(t, "BP70000003", "Contoso AG", "active", "customer", "supplier")
	var ids []string
	for _, c := range []struct {
		customer uuid.UUID
		date     string
	}{{e.customer, "2026-03-10"}, {second, "2026-03-11"}, {e.customer, "2026-03-12"}} {
		m, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(c.customer, c.date, e.item("10.00", "A1")), "")
		ids = append(ids, m["id"].(string))
	}
	list := func(t *testing.T, query string) []string {
		t.Helper()
		m, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices?"+query, "", "")
		var out []string
		for _, it := range items(t, m) {
			out = append(out, it["id"].(string))
		}
		return out
	}
	number := func(i int) string {
		var n string
		pgtest.Scan(t, `SELECT document_number FROM receivables.customer_invoice WHERE id = $1`, []any{ids[i]}, &n)
		return n
	}
	for _, c := range []struct {
		name, query string
		want        []string
	}{
		{"all, by invoice date", "", ids},
		{"a customer's", "customer_id=" + e.customer.String(), []string{ids[0], ids[2]}},
		{"a customer's drafts", "customer_id=" + second.String() + "&status=draft", []string{ids[1]}},
		{"another status", "status=posted", nil},
		{"a company's", "legal_entity_id=" + e.DE.String(), ids},
		{"another company's", "legal_entity_id=" + e.US.String(), nil},
		{"by document number", "document_number=" + number(1), []string{ids[1]}},
		{"from a date", "invoice_date_from=2026-03-11", []string{ids[1], ids[2]}},
		{"to a date", "invoice_date_to=2026-03-11", []string{ids[0], ids[1]}},
		{"newest first", "order_by=invoice_date%20desc", []string{ids[2], ids[1], ids[0]}},
		{"by number", "order_by=document_number", ids},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := list(t, c.query); strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		for _, order := range []string{"invoice_date", "document_number"} {
			first, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices?page_size=2&order_by="+order, "", "")
			token, _ := first["next_page_token"].(string)
			if len(items(t, first)) != 2 || token == "" {
				t.Fatalf("%s, first page: got %d items and token %q, want 2 and a token", order, len(items(t, first)), token)
			}
			rest, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices?page_size=2&order_by="+order+"&page_token="+token, "", "")
			if its := items(t, rest); len(its) != 1 || its[0]["id"] != ids[2] || rest["next_page_token"] != nil {
				t.Errorf("%s, second page: got %v, want the third invoice alone and no token", order, rest)
			}
		}
	})
	t.Run("with the names", func(t *testing.T) {
		m, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices?customer_id="+second.String(), "", "")
		it := items(t, m)[0]
		if it["customer_name"] != "Contoso AG" || it["customer_number"] != "BP70000003" || it["company_code"] != "2000" {
			t.Errorf("got %v %v %v, want Contoso AG BP70000003 2000", it["customer_name"], it["customer_number"], it["company_code"])
		}
	})
}

// TestCompanyAccessAndPermissions: an invoice of a company the caller may
// not act in is not found and not listed, and is not entered there;
// reading and entering need their permissions; an agent does not enter.
func TestCompanyAccessAndPermissions(t *testing.T) {
	e := setup(t)
	m, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("10.00", "A1")), "")
	path := "/v1/customer-invoices/" + m["id"].(string)
	elsewhere := e.As(e.Reader(t, "us-only", e.US))
	elsewhere.Problem(404, "request.not_found", "GET", path, "", "")
	if got, _ := elsewhere.Must(200, "GET", "/v1/customer-invoices", "", ""); len(items(t, got)) != 0 {
		t.Errorf("another company's list: got %v, want none", got["items"])
	}
	if p := elsewhere.Problem(422, "request.reference_not_found", "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("1.00", "")), ""); field(p) != "legal_entity_id" {
		t.Errorf("entering in another company: field %v, want legal_entity_id", field(p))
	}
	here := e.As(e.Reader(t, "de-only", e.DE))
	here.Must(200, "GET", path, "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/customer-invoices/"+uuid.NewString(), "", "")
	agent := e.As(e.Tenant.Agent)
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("1.00", "")), "")
	none := fixture.New(t)
	none.Problem(403, "identity.permission_missing", "GET", "/v1/customer-invoices", "", "")
	none.Problem(403, "identity.permission_missing", "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("1.00", "")), "")
}

// inRequest runs fn in a request of the tenant's person, rolled back.
func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}

// TestAssignments: an item's centers are checked as its posting will check
// them and read back with their codes (ADR-0050 §6).
func TestAssignments(t *testing.T) {
	e := setup(t)
	c := e.CostCenters(t, outputCodes(""))
	centered := func(cc, pc uuid.UUID) string {
		extra := ""
		if cc != uuid.Nil {
			extra += fmt.Sprintf(`, "cost_center_id": %q`, cc)
		}
		if pc != uuid.Nil {
			extra += fmt.Sprintf(`, "profit_center_id": %q`, pc)
		}
		return strings.Replace(e.item("100.00", "A1"), `, "tax_code"`, extra+`, "tax_code"`, 1)
	}
	got, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", centered(c.Sales, c.Profit), e.item("5.00", "")), "")
	its := items(t, got)
	for f, want := range map[string]any{"cost_center_id": c.Sales.String(), "cost_center_code": "2000", "profit_center_id": c.Profit.String(),
		"profit_center_code": "PC1"} {
		if its[0][f] != want {
			t.Errorf("item 1's %s: got %v, want %v", f, its[0][f], want)
		}
	}
	for _, f := range []string{"cost_center_id", "cost_center_code", "profit_center_id", "profit_center_code"} {
		if v, ok := its[1][f]; ok {
			t.Errorf("item 2's %s: got %v, want none", f, v)
		}
	}
	m := e.Problem(422, "ledger.cost_center_invalid", "POST", "/v1/customer-invoices",
		e.invoice(e.customer, "2026-03-10", e.item("5.00", ""), centered(uuid.New(), uuid.Nil)), "")
	if field(m) != "items[1].cost_center_id" {
		t.Errorf("an unknown cost center's field: got %v, want items[1].cost_center_id", field(m))
	}
}
