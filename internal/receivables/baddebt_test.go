package receivables_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Bad debt relief (ADR-0049 As built, bad debt relief): the output tax of
// a customer invoice a dispute case's write-off wrote off is recovered by
// an adjustment the return reports, under the conditions of the company's
// country.

// adjustment is an adjustment code of tax alone (kind adjustment, rate
// 0) with one component on the account, levying its lines' subdivision
// when onLine.
func adjustment(code, direction, key, jurisdiction, accountKey, account string, onLine bool) config.TaxCode {
	return config.TaxCode{Code: code, Description: code, Direction: direction, Kind: config.TaxAdjustment,
		Components: []config.TaxComponent{{Key: key, Jurisdiction: jurisdiction, JurisdictionOnLine: onLine, AccountKey: accountKey, Account: account,
			Rates: []config.TaxRate{{RatePercent: "0", ValidFrom: "2026-01-01"}}}}}
}

// reliefEnv is uaeEnv with the UAE tenant's adjustment codes: AS, output
// VAT levying the emirate (box 1's adjustment column), AX, output VAT
// levying none, and AP, input VAT (box 9's).
func reliefEnv(t *testing.T) discountEnv {
	t.Helper()
	return uaeEnv(t, func(c *config.Company) {
		c.TaxCodes = append(c.TaxCodes, adjustment("AS", config.TaxOutput, "vat", "AE", config.KeyOutput, "230000", true),
			adjustment("AX", config.TaxOutput, "vat", "AE", config.KeyOutput, "230000", false),
			adjustment("AP", config.TaxInput, "vat", "AE", config.KeyInput, "140000", false))
	})
}

// writtenOff opens a dispute case over the invoice's open item for the
// amount and resolves it by a write-off posted on the day, and returns the
// write-off's journal entry.
func (e discountEnv) writtenOff(t *testing.T, invoice map[string]any, amount, day string) string {
	t.Helper()
	c, tag := e.Must(201, "POST", "/v1/dispute-cases", fmt.Sprintf(`{"legal_entity_id": %q, "items": [{"fiscal_year": 2026, "journal_entry_id": "%v",
		"line_no": 1, "disputed_amount": {"amount": %q, "currency": %q}}], "reason": "The customer is insolvent"}`, e.le, invoice["journal_entry_id"],
		amount, e.currency), "")
	resolved, _ := e.Must(200, "POST", "/v1/dispute-cases/"+apitest.String(t, c, "id")+":resolve", fmt.Sprintf(`{"resolution": "write_off",
		"reason": "Uncollectable", "posting_date": %q}`, day), tag)
	return apitest.String(t, resolved, "write_off_journal_entry_id")
}

// relief is the JSON of a claim of the invoice's write-off under the
// code, with the notice and posting dates ("" for none).
func relief(invoice map[string]any, writeOff, code, notified, posted string) string {
	out := fmt.Sprintf(`{"customer_invoice_id": "%v", "write_off_journal_entry_id": %q, "tax_code": %q`, invoice["id"], writeOff, code)
	if notified != "" {
		out += fmt.Sprintf(`, "customer_notified_on": %q`, notified)
	}
	if posted != "" {
		out += fmt.Sprintf(`, "posting_date": %q`, posted)
	}
	return out + "}"
}

