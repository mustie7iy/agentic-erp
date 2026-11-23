package receivables_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/packs"
)

// Cash discount granted (ADR-0063 As built; the product owner's answer of
// 2026-11-21): a UAE company's invoice paid within its discount period
// takes its discount, which reduces the output tax per emirate and so the
// VAT 201; a Canadian company's stays a payment difference without tax
// (ETA s.161).

// uaeCompany is Maple Gulf Trading, a UAE company in AED.
var uaeCompany = fixture.Company{Code: "5000", Name: "Maple Gulf Trading", Country: "AE", Currency: "AED"}

// discountEnv is a tenant with a company like 2000 (outputCodes) under
// the code, country and currency of co: its number ranges for customer
// invoices and incoming payments, payment differences on 680000,
// exchange differences on 420000 and 630000, underpayments within 5.00 of
// its currency and 5%, its currency's rate into the group's EUR, a customer
// set up on 120000 with the term SK14 (2% within 14 days, 30 net), and
// extend's changes to the company.
type discountEnv struct {
	env
	le                   uuid.UUID
	currency             string
	revenue, receivable  uuid.UUID
	bank, incoming, diff uuid.UUID
}

func newDiscountEnv(t *testing.T, co fixture.Company, extend func(*config.Company, *config.Config)) discountEnv {
	t.Helper()
	f := fixture.NewWith(t, []fixture.Company{co}, func(doc *config.Config) {
		outputCodes("")(doc)
		c := *doc.Company("2000")
		c.Code, c.Name, c.Currency, c.Country = co.Code, co.Name, co.Currency, co.Country
		c.ChartOfAccounts = append(slices.Clone(c.ChartOfAccounts), config.Account{Number: "680000", Name: "Payment differences and cash discounts", Type: config.Expense},
			config.Account{Number: "420000", Name: "Exchange gains", Type: config.Revenue}, config.Account{Number: "630000", Name: "Exchange losses", Type: config.Expense})
		c.AccountDetermination.PaymentDifferenceExpense = "680000"
		// The group currency is EUR: what rounding leaves there is an exchange difference.
		c.AccountDetermination.FXGain, c.AccountDetermination.FXLoss = "420000", "630000"
		c.Receivables.PaymentTolerance = config.PaymentTolerance{UnderpaymentAmount: "5.00", UnderpaymentPercent: "5"}
		for _, r := range doc.NumberRanges {
			if r.Company == "2000" && slices.Contains([]string{"journal_dr", "journal_dz", "journal_dg", "journal_dd", "incoming_payment"}, r.DocumentType) {
				r.Company = co.Code
				doc.NumberRanges = append(doc.NumberRanges, r)
			}
		}
		if extend != nil {
			extend(&c, doc)
		}
		doc.Companies = append(doc.Companies, c)
	}, append(slices.Clone(perms), "tax.registration.read", "tax.return.read")...)
	e := discountEnv{env: env{Env: f}, currency: co.Currency}
	pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = $2`, []any{f.Tenant.ID, co.Code}, &e.le)
	e.revenue, e.receivable, e.diff = f.Account(t, co.Code, "400000"), f.Account(t, co.Code, "120000"), f.Account(t, co.Code, "680000")
	e.bank, e.incoming = f.Account(t, co.Code, "102000"), f.Account(t, co.Code, "103000")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', $2, 'EUR', '2026-01-01', 0.25, 'test', $3)`, f.Tenant.ID, co.Currency, f.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, created_by, updated_by)
		VALUES ($1, 'SK14', '14 days 2%, 30 net', 30, 14, 2, $2, $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	e.customer = e.partner(t, "BP70000003", "Desert Rose Retail", "active", "customer")
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'SK14', $5, $5)`, f.Tenant.ID, e.customer, e.le, e.receivable, f.Tenant.Human.ID)
	return e
}

// uaeEnv is discountEnv for Maple Gulf Trading on the UAE pack's tax
// codes (its output tax on 230000, input tax on 140000), its quarterly VAT
// registration with the FTA filing the pack's VAT 201.
func uaeEnv(t *testing.T, extend func(*config.Company)) discountEnv {
	t.Helper()
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	const version = "2026.9"
	ae := lib.Get("AE", version)
	if ae == nil {
		t.Fatalf("Get(AE, %s): got nil, want the UAE pack", version)
	}
	return newDiscountEnv(t, uaeCompany, func(c *config.Company, doc *config.Config) {
		accounts := map[string]string{"1300": "140000", "2300": "230000"}
		c.TaxCodes = nil
		for _, code := range ae.Tax.TaxCodes {
			code.Components = slices.Clone(code.Components)
			for i := range code.Components {
				code.Components[i].Account = accounts[code.Components[i].Account]
			}
			c.TaxCodes = append(c.TaxCodes, code)
		}
		c.TimeZone = "Asia/Dubai"
		c.TaxRegistrations = []config.TaxRegistration{{Code: "VAT", Authority: "FTA", Number: "100000000000003", ReturnType: "VAT201",
			Frequency: config.TaxQuarterly, FirstPeriodStart: "2026-01-01"}}
		doc.Packs = append(doc.Packs, config.PackAdoption{Company: uaeCompany.Code, Country: "AE", Version: version, AppliedOn: "2026-01-01"})
		if extend != nil {
			extend(c)
		}
	})
}

// item is the JSON of an item on the revenue account in the company's
// currency, its code and, for a code levying it, its emirate ("" none).
func (e discountEnv) item(amount, code, emirate string) string {
	it := itemOn(e.revenue, amount, e.currency, code)
	if emirate != "" {
		it = in(it, emirate)
	}
	return it
}

// invoiced enters and posts an invoice to the customer dated and posted on
// the day and returns it.
func (e discountEnv) invoiced(t *testing.T, day string, items ...string) map[string]any {
	t.Helper()
	return e.post(t, e.entered(t, fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": %q, "currency": %q, "items": [%s]}`,
		e.le, e.customer, day, e.currency, strings.Join(items, ", "))), day)
}

