package receivables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Bad debt relief charged again (ADR-0049 As built batch 91): what the
// customer later pays of a relieved invoice beyond what its write-offs
// left owed charges the relieved tax again, in proportion, and undoing
// that payment charges back less.

// paying is the JSON applying the receipt to the items on the day.
func (e discountEnv) paying(receipt any, day string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "receipt_journal_entry_id": "%v", "posting_date": %q, "items": [%s]}`,
		e.le, e.customer, receipt, day, strings.Join(items, ", "))
}

// leaving is the item's reference leaving the amount open as a residual
// item, reason short_payment.
func (e discountEnv) leaving(ref, amount string) string {
	return strings.TrimSuffix(ref, "}") + fmt.Sprintf(`, "residual": {"amount": {"amount": %q, "currency": %q}, "reason": "short_payment"}}`, amount, e.currency)
}

// recharges are the payment's charges again as "application reset
// recovered tax local".
func recharges(t *testing.T, payment map[string]any) string {
	t.Helper()
	var out []string
	for _, r := range payment["bad_debt_recharges"].([]any) {
		m := r.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %s %s %s", m["application_no"], m["reset"], amount(t, m, "recovered_amount"), amount(t, m, "tax_amount"),
			amount(t, m, "local_tax_amount")))
	}
	return strings.Join(out, "; ")
}

// lastRecharge is the entry of the payment's last charge again.
func lastRecharge(payment map[string]any) any {
	list := payment["bad_debt_recharges"].([]any)
	return list[len(list)-1].(map[string]any)["journal_entry_id"]
}

// TestUAEBadDebtRecharge (acceptance): the invoice of TestUAEBadDebtRelief
// (1,575.00 gross, 50.00 of Dubai's VAT and 25.00 of Abu Dhabi's), written
// off in full and its 75.00 relieved on 10 August, is paid in part on 15
// September: 525.00, a third, the rest left open as a residual item. A
// third of the relieved VAT, 25.00, is charged again under AS, credited on
// the output tax account per emirate, 16.67 and 8.33, and debited to
// 680000, so the third quarter's VAT 201 reports a third of the relief
// back in boxes 1b and 1a's adjustment columns. Paying the residual on 20
// September charges the other 50.00: the relief is wholly charged again.
// Undoing that payment charges back less, 50.00; the charges are audited
// and read back with their payments.
func TestUAEBadDebtRecharge(t *testing.T) {
	e := reliefEnv(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("1000.00", "S5", "AE-DU"), e.item("500.00", "S5", "AE-AZ"))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, e.writtenOff(t, invoice, "1575.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	boxes := []string{"1a_adjustment", "1b_adjustment", "12", "14"}
	first, _ := e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 52500, "2026-09-15").ID, "2026-09-15",
		e.leaving(itemRef(invoice["journal_entry_id"], 1), "1050.00")), "")
	if got, want := recharges(t, first), "0 false 525.00 AED 25.00 AED 25.00 AED"; got != want {
		t.Errorf("the first payment's charge: got %s, want %s", got, want)
	}
	if lines, want := e.jurisdictionLines(t, lastRecharge(first)), "230000 -833 AS AE-AZ; 230000 -1667 AS AE-DU; 680000 2500 - -"; lines != want {
		t.Errorf("the charge's lines: got %s, want %s", lines, want)
	}
	if b, want := e.boxes(t, "2026-07-01", boxes...), "1a_adjustment=-16.67 1b_adjustment=-33.33 12=25.00 14=25.00"; b != want {
		t.Errorf("the third quarter's VAT 201 after the first payment: got %s, want %s", b, want)
	}
	residual := itemRef(first["journal_entry_id"], int(items(t, first)[0]["residual"].(map[string]any)["line_no"].(float64)))
	second, tag := e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 105000, "2026-09-20").ID, "2026-09-20", residual), "")
	if got, want := recharges(t, second), "0 false 1575.00 AED 50.00 AED 50.00 AED"; got != want {
		t.Errorf("the second payment's charge: got %s, want %s", got, want)
	}
	if b, want := e.boxes(t, "2026-07-01", boxes...), "1a_adjustment=0.00 1b_adjustment=0.00 12=75.00 14=75.00"; b != want {
		t.Errorf("the third quarter's VAT 201 once paid in full: got %s, want %s", b, want)
	}
	undone, _ := e.Must(200, "POST", "/v1/incoming-payments/"+second["id"].(string)+":reset", `{"reason": "Applied to the wrong customer."}`, tag)
	if got, want := recharges(t, undone), "0 false 1575.00 AED 50.00 AED 50.00 AED; 0 true 525.00 AED -50.00 AED -50.00 AED"; got != want {
		t.Errorf("the undone payment's charges: got %s, want %s", got, want)
	}
	if b, want := e.boxes(t, "2026-07-01", boxes...), "1a_adjustment=-16.67 1b_adjustment=-33.33 12=25.00 14=25.00"; b != want {
		t.Errorf("the third quarter's VAT 201 after the undoing: got %s, want %s", b, want)
	}
	read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+second["id"].(string), "", "")
	if a, b := mustJSON(t, read["bad_debt_recharges"]), mustJSON(t, undone["bad_debt_recharges"]); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	var audited string
	pgtest.Scan(t, `SELECT string_agg(format('%s %s', detail->'tax_amount'->>'amount', detail->>'reset'), ', ' ORDER BY object_id)
		FROM audit.audit_event WHERE tenant_id = $1 AND action = 'receivables.bad_debt_recharge.post'`, []any{e.Tenant.ID}, &audited)
	if audited != "25.00 false, 50.00 false, -50.00 true" {
		t.Errorf("the charges audited: got %s, want 25.00 false, 50.00 false, -50.00 true", audited)
	}
}

// TestBadDebtRechargeOfWhatWasOwed: of an invoice of AED 105.00 written
// off by 35.00 and relieved, a payment of the 70.00 still owed, applied to
// the invoice's item with the write-off's credit, recovers nothing and
// charges nothing again; neither does a payment of another invoice of the
// customer. A relief of an invoice paid beyond what its write-offs leave is
// refused (422 receivables.bad_debt_paid); one of an invoice paid in part
// in US dollars (T75n) is refused, and so is a payment in dollars of a
// relieved invoice (422 receivables.relieved_invoice_other_currency).
func TestBadDebtRechargeOfWhatWasOwed(t *testing.T) {
	e := reliefEnv(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	writeOff := e.writtenOff(t, invoice, "35.00", "2026-03-15")
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff, "AS", "2026-08-01", "2026-08-10"), "")
	other := e.invoiced(t, "2026-01-10", e.item("10.00", "S5", "AE-DU"))
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 8050, "2026-09-15").ID, "2026-09-15",
		itemRef(invoice["journal_entry_id"], 1), itemRef(writeOff, 2), itemRef(other["journal_entry_id"], 1)), "")
	if r := recharges(t, got); r != "" {
		t.Errorf("a payment of what was owed: got the charges %s, want none", r)
	}
	paid := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	paidOff := e.writtenOff(t, paid, "35.00", "2026-03-15")
	e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 10500, "2026-04-01").ID, "2026-04-01", itemRef(paid["journal_entry_id"], 1)), "")
	p := e.In(t).Problem(422, "receivables.bad_debt_paid", "POST", "/v1/bad-debt-reliefs", relief(paid, paidOff, "AS", "2026-08-01", "2026-08-10"), "")
	if field(p) != "write_off_journal_entry_id" {
		t.Errorf("the field: got %v, want write_off_journal_entry_id", field(p))
	}
	// A receipt in US dollars, one dirham to the dollar (T75n).
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.25, 'test', $2),
			($1, gen_random_uuid(), 'average', 'USD', 'AED', '2026-01-01', 1, 'test', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	dollars := func(amount int64, day string) ledger.Entry { return e.receiptIn(t, amount, "USD", day) }
	foreign := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	foreignOff := e.writtenOff(t, foreign, "35.00", "2026-03-15")
	e.Must(201, "POST", "/v1/incoming-payments", e.paying(dollars(1000, "2026-04-01").ID, "2026-04-01",
		withCurrencyResidual(itemRef(foreign["journal_entry_id"], 1), "95.00", "USD", "short_payment")), "")
	e.In(t).Problem(422, "receivables.relieved_invoice_other_currency", "POST", "/v1/bad-debt-reliefs",
		relief(foreign, foreignOff, "AS", "2026-08-01", "2026-08-10"), "")
	relieved := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(relieved, e.writtenOff(t, relieved, "35.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	e.In(t).Problem(422, "receivables.relieved_invoice_other_currency", "POST", "/v1/incoming-payments", e.paying(dollars(1000, "2026-09-16").ID,
		"2026-09-16", withCurrencyResidual(itemRef(relieved["journal_entry_id"], 1), "95.00", "USD", "short_payment")), "")
}

// TestCanadianBadDebtRecharge: of an invoice of CAD 100.00 with 5.00 GST,
// 60.00 written off and its 2.86 relieved under GA; the customer's 105.00
// left on account and later applied to the invoice pays the 60.00 written
// off too, so the 2.86 is charged again under GA, credited on 140000, and
// the GST34's line 107 falls back to nothing. Undoing the application
// charges back less; without its payment differences account a company's
// charge is refused and nothing is applied.
func TestCanadianBadDebtRecharge(t *testing.T) {
	e := canadianReliefEnv(t)
	invoice := e.invoiced(t, "2026-03-10", e.item("100.00", "GO5", ""))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, e.writtenOff(t, invoice, "60.00", "2026-04-15"), "GA", "", "2026-04-15"), "")
	body := e.paying(e.receipt(t, 10500, "2026-05-01").ID, "2026-05-01")
	credit, tag := e.Must(201, "POST", "/v1/incoming-payments", body[:len(body)-1]+`, "on_account": true}`, "")
	if r := recharges(t, credit); r != "" {
		t.Errorf("a payment on account: got the charges %s, want none", r)
	}
	apply := fmt.Sprintf(`{"posting_date": "2026-05-02", "items": [%s]}`, itemRef(invoice["journal_entry_id"], 1))
	applied, tag := e.Must(201, "POST", "/v1/incoming-payments/"+credit["id"].(string)+"/applications", apply, tag)
	if got, want := recharges(t, applied), "1 false 60.00 CAD 2.86 CAD 2.86 CAD"; got != want {
		t.Errorf("the application's charge: got %s, want %s", got, want)
	}
	if lines, want := e.jurisdictionLines(t, lastRecharge(applied)), "140000 -286 GA -; 680000 286 - -"; lines != want {
		t.Errorf("the charge's lines: got %s, want %s", lines, want)
	}
	if b, want := e.boxes(t, "2026-04-01", "103", "107", "109"), "103=5.00 107=0.00 109=5.00"; b != want {
		t.Errorf("the second quarter's GST34 after the charge: got %s, want %s", b, want)
	}
	undone, _ := e.Must(200, "POST", "/v1/incoming-payments/"+credit["id"].(string)+"/applications/1:reset", `{"reason": "Wrong invoice."}`, tag)
	if got, want := recharges(t, undone), "1 false 60.00 CAD 2.86 CAD 2.86 CAD; 1 true 0.00 CAD -2.86 CAD -2.86 CAD"; got != want {
		t.Errorf("the undone application's charges: got %s, want %s", got, want)
	}
	pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE tenant_id = $1 AND legal_entity_id = $2 AND key = 'payment_difference_expense'`, e.Tenant.ID, e.le)
	again, _ := e.Must(200, "GET", "/v1/incoming-payments/"+credit["id"].(string), "", "")
	if again["version"] != float64(3) {
		t.Errorf("the payment's version: got %v, want 3", again["version"])
	}
	e.In(t).Problem(422, "receivables.no_write_off_account", "POST", "/v1/incoming-payments/"+credit["id"].(string)+"/applications", apply, "")
}