// TestUAEBadDebtRelief (acceptance): an invoice of AED 1,000.00 in Dubai
// and AED 500.00 in Abu Dhabi at 5%, 1,575.00 gross, dated 10 January
// and due on 9 February (SK14, 30 days net), is written off in full by a
// dispute case on 15 March. On 10 August, six months after its due date,
// with the customer notified on 1 August, its tax is recovered under AS:
// 50.00 of Dubai's VAT and 25.00 of Abu Dhabi's, debited on the output
// tax account with each emirate and credited to 680000, which the
// write-off debited. The third quarter's VAT 201 reports them in boxes 1b
// and 1a's adjustment columns, so the tax due (box 12) and payable (14)
// fall by 75.00. The relief reads back and lists by its invoice, is
// audited, and is not claimed twice.
func TestUAEBadDebtRelief(t *testing.T) {
	e := reliefEnv(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("1000.00", "S5", "AE-DU"), e.item("500.00", "S5", "AE-AZ"))
	writeOff := e.writtenOff(t, invoice, "1575.00", "2026-03-15")
	boxes := []string{"1a_vat", "1a_adjustment", "1b_vat", "1b_adjustment", "12", "14"}
	if got, want := e.boxes(t, "2026-07-01", boxes...), "1a_vat=25.00 1a_adjustment=0.00 1b_vat=50.00 1b_adjustment=0.00 12=75.00 14=75.00"; got != want {
		t.Fatalf("the third quarter's VAT 201 before the relief: got %s, want %s", got, want)
	}
	got, _ := e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff, "AS", "2026-08-01", "2026-08-10"), "")
	for field, want := range map[string]string{"written_off_amount": "1575.00 AED", "tax_amount": "75.00 AED", "local_written_off_amount": "1575.00 AED",
		"local_tax_amount": "75.00 AED"} {
		if a := amount(t, got, field); a != want {
			t.Errorf("%s: got %s, want %s", field, a, want)
		}
	}
	if s := fmt.Sprintf("%v %v %v %v %v %v", got["customer_invoice_number"], got["customer_number"], got["company_code"], got["tax_code"],
		got["customer_notified_on"], got["posting_date"]); s != fmt.Sprintf("%v BP70000003 5000 AS 2026-08-01 2026-08-10", invoice["document_number"]) {
		t.Errorf("the relief's names and dates: got %s", s)
	}
	if lines, want := e.jurisdictionLines(t, got["journal_entry_id"]), "230000 2500 AS AE-AZ; 230000 5000 AS AE-DU; 680000 -7500 - -"; lines != want {
		t.Errorf("the relief's lines: got %s, want %s", lines, want)
	}
	if its, want := e.taxItems(t, got["journal_entry_id"]), "AS AE-AZ 0 2500; AS AE-DU 0 5000"; its != want {
		t.Errorf("the relief's tax items: got %s, want %s", its, want)
	}
	after := "1a_vat=25.00 1a_adjustment=-25.00 1b_vat=50.00 1b_adjustment=-50.00 12=0.00 14=0.00"
	if b := e.boxes(t, "2026-07-01", boxes...); b != after {
		t.Errorf("the third quarter's VAT 201 after the relief: got %s, want %s", b, after)
	}
	id := apitest.String(t, got, "id")
	read, _ := e.Must(200, "GET", "/v1/bad-debt-reliefs/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/bad-debt-reliefs?customer_invoice_id=%v", invoice["id"]), "", "")
	if ids := items(t, page); len(ids) != 1 || ids[0]["id"] != id {
		t.Errorf("the invoice's reliefs: got %v, want %s alone", page["items"], id)
	}
	var audited string
	pgtest.Scan(t, `SELECT detail->'tax_amount'->>'amount' || ' ' || (detail->>'customer_notified_on') FROM audit.audit_event WHERE tenant_id = $1
		AND action = 'receivables.bad_debt_relief.claim' AND object_id = $2`, []any{e.Tenant.ID, id}, &audited)
	if audited != "75.00 2026-08-01" {
		t.Errorf("the relief audited: got %s, want 75.00 2026-08-01", audited)
	}
	if p := e.In(t).Problem(409, "receivables.bad_debt_relief_claimed", "POST", "/v1/bad-debt-reliefs",
		relief(invoice, writeOff, "AS", "2026-08-01", "2026-08-11"), ""); p["detail"] == nil {
		t.Errorf("the second claim: got %v, want its detail", p)
	}
}