// receipt posts a statement line of the company's house bank MAIN, as
// env.receipt does company 2000's.
func (e discountEnv) receipt(t *testing.T, amount int64, day string) ledger.Entry {
	t.Helper()
	text := "Bank statement " + day + " line 1"
	return e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: e.le, DocumentType: "SA", PostingDate: date(t, day), Currency: e.currency,
			BankAccountID: e.bank, Amount: money.Minor(amount), CounterAccountID: e.incoming, HeaderText: &text, SourceContext: "payments",
			SourceDocumentID: uuid.New()})
	})
}

// payment is the JSON applying the receipt to the items on 2026-03-21.
func (e discountEnv) payment(receipt any, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "receipt_journal_entry_id": "%v", "posting_date": "2026-03-21", "items": [%s]}`,
		e.le, e.customer, receipt, strings.Join(items, ", "))
}

// discounted is the item's reference asking for its cash discount.
func discounted(ref string) string { return strings.TrimSuffix(ref, "}") + `, "cash_discount": true}` }

// vat201 is the boxes of the company's VAT 201 draft for the first
// quarter of 2026 the test reads, as code=amount.
func (e discountEnv) vat201(t *testing.T, codes ...string) string {
	t.Helper()
	regs, _ := e.Must(200, "GET", "/v1/tax-registrations?legal_entity_id="+e.le.String(), "", "")
	reg := regs["items"].([]any)[0].(map[string]any)["id"].(string)
	draft, _ := e.Must(200, "GET", "/v1/tax-registrations/"+reg+"/draft-return?period_start=2026-01-01", "", "")
	got := map[string]string{}
	for _, b := range draft["boxes"].([]any) {
		box := b.(map[string]any)
		got[box["code"].(string)] = box["amount"].(map[string]any)["amount"].(string)
	}
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = c + "=" + got[c]
	}
	return strings.Join(out, " ")
}

// TestUAECashDiscount (acceptance): an invoice of AED 1,000.00 in Dubai
// and AED 500.00 in Abu Dhabi at 5% (S5) and AED 200.00 zero-rated (Z0),
// 1,775.00 gross, is paid on 20 March, within its 14 days, less its 2%
// discount: 35.50, of which 1.00 is Dubai's VAT and 0.50 Abu Dhabi's
// (each item's tax over the gross), the net 34.00 shared by the bases,
// 20.00, 10.00 and 4.00. The clearing entry debits them on 680000 and the
// output tax account, each with its code and emirate; its tax items reduce
// the VAT 201's boxes 1a, 1b and 4. Undoing the payment reverses them.
func TestUAECashDiscount(t *testing.T) {
	e := uaeEnv(t, nil)
	before := "1a_amount=500.00 1a_vat=25.00 1b_amount=1000.00 1b_vat=50.00 4_amount=200.00"
	invoice := e.invoiced(t, "2026-03-10", e.item("1000.00", "S5", "AE-DU"), e.item("500.00", "S5", "AE-AZ"), e.item("200.00", "Z0", ""))
	if a := amount(t, invoice, "gross_amount"); a != "1775.00 AED" {
		t.Fatalf("the invoice's gross: got %s, want 1775.00 AED", a)
	}
	if got := e.vat201(t, "1a_amount", "1a_vat", "1b_amount", "1b_vat", "4_amount"); got != before {
		t.Fatalf("the VAT 201 before the payment: got %s, want %s", got, before)
	}
	got, tag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.receipt(t, 173950, "2026-03-20").ID, discounted(itemRef(invoice["journal_entry_id"], 1))), "")
	for field, want := range map[string]string{"amount": "1739.50 AED", "cash_discount": "35.50 AED", "difference": "0.00 AED"} {
		if a := amount(t, got, field); a != want {
			t.Errorf("%s: got %s, want %s", field, a, want)
		}
	}
	if a := amount(t, items(t, got)[0], "cash_discount"); a != "35.50 AED" {
		t.Errorf("the item's cash discount: got %s, want 35.50 AED", a)
	}
	if a := amount(t, got["customers"].([]any)[0].(map[string]any), "amount"); a != "1739.50 AED" {
		t.Errorf("the customer's part: got %s, want 1739.50 AED", a)
	}
	// The lines' EUR amounts, each the receipt's rounded once, leave a
	// cent's rounding in group currency: an exchange difference on 420000.
	wantLines := "103000 173950 - -; 120000 -177500 - -; 680000 1000 S5 AE-AZ; 680000 2000 S5 AE-DU; 680000 400 Z0 -; 230000 50 S5 AE-AZ; 230000 100 S5 AE-DU; " +
		"420000 0 - -"
	if lines := e.jurisdictionLines(t, got["journal_entry_id"]); lines != wantLines {
		t.Errorf("the clearing entry's lines: got %s, want %s", lines, wantLines)
	}
	if its, want := e.taxItems(t, got["journal_entry_id"]), "S5 AE-AZ 1000 50; S5 AE-DU 2000 100; Z0 AE 400 0"; its != want {
		t.Errorf("the clearing entry's tax items: got %s, want %s", its, want)
	}
	after := "1a_amount=490.00 1a_vat=24.50 1b_amount=980.00 1b_vat=49.00 4_amount=196.00"
	if boxes := e.vat201(t, "1a_amount", "1a_vat", "1b_amount", "1b_vat", "4_amount"); boxes != after {
		t.Errorf("the VAT 201 after the discount: got %s, want %s", boxes, after)
	}
	read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	var audited, published string
	pgtest.Scan(t, `SELECT detail->'cash_discount'->>'amount' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'receivables.incoming_payment.apply'
		AND object_id = $2`, []any{e.Tenant.ID, got["id"]}, &audited)
	pgtest.Scan(t, `SELECT payload->'cash_discount'->>'amount' FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.receivables.incoming_payment_applied.v1'`, []any{e.Tenant.ID, got["id"]}, &published)
	if audited != "35.50" {
		t.Errorf("the cash discount audited: got %s, want 35.50", audited)
	}
	if published != "35.50" {
		t.Errorf("the cash discount published: got %s, want 35.50", published)
	}
	e.Must(200, "POST", "/v1/incoming-payments/"+got["id"].(string)+":reset", `{"reason": "Applied to the wrong customer."}`, tag)
	if boxes := e.vat201(t, "1a_amount", "1a_vat", "1b_amount", "1b_vat", "4_amount"); boxes != before {
		t.Errorf("the VAT 201 after the payment is undone: got %s, want %s", boxes, before)
	}
}