// TestBadDebtRechargeDatabaseFailures applies a payment of a relieved
// invoice, applies a credit on account to one and undoes it, with each of
// their database statements failing in turn.
func TestBadDebtRechargeDatabaseFailures(t *testing.T) {
	e := reliefEnv(t)
	h := endpoints(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, e.writtenOff(t, invoice, "105.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	ref := gen.IncomingPaymentItemRef{FiscalYear: 2026, JournalEntryID: uuid.MustParse(invoice["journal_entry_id"].(string)), LineNo: 1}
	receipt := e.receipt(t, 10500, "2026-09-15")
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "apply a payment of a relieved invoice", func(ctx context.Context) error {
		_, err := h.CreateIncomingPayment(ctx, &gen.IncomingPaymentCreate{LegalEntityID: e.le, CustomerID: e.customer, ReceiptJournalEntryID: receipt.ID,
			PostingDate: gen.NewOptDate(date(t, "2026-09-15")), Items: []gen.IncomingPaymentItemRef{ref}}, gen.CreateIncomingPaymentParams{})
		return err
	})
	body := e.paying(e.receipt(t, 10500, "2026-09-15").ID, "2026-09-15")
	credit, _ := e.Must(201, "POST", "/v1/incoming-payments", body[:len(body)-1]+`, "on_account": true}`, "")
	id := uuid.MustParse(credit["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "apply a credit on account to a relieved invoice", func(ctx context.Context) error {
		_, err := h.ApplyIncomingPaymentOnAccount(ctx, &gen.IncomingPaymentApplicationCreate{PostingDate: gen.NewOptDate(date(t, "2026-09-15")),
			Items: []gen.IncomingPaymentItemRef{ref}}, gen.ApplyIncomingPaymentOnAccountParams{ID: id})
		return err
	})
	applied, _ := e.Must(201, "POST", "/v1/incoming-payments/"+credit["id"].(string)+"/applications",
		fmt.Sprintf(`{"posting_date": "2026-09-15", "items": [%s]}`, itemRef(invoice["journal_entry_id"], 1)), "")
	if r := recharges(t, applied); r != "1 false 105.00 AED 5.00 AED 5.00 AED" {
		t.Fatalf("the application's charge: got %s, want 1 false 105.00 AED 5.00 AED 5.00 AED", r)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "undo an application that charged a relief again", func(ctx context.Context) error {
		_, err := h.ResetIncomingPaymentApplication(ctx, &gen.IncomingPaymentReset{Reason: "Wrong invoice."},
			gen.ResetIncomingPaymentApplicationParams{ID: id, ApplicationNo: 1, IfMatch: `"2"`})
		return err
	})
}

// receiptIn posts a statement line of the company's house bank in the
// currency, as receipt does in the company's.
func (e discountEnv) receiptIn(t *testing.T, amount int64, currency, day string) ledger.Entry {
	t.Helper()
	return e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: e.le, DocumentType: "SA", PostingDate: date(t, day), Currency: currency,
			BankAccountID: e.bank, Amount: money.Minor(amount), CounterAccountID: e.incoming, SourceContext: "payments", SourceDocumentID: uuid.New()})
	})
}

