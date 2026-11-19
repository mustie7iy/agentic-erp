package receivables_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// foreignReceipt posts a bank statement line of house bank MAIN in the
// currency, as receipt does in EUR.
func (e env) foreignReceipt(t *testing.T, amount int64, currency, day string) ledger.Entry {
	t.Helper()
	return e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: e.DE, DocumentType: "SA", PostingDate: date(t, day), Currency: currency,
			BankAccountID: e.bank, Amount: money.Minor(amount), CounterAccountID: e.incoming, SourceContext: "payments", SourceDocumentID: uuid.New()})
	})
}

// rate records the average rate of the currency into EUR from the day.
func (e env) rate(t *testing.T, from, day, rate string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', $2, 'EUR', $3, $4, 'ECB', $5)`, e.Tenant.ID, from, day, rate, e.Tenant.Human.ID)
}

// owedIn posts an item the customer owes in the currency, debit positive,
// and returns it as the request names it.
func (e env) owedIn(t *testing.T, amount int64, currency, day string) string {
	t.Helper()
	customer := e.customer
	entry := e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date(t, day), PostingDate: date(t, day),
			Currency: currency, SourceContext: "receivables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: e.receivable, Amount: money.Minor(amount), PartnerID: &customer}, {AccountID: e.revenue, Amount: money.Minor(-amount)}}})
	})
	return itemRef(entry.ID, 1)
}

// withCurrencyResidual is an item reference with a residual of the amount
// in the currency for the reason.
func withCurrencyResidual(ref, amount, currency, reason string) string {
	return strings.TrimSuffix(ref, "}") + fmt.Sprintf(`, "residual": {"amount": {"amount": %q, "currency": %q}, "reason": %q}}`, amount, currency, reason)
}

// TestReceiptInAnotherCurrency (T75n acceptance): a receipt in GBP is
// applied to the customer's invoice in EUR, the company's currency, and to
// a second invoice with a residual: each item is translated into GBP at
// the day's rate and cleared; the payment shows each item's own amount and
// the rate beside its amount in GBP, and an item in GBP shows no rate. It
// reads back the same; an item whose currency has no rate into the
// receipt's that day (GBP into USD) is refused, naming it, and nothing
// posts.
func TestReceiptInAnotherCurrency(t *testing.T) {
	e := setupTolerance(t)
	e.rate(t, "GBP", "2026-03-01", "1.20")
	e.rate(t, "USD", "2026-03-01", "0.90")
	invoice, second := e.invoiced(t, e.customer, "120.00", "2026-03-10"), e.invoiced(t, e.customer, "60.00", "2026-03-10")
	receipt := e.foreignReceipt(t, 12000, "GBP", "2026-03-20")
	t.Run("an item without a rate that day", func(t *testing.T) {
		dollars, sterling := e.foreignReceipt(t, 13333, "USD", "2026-03-20"), e.owedIn(t, 10000, "GBP", "2026-03-10")
		got, _ := e.In(t).Must(422, "POST", "/v1/incoming-payments", e.payment(e.customer, dollars.ID, "2026-03-21", invoice, sterling), "")
		if got["code"] != "ledger.no_exchange_rate" || field(got) != "items[1]" {
			t.Errorf("got %v naming %v, want ledger.no_exchange_rate naming items[1]", got["code"], field(got))
		}
	})
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", invoice,
		withCurrencyResidual(second, "30.00", "GBP", "disputed")), "")
	if got["currency"] != "GBP" || amount(t, got, "amount") != "120.00 GBP" {
		t.Errorf("payment: got %v %s, want 120.00 GBP", got["currency"], amount(t, got, "amount"))
	}
	its := items(t, got)
	for i, want := range []struct{ amount, own, rate string }{{"100.00 GBP", "120.00 EUR", "0.833333333333"}, {"50.00 GBP", "60.00 EUR", "0.833333333333"}} {
		if a, o := amount(t, its[i], "amount"), amount(t, its[i], "item_amount"); a != want.amount || o != want.own || its[i]["exchange_rate"] != want.rate {
			t.Errorf("item %d: got %s, own %s at %v, want %s, own %s at %s", i+1, a, o, its[i]["exchange_rate"], want.amount, want.own, want.rate)
		}
	}
	if residual, _ := its[1]["residual"].(map[string]any); residual == nil || amount(t, residual, "amount") != "30.00 GBP" {
		t.Errorf("item 2's residual: got %v, want 30.00 GBP", its[1]["residual"])
	}
	read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String(), "", "")
	var open []string
	for _, it := range items(t, page) {
		open = append(open, amount(t, it, "amount")+" / "+amount(t, it, "amount_lc"))
	}
	slices.Sort(open)
	if got, want := strings.Join(open, ", "), "100.00 GBP / 120.00 EUR, 30.00 GBP / 36.00 EUR"; got != want {
		t.Errorf("the customer's open items: got %s, want the GBP item refused and the residual of 30.00 GBP at the invoice's rate", got)
	}
	t.Run("an item in the payment's currency shows no rate", func(t *testing.T) {
		pounds := e.foreignReceipt(t, 3000, "GBP", "2026-03-22")
		got, _ := e.In(t).Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, pounds.ID, "2026-03-22", itemRef(got["journal_entry_id"], 4)), "")
		it := items(t, got)[0]
		if _, shown := it["exchange_rate"]; shown || amount(t, it, "item_amount") != "30.00 GBP" {
			t.Errorf("item: got own %s at %v, want 30.00 GBP and no rate", amount(t, it, "item_amount"), it["exchange_rate"])
		}
	})
}