// credited posts a credit of the customer's on its receivables account,
// as env.credited does company 2000's, and returns its open item.
func (e discountEnv) credited(t *testing.T, amount int64, day string) string {
	t.Helper()
	entry := e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Post(ctx, ledger.Posting{LegalEntityID: e.le, DocumentType: "SA", DocumentDate: date(t, day), PostingDate: date(t, day),
			Currency: e.currency, SourceContext: "receivables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: e.receivable, Amount: money.Minor(-amount), PartnerID: &e.customer}, {AccountID: e.revenue, Amount: money.Minor(amount)}}})
	})
	return itemRef(entry.ID, 1)
}

// mixedCode is SM, an output code of two components: VAT at 5% on 230000
// levying the emirate each line names and a levy of 1% on 231000 that
// does not.
var mixedCode = config.TaxCode{Code: "SM", Description: "VAT and a levy", Direction: config.TaxOutput, Components: []config.TaxComponent{
	{Key: "vat", Jurisdiction: "AE", JurisdictionOnLine: true, AccountKey: config.KeyOutput, Account: "230000", Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}},
	{Key: "levy", Jurisdiction: "AE", AccountKey: config.KeyOutput, Account: "231000", Rates: []config.TaxRate{{RatePercent: "1", ValidFrom: "2026-01-01"}}}}}