// TestBadDebtRechargeOfAnUnderpayment: of an invoice of AED 105.00 wholly
// written off and its 5.00 relieved, the customer pays 100.00, the 5.00
// short written off within the company's tolerance: what it paid is
// 100.00, so 4.76 of the VAT is charged again (5.00 x 100.00 / 105.00),
// not the whole; an underpayment beside a credit is shared over the
// debits alone.
func TestBadDebtRechargeOfAnUnderpayment(t *testing.T) {
	e := reliefEnv(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, e.writtenOff(t, invoice, "105.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 10000, "2026-09-15").ID, "2026-09-15",
		itemRef(invoice["journal_entry_id"], 1)), "")
	if a := amount(t, got, "difference"); a != "5.00 AED" {
		t.Fatalf("the payment's difference: got %s, want 5.00 AED", a)
	}
	if r, want := recharges(t, got), "0 false 100.00 AED 4.76 AED 4.76 AED"; r != want {
		t.Errorf("the charge: got %s, want %s", r, want)
	}
	// Applied beside a credit of 100.00, a receipt of 4.80 leaves 0.20
	// short: it is shared over the debits alone, so 104.80 was paid of the
	// invoice and 4.99 charged again (5.00 x 104.80 / 105.00).
	beside := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(beside, e.writtenOff(t, beside, "105.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	got, _ = e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 480, "2026-09-15").ID, "2026-09-15",
		itemRef(beside["journal_entry_id"], 1), e.credited(t, 10000, "2026-03-10")), "")
	if a := amount(t, got, "difference"); a != "0.20 AED" {
		t.Fatalf("the second payment's difference: got %s, want 0.20 AED", a)
	}
	if r, want := recharges(t, got), "0 false 104.80 AED 4.99 AED 4.99 AED"; r != want {
		t.Errorf("the charge beside a credit: got %s, want %s", r, want)
	}
}