// TestUAEBadDebtReliefRefusals: what a UAE claim is refused for, each
// naming its field; nothing is posted.
func TestUAEBadDebtReliefRefusals(t *testing.T) {
	e := reliefEnv(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("1000.00", "S5", "AE-DU"))
	writeOff := e.writtenOff(t, invoice, "1050.00", "2026-03-15")
	zero := e.invoiced(t, "2026-01-10", e.item("1000.00", "Z0", ""))
	zeroOff := e.writtenOff(t, zero, "1000.00", "2026-03-15")
	draft := map[string]any{"id": e.entered(t, fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-01-10", "currency": "AED",
		"items": [%s]}`, e.le, e.customer, e.item("10.00", "S5", "AE-DU")))}
	memo := map[string]any{"id": e.entered(t, fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "kind": "credit_memo", "invoice_date": "2026-01-10",
		"currency": "AED", "items": [%s]}`, e.le, e.customer, e.item("10.00", "S5", "AE-DU")))}
	cases := []struct {
		name, body string
		status     int
		code, at   string
	}{
		{"an unknown invoice", relief(map[string]any{"id": uuid.New()}, writeOff, "AS", "2026-08-01", "2026-08-10"), 422, "request.reference_not_found",
			"customer_invoice_id"},
		{"a draft", relief(draft, writeOff, "AS", "2026-08-01", "2026-08-10"), 409, "receivables.invalid_status", ""},
		{"a credit memo", relief(memo, writeOff, "AS", "2026-08-01", "2026-08-10"), 422, "receivables.bad_debt_not_invoice", "customer_invoice_id"},
		{"an entry that is no write-off of it", relief(invoice, zeroOff, "AS", "2026-08-01", "2026-08-10"), 422, "receivables.not_written_off",
			"write_off_journal_entry_id"},
		{"before the write-off", relief(invoice, writeOff, "AS", "2026-03-01", "2026-03-14"), 422, "receivables.relief_before_write_off", "posting_date"},
		{"within six months of the due date", relief(invoice, writeOff, "AS", "2026-08-01", "2026-08-08"), 422, "receivables.bad_debt_relief_too_early",
			"posting_date"},
		{"without the customer's notice", relief(invoice, writeOff, "AS", "", "2026-08-10"), 422, "receivables.customer_not_notified", "customer_notified_on"},
		{"notified after the relief", relief(invoice, writeOff, "AS", "2026-08-11", "2026-08-10"), 422, "receivables.invalid_notice_date",
			"customer_notified_on"},
		{"notified before the invoice", relief(invoice, writeOff, "AS", "2026-01-09", "2026-08-10"), 422, "receivables.invalid_notice_date",
			"customer_notified_on"},
		{"an invoice without output tax", relief(zero, zeroOff, "AS", "2026-08-01", "2026-08-10"), 422, "receivables.no_output_tax", "customer_invoice_id"},
		{"a standard code", relief(invoice, writeOff, "S5", "2026-08-01", "2026-08-10"), 422, "receivables.invalid_relief_tax_code", "tax_code"},
		{"an input adjustment code", relief(invoice, writeOff, "AP", "2026-08-01", "2026-08-10"), 422, "receivables.invalid_relief_tax_code", "tax_code"},
		{"a code levying no emirate", relief(invoice, writeOff, "AX", "2026-08-01", "2026-08-10"), 422, "receivables.invalid_relief_tax_code", "tax_code"},
		{"an unknown code", relief(invoice, writeOff, "QQ", "2026-08-01", "2026-08-10"), 422, "receivables.invalid_relief_tax_code", "tax_code"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(c.status, c.code, "POST", "/v1/bad-debt-reliefs", c.body, "")
			if c.at != "" && field(p) != c.at {
				t.Errorf("the field: got %v, want %s", field(p), c.at)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.bad_debt_relief WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("reliefs recorded: got %d, want 0", n)
	}
	pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE tenant_id = $1 AND legal_entity_id = $2 AND key = 'payment_difference_expense'`, e.Tenant.ID, e.le)
	e.In(t).Problem(422, "receivables.no_write_off_account", "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff, "AS", "2026-08-01", "2026-08-10"), "")
}

// TestCanadianBadDebtRelief: a Canadian company recovers the GST of what
// it wrote off under ETA s.231 at once, without a notice: of an invoice
// of CAD 100.00 with 5.00 GST, 60.00 written off recovers 2.86 (60.00 x
// 5.00 / 105.00), debited under GA, an input adjustment of the GST on
// 140000, so the GST34 reports it on line 107 and the net tax (109) falls
// by it. The notice is recorded when given; an output adjustment code
// would report it on line 104 and is refused.
func TestCanadianBadDebtRelief(t *testing.T) {
	e := newDiscountEnv(t, fixture.MapleCompany, func(c *config.Company, doc *config.Config) {
		c.TaxCodes = append(slices.Clone(c.TaxCodes), config.TaxCode{Code: "GO5", Description: "GST 5% on sales", Direction: config.TaxOutput,
			Components: []config.TaxComponent{{Key: "gst", Jurisdiction: "CA", AccountKey: config.KeyOutput, Account: "230000",
				Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}},
			adjustment("GA", config.TaxInput, "gst", "CA", config.KeyInput, "140000", false),
			adjustment("GI", config.TaxOutput, "gst", "CA", config.KeyOutput, "230000", false))
		c.TaxRegistrations = []config.TaxRegistration{{Code: "GSTHST", Authority: "CRA", Number: "123456782 RT 0001", ReturnType: "GST34",
			Frequency: config.TaxQuarterly, FirstPeriodStart: "2026-01-01"}}
		doc.Packs = append(doc.Packs, config.PackAdoption{Company: fixture.MapleCompany.Code, Country: "CA", Version: "2026.12", AppliedOn: "2026-01-01"})
	})
	invoice := e.invoiced(t, "2026-03-10", e.item("100.00", "GO5", ""))
	writeOff := e.writtenOff(t, invoice, "60.00", "2026-04-15")
	e.In(t).Problem(422, "receivables.invalid_relief_tax_code", "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff, "GI", "", "2026-04-15"), "")
	if got, want := e.boxes(t, "2026-04-01", "103", "107", "109"), "103=5.00 107=0.00 109=5.00"; got != want {
		t.Fatalf("the second quarter's GST34 before the relief: got %s, want %s", got, want)
	}
	got, _ := e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff, "GA", "", "2026-04-15"), "")
	if a := amount(t, got, "tax_amount"); a != "2.86 CAD" {
		t.Errorf("the GST recovered: got %s, want 2.86 CAD", a)
	}
	if n, ok := got["customer_notified_on"]; ok {
		t.Errorf("the notice: got %v, want none", n)
	}
	if lines, want := e.jurisdictionLines(t, got["journal_entry_id"]), "140000 286 GA -; 680000 -286 - -"; lines != want {
		t.Errorf("the relief's lines: got %s, want %s", lines, want)
	}
	if b, want := e.boxes(t, "2026-04-01", "103", "107", "109"), "103=5.00 107=2.86 109=2.14"; b != want {
		t.Errorf("the second quarter's GST34 after the relief: got %s, want %s", b, want)
	}
	other := e.invoiced(t, "2026-03-10", e.item("10.00", "GO5", ""))
	got, _ = e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(other, e.writtenOff(t, other, "10.50", "2026-04-15"), "GA", "2026-04-10", "2026-04-15"), "")
	if n := got["customer_notified_on"]; n != "2026-04-10" {
		t.Errorf("the notice given: got %v, want 2026-04-10", n)
	}
}

// TestBadDebtReliefElsewhere: a company of a country without a confirmed
// rule (company 2000, Germany) is refused, and one company's relief is not
// found nor listed from another.
func TestBadDebtReliefElsewhere(t *testing.T) {
	e := setup(t)
	posted := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-10")
	p := e.In(t).Problem(422, "receivables.bad_debt_relief_not_provided", "POST", "/v1/bad-debt-reliefs", relief(posted, uuid.New().String(), "A1", "", ""), "")
	if field(p) != "customer_invoice_id" {
		t.Errorf("the field: got %v, want customer_invoice_id", field(p))
	}
	u := reliefEnv(t)
	invoice := u.invoiced(t, "2026-01-10", u.item("100.00", "S5", "AE-DU"))
	got, _ := u.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, u.writtenOff(t, invoice, "105.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	reader := u.As(u.Reader(t, "Reader of 2000", u.DE))
	reader.Problem(404, "request.not_found", "GET", "/v1/bad-debt-reliefs/"+apitest.String(t, got, "id"), "", "")
	page, _ := reader.Must(200, "GET", "/v1/bad-debt-reliefs", "", "")
	if n := len(items(t, page)); n != 0 {
		t.Errorf("reliefs listed to a reader of 2000: got %d, want 0", n)
	}
	page, _ = u.Must(200, "GET", "/v1/bad-debt-reliefs?legal_entity_id="+u.le.String(), "", "")
	if n := len(items(t, page)); n != 1 {
		t.Errorf("the company's reliefs: got %d, want 1", n)
	}
	u.In(t).Problem(400, "request.invalid_page_token", "GET", "/v1/bad-debt-reliefs?page_token=x", "", "")
}

// TestBadDebtReliefDatabaseFailures claims, reads and lists a relief with
// each of their database statements failing in turn.
func TestBadDebtReliefDatabaseFailures(t *testing.T) {
	e := reliefEnv(t)
	h := endpoints(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	writeOff := uuid.MustParse(e.writtenOff(t, invoice, "105.00", "2026-03-15"))
	in := &gen.BadDebtReliefCreate{CustomerInvoiceID: uuid.MustParse(invoice["id"].(string)), WriteOffJournalEntryID: writeOff, TaxCode: "AS",
		CustomerNotifiedOn: gen.NewOptDate(date(t, "2026-08-01")), PostingDate: gen.NewOptDate(date(t, "2026-08-10"))}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "claim a bad debt relief", func(ctx context.Context) error {
		_, err := h.CreateBadDebtRelief(ctx, in, gen.CreateBadDebtReliefParams{})
		return err
	})
	got, _ := e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff.String(), "AS", "2026-08-01", "2026-08-10"), "")
	id := uuid.MustParse(got["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "read a bad debt relief", func(ctx context.Context) error {
		_, err := h.GetBadDebtRelief(ctx, gen.GetBadDebtReliefParams{ID: id})
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "list bad debt reliefs", func(ctx context.Context) error {
		_, err := h.ListBadDebtReliefs(ctx, gen.ListBadDebtReliefsParams{})
		return err
	})
}