// TestUAECashDiscountRefusals: only an invoice paid in full takes its
// discount, within its terms' discount period counted on the receipt's
// day, and only one whose codes the split can keep the emirates of (not
// SM, a component of which levies none); an application of a credit on
// account grants none; a company without a payment_difference_expense
// account grants none either. Each refusal names the item's field and
// posts nothing.
func TestUAECashDiscountRefusals(t *testing.T) {
	e := uaeEnv(t, func(c *config.Company) {
		c.ChartOfAccounts = append(c.ChartOfAccounts, config.Account{Number: "231000", Name: "Levy payable", Type: config.Liability})
		c.TaxCodes = append(c.TaxCodes, mixedCode)
	})
	invoice := func() string {
		return itemRef(e.invoiced(t, "2026-03-10", e.item("100.00", "S5", "AE-DU"))["journal_entry_id"], 1)
	}
	mixed := itemRef(e.invoiced(t, "2026-03-10", e.item("100.00", "SM", "AE-DU"))["journal_entry_id"], 1)
	// A payment left on account, whose credit an application would apply.
	body := e.payment(e.receipt(t, 5000, "2026-03-20").ID)
	credit, _ := e.Must(201, "POST", "/v1/incoming-payments", body[:len(body)-1]+`, "on_account": true}`, "")
	for _, c := range []struct {
		name, body, code, field string
	}{
		{"a credit of the customer's", e.payment(e.receipt(t, 8290, "2026-03-20").ID, discounted(invoice()), discounted(e.credited(t, 2000, "2026-03-10"))),
			"ledger.invalid_discount", "items[1].cash_discount"},
		{"an item paid in part", e.payment(e.receipt(t, 9290, "2026-03-20").ID, discounted(withResidual(invoice(), "10.00", "short_payment", ""))),
			"ledger.invalid_discount", "items[0].cash_discount"},
		{"a receipt after the discount period", e.payment(e.receipt(t, 10290, "2026-03-25").ID, discounted(invoice())), "ledger.no_cash_discount",
			"items[0].cash_discount"},
		{"a code levying the emirate in one component alone", e.payment(e.receipt(t, 10388, "2026-03-20").ID, discounted(mixed)), "ledger.invalid_discount",
			"items[0].cash_discount"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.payments(t)
			got, _ := e.Must(422, "POST", "/v1/incoming-payments", strings.ReplaceAll(c.body, `"EUR"`, `"AED"`), "")
			if got["code"] != c.code || field(got) != c.field {
				t.Errorf("got %v naming %v, want %s naming %s", got["code"], field(got), c.code, c.field)
			}
			if n := e.payments(t); n != before {
				t.Errorf("payments: got %d, want %d", n, before)
			}
		})
	}
	got, _ := e.Must(422, "POST", "/v1/incoming-payments/"+credit["id"].(string)+"/applications",
		fmt.Sprintf(`{"posting_date": "2026-03-21", "items": [%s]}`, discounted(invoice())), "")
	if got["code"] != "receivables.cash_discount_not_granted" || field(got) != "items[0].cash_discount" {
		t.Errorf("an application of a credit on account: got %v naming %v, want receivables.cash_discount_not_granted naming items[0].cash_discount",
			got["code"], field(got))
	}
	bare := uaeEnv(t, func(c *config.Company) {
		c.AccountDetermination.PaymentDifferenceExpense, c.Receivables.PaymentTolerance = "", config.PaymentTolerance{} // a tolerance needs the account
	})
	ref := itemRef(bare.invoiced(t, "2026-03-10", bare.item("100.00", "S5", "AE-DU"))["journal_entry_id"], 1)
	got, _ = bare.Must(422, "POST", "/v1/incoming-payments", bare.payment(bare.receipt(t, 10290, "2026-03-20").ID, discounted(ref)), "")
	if got["code"] != "ledger.no_payment_difference_account" {
		t.Errorf("without a payment_difference_expense account: got %v, want ledger.no_payment_difference_account", got["code"])
	}
}