// TestBadDebtRechargeInAnotherCurrency: an invoice in US dollars of the
// UAE company, USD 105.00 with 5.00 of VAT at 3.6725, wholly written off
// and relieved, is paid 104.90 and then the 0.10 left: the first payment
// charges the whole 5.00 again (5.00 x 104.90 / 105.00 rounds to it) with
// all of the local VAT the relief recovered, and the second nothing, so no
// dirham of it is left unrecovered.
func TestBadDebtRechargeInAnotherCurrency(t *testing.T) {
	e := reliefEnv(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'AED', '2026-01-01', 3.6725, 'test', $2),
			($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.9, 'test', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	usd := e
	usd.currency = "USD"
	invoice := usd.invoiced(t, "2026-01-10", usd.item("100.00", "S5", "AE-DU"))
	r, _ := e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, usd.writtenOff(t, invoice, "105.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	first, _ := e.Must(201, "POST", "/v1/incoming-payments", usd.paying(e.receiptIn(t, 10490, "USD", "2026-09-15").ID, "2026-09-15",
		usd.leaving(itemRef(invoice["journal_entry_id"], 1), "0.10")), "")
	if got, want := recharges(t, first), "0 false 104.90 USD 5.00 USD "+amount(t, r, "local_tax_amount"); got != want {
		t.Errorf("the first payment's charge: got %s, want %s", got, want)
	}
	residual := itemRef(first["journal_entry_id"], int(items(t, first)[0]["residual"].(map[string]any)["line_no"].(float64)))
	second, _ := e.Must(201, "POST", "/v1/incoming-payments", usd.paying(e.receiptIn(t, 10, "USD", "2026-09-16").ID, "2026-09-16", residual), "")
	if got := recharges(t, second); got != "" {
		t.Errorf("the second payment's charge: got %s, want none", got)
	}
}