// TestCanadianCashDiscount (ETA s.161, confirmed by the tax specialist on
// 2026-11-21): a Canadian company grants no cash discount that adjusts its
// GST: one asked for is refused, and what the customer takes off within
// the discount period, 2% of an invoice of CAD 100.00 with 5.00 GST, is a
// payment difference written off gross within the tolerance, without a tax
// code, so no tax item is written and the invoice's GST stays as levied
// (the lines' EUR amounts leave a cent's rounding in group currency, an
// exchange difference on 420000).
func TestCanadianCashDiscount(t *testing.T) {
	e := newDiscountEnv(t, fixture.MapleCompany, func(c *config.Company, _ *config.Config) {
		c.TaxCodes = append(slices.Clone(c.TaxCodes), config.TaxCode{Code: "GO5", Description: "GST 5% on sales", Direction: config.TaxOutput,
			Components: []config.TaxComponent{{Key: "gst", Jurisdiction: "CA", AccountKey: config.KeyOutput, Account: "230000",
				Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}})
	})
	invoice := e.invoiced(t, "2026-03-10", e.item("100.00", "GO5", ""))
	ref := itemRef(invoice["journal_entry_id"], 1)
	receipt := e.receipt(t, 10290, "2026-03-20").ID
	got, _ := e.Must(422, "POST", "/v1/incoming-payments", e.payment(receipt, discounted(ref)), "")
	if got["code"] != "receivables.cash_discount_not_granted" || field(got) != "items[0].cash_discount" {
		t.Errorf("a cash discount asked for: got %v naming %v, want receivables.cash_discount_not_granted naming items[0].cash_discount", got["code"], field(got))
	}
	got, _ = e.Must(201, "POST", "/v1/incoming-payments", e.payment(receipt, ref), "")
	if d, c := amount(t, got, "difference"), amount(t, got, "cash_discount"); d != "2.10 CAD" || c != "0.00 CAD" {
		t.Errorf("difference and cash discount: got %s and %s, want 2.10 CAD and 0.00 CAD", d, c)
	}
	if lines, want := e.jurisdictionLines(t, got["journal_entry_id"]), "103000 10290 - -; 120000 -10500 - -; 680000 210 - -; 420000 0 - -"; lines != want {
		t.Errorf("the clearing entry's lines: got %s, want %s", lines, want)
	}
	if its := e.taxItems(t, got["journal_entry_id"]); its != "" {
		t.Errorf("the clearing entry's tax items: got %s, want none", its)
	}
	if its, want := e.taxItems(t, invoice["journal_entry_id"]), "GO5 CA -10000 -500"; its != want {
		t.Errorf("the invoice's tax items: got %s, want %s", its, want)
	}
}

// TestCashDiscountDatabaseFailures applies a UAE payment taking its cash
// discount with each of its database statements failing in turn.
func TestCashDiscountDatabaseFailures(t *testing.T) {
	e := uaeEnv(t, nil)
	h := endpoints(t)
	receipt := e.receipt(t, 10290, "2026-03-20")
	owed := gen.IncomingPaymentItemRef{FiscalYear: 2026, JournalEntryID: entryOf(t, itemRef(e.invoiced(t, "2026-03-10", e.item("100.00", "S5", "AE-DU"))["journal_entry_id"], 1)),
		LineNo: 1, CashDiscount: gen.NewOptBool(true)}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "apply a payment taking its cash discount", func(ctx context.Context) error {
		_, err := h.CreateIncomingPayment(ctx, &gen.IncomingPaymentCreate{LegalEntityID: e.le, CustomerID: e.customer, ReceiptJournalEntryID: receipt.ID,
			PostingDate: gen.NewOptDate(date(t, "2026-03-21")), Items: []gen.IncomingPaymentItemRef{owed}}, gen.CreateIncomingPaymentParams{})
		return err
	})
}

// TestUAECashDiscountOfSome: of two invoices one receipt pays, the one in
// US dollars takes its discount and the dirham one does not. USD 105.00
// (100.00 in Dubai at 5%) is AED 385.61 at 3.6725; its 2%, 7.71, has
// 0.37 of Dubai's VAT (5.00 of 105.00) and 7.34 net. The receipt is
// 385.61 - 7.71 + 105.00 = 482.90.
func TestUAECashDiscountOfSome(t *testing.T) {
	e := uaeEnv(t, nil)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'AED', '2026-01-01', 3.6725, 'test', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.92, 'test', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	dollars := e.post(t, e.entered(t, fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-03-10", "currency": "USD", "items": [%s]}`,
		e.le, e.customer, in(itemOn(e.revenue, "100.00", "USD", "S5"), "AE-DU"))), "2026-03-10")
	dirhams := e.invoiced(t, "2026-03-10", e.item("100.00", "S5", "AE-AZ"))
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.receipt(t, 48290, "2026-03-20").ID, discounted(itemRef(dollars["journal_entry_id"], 1)),
		itemRef(dirhams["journal_entry_id"], 1)), "")
	if c, d := amount(t, got, "cash_discount"), amount(t, got, "difference"); c != "7.71 AED" || d != "0.00 AED" {
		t.Errorf("cash discount and difference: got %s and %s, want 7.71 AED and 0.00 AED", c, d)
	}
	its := items(t, got)
	if a := amount(t, its[0], "cash_discount"); a != "7.71 AED" {
		t.Errorf("the dollar invoice's cash discount: got %s, want 7.71 AED", a)
	}
	if d, ok := its[1]["cash_discount"]; ok {
		t.Errorf("the dirham invoice's cash discount: got %v, want none", d)
	}
	var lines string
	pgtest.Scan(t, `SELECT string_agg(format('%s %s %s', a.number, l.amount_tc, l.tax_jurisdiction), '; ' ORDER BY l.line_no)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2 AND l.tax_code IS NOT NULL`, []any{e.Tenant.ID, got["journal_entry_id"]}, &lines)
	if want := "680000 734 AE-DU; 230000 37 AE-DU"; lines != want {
		t.Errorf("the discount's lines: got %s, want %s", lines, want)
	}
	if its, want := e.taxItems(t, got["journal_entry_id"]), "S5 AE-DU 734 37"; its != want {
		t.Errorf("the clearing entry's tax items: got %s, want %s", its, want)
	}
}