// TestBadDebtReliefRacesAPayment: a relief and a payment of one customer
// run one after the other, in either order, without a deadlock. A payment
// of the whole invoice holding the customer makes a relief claimed
// meanwhile wait and then refuse it (422 receivables.bad_debt_paid), the
// invoice having no relief yet; a relief of a second write-off holding the
// customer and its invoice's row makes a payment of the invoice wait and
// then charge both reliefs' tax again (1.67 + 1.66).
func TestBadDebtReliefRacesAPayment(t *testing.T) {
	e := reliefEnv(t)
	h := endpoints(t)
	pay := func(receipt ledger.Entry, invoice map[string]any) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CreateIncomingPayment(ctx, &gen.IncomingPaymentCreate{LegalEntityID: e.le, CustomerID: e.customer, ReceiptJournalEntryID: receipt.ID,
				PostingDate: gen.NewOptDate(date(t, "2026-09-15")), Items: []gen.IncomingPaymentItemRef{{FiscalYear: 2026,
					JournalEntryID: uuid.MustParse(invoice["journal_entry_id"].(string)), LineNo: 1}}}, gen.CreateIncomingPaymentParams{})
			return err
		}
	}
	claim := func(invoice map[string]any, writeOff string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CreateBadDebtRelief(ctx, &gen.BadDebtReliefCreate{CustomerInvoiceID: uuid.MustParse(invoice["id"].(string)),
				WriteOffJournalEntryID: uuid.MustParse(writeOff), TaxCode: "AS", CustomerNotifiedOn: gen.NewOptDate(date(t, "2026-08-01")),
				PostingDate: gen.NewOptDate(date(t, "2026-09-15"))}, gen.CreateBadDebtReliefParams{})
			return err
		}
	}
	paid := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	off := e.writtenOff(t, paid, "35.00", "2026-03-15")
	first, second := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, pay(e.receipt(t, 10500, "2026-09-15"), paid), claim(paid, off))
	if p := problem.From(second); first != nil || p.Code != "receivables.bad_debt_paid" {
		t.Errorf("a payment, then a relief: got %v and %v, want the payment applied and the relief refused as receivables.bad_debt_paid", first, second)
	}
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, e.writtenOff(t, invoice, "35.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
	again := e.writtenOff(t, invoice, "35.00", "2026-03-16")
	receipt := e.receipt(t, 10500, "2026-09-15")
	first, second = apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, claim(invoice, again), pay(receipt, invoice))
	if first != nil || second != nil {
		t.Fatalf("a relief, then a payment: got %v and %v, want both done", first, second)
	}
	var charged string
	pgtest.Scan(t, `SELECT sum(tax_amount)::text FROM receivables.bad_debt_recharge WHERE tenant_id = $1 AND customer_invoice_id = $2`,
		[]any{e.Tenant.ID, invoice["id"]}, &charged)
	if charged != "333" {
		t.Errorf("the tax charged again: got %s, want 333 (both reliefs')", charged)
	}
}
