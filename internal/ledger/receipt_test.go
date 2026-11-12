package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// withIncoming gives company 2000 the open-item managed account 103000, a
// house bank's incoming payments clearing account (ADR-0047).
func withIncoming(doc *config.Config) *config.Config {
	de := &doc.Companies[1]
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "103000", Name: "Bank incoming", Type: config.Asset, OpenItems: true})
	return doc
}

// receipts is a tenant whose company 2000 has the receivables account
// 120000 and the incoming account 103000.
type receipts struct {
	env
	bank, incoming uuid.UUID
}

func receiptSetup(t *testing.T, doc func(testing.TB) *config.Config) receipts {
	t.Helper()
	e := setup(t)
	if err := apply(t, e, withIncoming(doc(t))); err != nil {
		t.Fatal(err)
	}
	return receipts{env: e, bank: settleAccount(t, e, "2000", "100000"), incoming: settleAccount(t, e, "2000", "103000")}
}

// committed runs fn in a committed request of the tenant's human.
func (r receipts) committed(t *testing.T, fn func(ctx context.Context) (ledger.Entry, error)) ledger.Entry {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
		entry, err = fn(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entry
}

// received posts a statement line as payments does: the amount (debit
// positive on the bank account 100000) against the incoming account; the
// entry is the receipt.
func (r receipts) received(t *testing.T, amount int64, currency, day string) ledger.ItemRef {
	t.Helper()
	entry := r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: r.de, DocumentType: "SA", PostingDate: date(day), Currency: currency,
			BankAccountID: r.bank, Amount: money.Minor(amount), CounterAccountID: r.incoming, SourceContext: "payments", SourceDocumentID: uuid.New()})
	})
	return ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID}
}

// owed posts a customer document in the currency, its amount debit
// positive, and returns its open item.
func (r receipts) owed(t *testing.T, customer uuid.UUID, amount int64, currency, day string) ledger.ItemRef {
	t.Helper()
	in := customerPosting(t, r.env, customer, amount, day, nil)
	in.Currency = currency
	entry := r.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Post(ctx, in) })
	return ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: 1}
}

// application applies the receipt to the customer's items on the day.
func (r receipts) application(receipt ledger.ItemRef, customer uuid.UUID, currency, day string, items ...ledger.ItemRef) ledger.ReceiptApplication {
	text := "Incoming payment"
	return ledger.ReceiptApplication{LegalEntityID: r.de, DocumentType: "SA", PostingDate: date(day), Currency: currency, ReceiptAccountID: r.incoming,
		Receipt: receipt, CustomerID: customer, Items: items, HeaderText: &text, SourceContext: "receivables", SourceDocumentID: uuid.New()}
}

// refused applies in a request that is rolled back and returns the
// refusal's code and detail.
func (r receipts) refused(t *testing.T, in ledger.ReceiptApplication) (code, detail string) {
	t.Helper()
	p := r.refusal(t, in)
	return p.Code, p.Detail
}

// refusal applies in a request that is rolled back and returns the
// refusal.
func (r receipts) refusal(t *testing.T, in ledger.ReceiptApplication) *problem.Problem {
	t.Helper()
	err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) error {
		_, err := postings().ApplyReceipt(ctx, in)
		return err
	})
	if err == nil {
		t.Fatal("got the receipt applied, want a refusal")
	}
	return problem.From(err)
}

// posted is the entry's lines of the leading ledger as the tests compare
// them: account number and the three amounts, by line.
func posted(t *testing.T, e env, entry uuid.UUID) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_tc || '/' || l.amount_lc || '/' || l.amount_gc, '; ' ORDER BY l.line_no)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{e.ten.ID, entry}, &out)
	return out
}

// TestApplyReceipt (T75c, ADR-0063 §6): a receipt on the incoming account
// is applied to a customer's invoices and a credit of the customer's that
// together are the receipt; one entry offsets the receipt and each item at
// its amounts and clears them all; the customer has no open item left and
// the incoming account no open line.
func TestApplyReceipt(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	receipt := r.received(t, 12000, "EUR", "2026-03-20")
	first, second, credit := r.owed(t, north, 10000, "EUR", "2026-03-01"), r.owed(t, north, 5000, "EUR", "2026-03-02"),
		r.owed(t, north, -3000, "EUR", "2026-03-05")
	in := r.application(receipt, north, "EUR", "2026-03-21", first, second, credit)
	ref := "IP100001"
	in.Reference = &ref
	var applied ledger.AppliedReceipt
	if err := apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
		applied, err = postings().ApplyReceipt(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if applied.Receipt != 12000 || fmt.Sprint([]money.Minor(applied.Items)) != fmt.Sprint([]money.Minor{10000, 5000, -3000}) {
		t.Errorf("applied: got a receipt of %d and items %v, want 12000 and [10000 5000 -3000]", applied.Receipt, applied.Items)
	}
	entry := applied.Entry
	if got, want := posted(t, r.env, entry.ID), "103000 12000/12000/12000; 120000 -10000/-10000/-10000; 120000 -5000/-5000/-5000; 120000 3000/3000/3000"; got != want {
		t.Errorf("the clearing entry's lines: got %s, want %s", got, want)
	}
	if entry.SourceContext != "receivables" || entry.SourceDocumentID == nil || *entry.SourceDocumentID != in.SourceDocumentID || entry.DocumentType != "SA" ||
		entry.Reference == nil || *entry.Reference != ref || entry.PostingDate != date("2026-03-21") {
		t.Errorf("the clearing entry: got source %s %v, type %s, reference %v, posted %s; want receivables' document, SA, %s, 2026-03-21",
			entry.SourceContext, entry.SourceDocumentID, entry.DocumentType, entry.Reference, entry.PostingDate, ref)
	}
	for i, l := range entry.Lines[1:] {
		if l.PartnerID == nil || *l.PartnerID != north {
			t.Errorf("line %d's partner: got %v, want the customer", i+2, l.PartnerID)
		}
	}
	wantCleared(t, map[string]string{receipt.EntryID.String(): "2", first.EntryID.String(): "1", second.EntryID.String(): "1", credit.EntryID.String(): "1",
		entry.ID.String(): "1,2,3,4"}, map[string]string{receipt.EntryID.String(): "the receipt", first.EntryID.String(): "the first invoice",
		second.EntryID.String(): "the second invoice", credit.EntryID.String(): "the credit", entry.ID.String(): "the clearing entry"})
	if open := receivable(t, r.env, north); len(open) != 0 {
		t.Errorf("the customer's open items: got %d, want none", len(open))
	}
	var lines []ledger.OpenLine
	if err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
		lines, err = ledger.Accounts{}.OpenLines(ctx, r.de, []uuid.UUID{r.incoming})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("open lines on the incoming account: got %d, want none", len(lines))
	}
	var clearings, events int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.clearing.create' AND detail->>'clearing_entry_id' = $2),
			(SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1 AND event_type LIKE '%.items_cleared.%' AND payload->>'clearing_entry_id' = $2)`,
		[]any{r.ten.ID, entry.ID.String()}, &clearings, &events)
	if clearings != 1 || events != 1 {
		t.Errorf("the clearing's audit records and items_cleared events: got %d and %d, want 1 and 1", clearings, events)
	}
	t.Run("applied twice", func(t *testing.T) {
		if code, _ := r.refused(t, in); code != "ledger.item_already_cleared" {
			t.Errorf("got %s, want ledger.item_already_cleared", code)
		}
	})
	t.Run("outside a unit of work and failing statements", func(t *testing.T) {
		again := r.application(r.received(t, 700, "EUR", "2026-03-22"), north, "EUR", "2026-03-23", r.owed(t, north, 700, "EUR", "2026-03-06"))
		if _, err := postings().ApplyReceipt(context.Background(), again); err == nil {
			t.Error("outside a unit of work: got nil error, want an error")
		}
		apitest.EveryFailure(t, r.ten, r.ten.Human, "apply a receipt", func(ctx context.Context) error {
			_, err := postings().ApplyReceipt(ctx, again)
			return err
		})
		short := again
		short.Items = []ledger.ItemRef{r.owed(t, north, 600, "EUR", "2026-03-07")}
		apitest.EveryFailure(t, r.ten, r.ten.Human, "apply a receipt to items that are not it", func(ctx context.Context) error {
			_, err := postings().ApplyReceipt(ctx, short)
			if problem.From(err).Code == "ledger.clearing_not_balanced" {
				return nil // every statement ran: the refusal is the call's answer
			}
			return err
		})
	})
}

// TestApplyReceiptRefusals: what is not a receipt, and items that are not
// the customer's open items in the legal entity and the receipt's
// currency, or do not total the receipt, are refused and nothing posts.
func TestApplyReceiptRefusals(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north, south := r.newCustomer(t, "BP00000031", true), r.newCustomer(t, "BP00000032", true)
	settleRate(t, r.env, "GBP", "2026-03-01", "1.20")
	settleRate(t, r.env, "USD", "2026-03-01", "0.90")
	receipt := r.received(t, 10000, "EUR", "2026-03-20")
	invoice, souths := r.owed(t, north, 10000, "EUR", "2026-03-01"), r.owed(t, south, 10000, "EUR", "2026-03-01")
	sterling, small := r.owed(t, north, 10000, "GBP", "2026-03-01"), r.owed(t, north, 9999, "EUR", "2026-03-02")
	paid, credit := r.owed(t, north, 400, "EUR", "2026-03-03"), r.owed(t, north, -400, "EUR", "2026-03-03")
	r.Must(201, "POST", "/v1/clearings", clearing(r.de, "2026-03-31", ref{map[string]any{"fiscal_year": 2026, "id": paid.EntryID.String()}, 1},
		ref{map[string]any{"fiscal_year": 2026, "id": credit.EntryID.String()}, 1}), "")
	reversed, held := r.owed(t, north, 10000, "EUR", "2026-03-04"), r.owed(t, north, 10000, "EUR", "2026-03-05")
	tiny := r.owed(t, north, 1, "EUR", "2026-03-06") // 0.01: with the item of 99.99 it is the receipt
	r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Reverse(ctx, reversed.EntryID, nil, nil)
	})
	if err := apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) error {
		return postings().Reserve(ctx, ledger.Owner{Context: "payments", ID: uuid.New(), Reference: "PR1"}, []ledger.ItemRef{held})
	}); err != nil {
		t.Fatal(err)
	}
	suppliers := r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Post(ctx, termsPosting(t, r.env, nil, &r.supplier))
	})
	elsewhere := settlePost(t, r.env, r.us, "1000", "USD", 10000, "INV-US")
	// Not receipts: a payment out (a debit on the incoming account), an
	// entry with two lines there, one with none, a reversed one.
	out := r.received(t, -10000, "EUR", "2026-03-20")
	twice := r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Post(ctx, ledger.Posting{LegalEntityID: r.de, DocumentType: "SA", DocumentDate: date("2026-03-20"), PostingDate: date("2026-03-20"),
			Currency: "EUR", SourceContext: "payments", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{{AccountID: r.bank, Amount: 10000},
				{AccountID: r.incoming, Amount: -6000}, {AccountID: r.incoming, Amount: -4000}}})
	})
	undone := r.received(t, 10000, "EUR", "2026-03-20")
	r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Reverse(ctx, undone.EntryID, nil, nil)
	})
	app := func(receipt ledger.ItemRef, customer uuid.UUID, items ...ledger.ItemRef) ledger.ReceiptApplication {
		return r.application(receipt, customer, "EUR", "2026-03-21", items...)
	}
	onBank, elsewhereLE, unknownLE := app(receipt, north, invoice), app(receipt, north, invoice), app(receipt, north, invoice)
	onBank.ReceiptAccountID, elsewhereLE.LegalEntityID, unknownLE.LegalEntityID = r.bank, r.us, uuid.New()
	for _, c := range []struct {
		name   string
		in     ledger.ReceiptApplication
		code   string
		detail string
	}{
		{"no items", app(receipt, north), "ledger.no_items", ""},
		{"an item twice", app(receipt, north, invoice, invoice), "ledger.duplicate_item", ""},
		{"less than the receipt", app(receipt, north, small), "ledger.clearing_not_balanced", "receipt of 100.00 EUR: they differ from it by -0.01"},
		{"more than the receipt", app(receipt, north, invoice, small), "ledger.clearing_not_balanced", "differ from it by 99.99"},
		{"another customer's item", app(receipt, north, souths), "ledger.not_the_customers_item", "Item 1"},
		{"a supplier's item", app(receipt, r.supplier, ledger.ItemRef{FiscalYear: 2026, EntryID: suppliers.ID, LineNo: 2}), "ledger.not_the_customers_item", ""},
		{"another company's item", app(receipt, north, elsewhere), "ledger.not_an_open_item", "Item 1"},
		{"a revenue line", app(receipt, north, ledger.ItemRef{FiscalYear: 2026, EntryID: invoice.EntryID, LineNo: 2}), "ledger.not_an_open_item", ""},
		{"a cleared item", app(receipt, north, invoice, paid), "ledger.item_already_cleared", "Item 2"},
		{"a reversed item", app(receipt, north, reversed), "ledger.entry_reversed", "Item 1"},
		{"a reserved item", app(receipt, north, held), "ledger.item_reserved", "PR1"},
		{"an item in another currency", app(receipt, north, invoice, sterling), "ledger.mixed_items", "Item 2 is in GBP, the receipt in EUR"},
		{"a receipt in another currency than the request says", r.application(receipt, north, "GBP", "2026-03-21", sterling), "ledger.mixed_items", "settlement"},
		{"a payment out", app(out, north, invoice), "ledger.not_a_receipt", "debits the account"},
		{"two lines on the account", app(ledger.ItemRef{FiscalYear: 2026, EntryID: twice.ID}, north, invoice), "ledger.not_a_receipt", "has 2 lines"},
		{"no line on the account", app(invoice, north, invoice), "ledger.not_an_open_item", "has no line on the clearing account"},
		{"a reversed receipt", app(undone, north, invoice), "ledger.entry_reversed", "The receipt's journal entry"},
		{"an account that is not open-item managed", onBank, "ledger.not_open_item_managed", ""},
		{"another company", elsewhereLE, "ledger.not_open_item_managed", ""},
		{"an unknown company", unknownLE, "request.reference_not_found", ""},
		{"a date without a fiscal period", r.application(receipt, north, "EUR", "2031-03-21", invoice), "ledger.no_fiscal_period", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, detail := r.refused(t, c.in)
			if code != c.code {
				t.Errorf("got %s (%s), want %s", code, detail, c.code)
			}
			if !strings.Contains(detail, c.detail) {
				t.Errorf("detail: got %q, want it to contain %q", detail, c.detail)
			}
		})
	}
	// Every refusal of an item names the request's own field.
	for name, c := range map[string]struct {
		in    ledger.ReceiptApplication
		field string
	}{
		"another customer's": {app(receipt, north, souths), "items[0]"}, "another company's": {app(receipt, north, elsewhere), "items[0]"},
		"cleared": {app(receipt, north, invoice, paid), "items[1]"}, "reversed": {app(receipt, north, reversed), "items[0]"},
		"reserved": {app(receipt, north, small, held), "items[1]"}, "in another currency": {app(receipt, north, invoice, sterling), "items[1]"},
	} {
		t.Run("the field of an item "+name, func(t *testing.T) {
			p := r.refusal(t, c.in)
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field || p.Errors[0].Message == "" {
				t.Errorf("%s: got field errors %+v, want one naming %s with a message", p.Code, p.Errors, c.field)
			}
		})
	}
	// An account that can no longer be posted is refused by what the caller
	// named, not by the entry's line: the receipt, or the item.
	block := func(account uuid.UUID, blocked bool) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND account_id = $4`, r.ten.ID, r.de, blocked, account)
	}
	for name, c := range map[string]struct {
		account       uuid.UUID
		detail, field string
	}{
		"the receipt's account blocked": {r.incoming, "The receipt's account cannot be posted", ""},
		"an item's account blocked":     {settleAccount(t, r.env, "2000", "120000"), "Item 1's account cannot be posted", "items[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			block(c.account, true)
			defer block(c.account, false)
			p := r.refusal(t, app(receipt, north, tiny, small))
			field := ""
			if len(p.Errors) == 1 {
				field = p.Errors[0].Field
			}
			if p.Code != "ledger.account_not_postable" || !strings.Contains(p.Detail, c.detail) || field != c.field || len(p.Errors) > 1 {
				t.Errorf("got %s (%s) with field errors %+v, want ledger.account_not_postable, %q, field %q", p.Code, p.Detail, p.Errors, c.detail, c.field)
			}
		})
	}
	t.Run("a closed period", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND period = 3`, r.ten.ID, r.de)
		if code, _ := r.refused(t, app(receipt, north, invoice)); code != "ledger.period_not_open" {
			t.Errorf("got %s, want ledger.period_not_open", code)
		}
	})
	if open := receivable(t, r.env, north); len(open) != 5 {
		t.Errorf("the customer's open items after the refusals: got %d, want the 5 posted and neither cleared nor reversed", len(open))
	}
}

// TestApplyReceiptExchangeDifference: a receipt booked at another rate
// than the invoice clears it at each one's own amounts, and what is left
// in local and in group currency is a realized exchange difference: a loss
// when less came in than was booked, a gain when more did; without the
// account for it the application is refused.
func TestApplyReceiptExchangeDifference(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	settleRate(t, r.env, "GBP", "2026-03-01", "1.20")
	settleRate(t, r.env, "GBP", "2026-04-01", "1.10")
	settleRate(t, r.env, "GBP", "2026-05-01", "1.30")
	gain, loss := settleAccount(t, r.env, "2000", "400000"), settleAccount(t, r.env, "2000", "520000")
	for _, c := range []struct {
		name, day, key string
		account        uuid.UUID
		want           string
	}{
		{"a loss", "2026-04-10", "fx_loss", loss, "103000 10000/11000/11000; 120000 -10000/-12000/-12000; 520000 0/1000/0; 520000 0/0/1000"},
		{"a gain", "2026-05-10", "fx_gain", gain, "103000 10000/13000/13000; 120000 -10000/-12000/-12000; 400000 0/-1000/0; 400000 0/0/-1000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := r.application(r.received(t, 10000, "GBP", c.day), north, "GBP", c.day, r.owed(t, north, 10000, "GBP", "2026-03-10"))
			if code, _ := r.refused(t, in); code != "ledger.no_fx_account" {
				t.Fatalf("without the %s account: got %s, want ledger.no_fx_account", c.key, code)
			}
			pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, $3, $4, $5)`,
				r.ten.ID, r.de, c.key, c.account, r.ten.Human.ID)
			var applied ledger.AppliedReceipt
			if err := apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
				applied, err = postings().ApplyReceipt(ctx, in)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got := posted(t, r.env, applied.Entry.ID); got != c.want {
				t.Errorf("lines: got %s, want %s", got, c.want)
			}
		})
	}
	if open := receivable(t, r.env, north); len(open) != 0 {
		t.Errorf("the customer's open items: got %d, want none", len(open))
	}
	t.Run("the exchange difference's account blocked", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND account_id = $3`, r.ten.ID, r.de, loss)
		p := r.refusal(t, r.application(r.received(t, 10000, "GBP", "2026-04-11"), north, "GBP", "2026-04-11", r.owed(t, north, 10000, "GBP", "2026-03-10")))
		if p.Code != "ledger.account_not_postable" || !strings.Contains(p.Detail, "exchange difference's account (fx_gain or fx_loss") || len(p.Errors) != 0 {
			t.Errorf("got %s (%s) with field errors %+v, want ledger.account_not_postable naming the exchange difference's account and no field", p.Code, p.Detail, p.Errors)
		}
	})
}

// TestApplyReceiptParallelLedgers: the application posts to every ledger
// the receipt and the items were posted in and clears their copies there
// (ADR-0045); a parallel ledger's copy of a receipt is no receipt.
func TestApplyReceiptParallelLedgers(t *testing.T) {
	r := receiptSetup(t, func(tb testing.TB) *config.Config {
		tb.Helper()
		doc := parallel(tb)
		de := &doc.Companies[1]
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
		return doc
	})
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period <= 12
		AND status = 'closed'`, r.ten.ID)
	p := parallelEnv{env: r.env}
	p.leading, p.local = p.ledger(t, "0L"), p.ledger(t, "2L")
	north := r.newCustomer(t, "BP00000031", true)
	receipt, invoice := r.received(t, 10000, "EUR", "2026-03-20"), r.owed(t, north, 10000, "EUR", "2026-03-01")
	receiptCopy, invoiceCopy := p.copyOf(t, receipt.EntryID), p.copyOf(t, invoice.EntryID)
	copied := r.application(ledger.ItemRef{FiscalYear: 2026, EntryID: uuid.MustParse(receiptCopy)}, north, "EUR", "2026-03-21", invoice)
	if code, detail := r.refused(t, copied); code != "ledger.not_a_receipt" || !strings.Contains(detail, "leading ledger") {
		t.Errorf("a copy as the receipt: got %s (%s), want ledger.not_a_receipt naming the leading ledger", code, detail)
	}
	var applied ledger.AppliedReceipt
	if err := apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
		applied, err = postings().ApplyReceipt(ctx, r.application(receipt, north, "EUR", "2026-03-21", invoice))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ledgers, _ := p.group(t, applied.Entry.ID.String()); ledgers != "0L,2L" {
		t.Errorf("the application posted to: got %s, want 0L,2L", ledgers)
	}
	own := p.copyOf(t, applied.Entry.ID)
	wantCleared(t, map[string]string{receipt.EntryID.String(): "2", receiptCopy: "2", invoice.EntryID.String(): "1", invoiceCopy: "1",
		applied.Entry.ID.String(): "1,2", own: "1,2"}, map[string]string{receipt.EntryID.String(): "the receipt", receiptCopy: "the receipt's copy",
		invoice.EntryID.String(): "the invoice", invoiceCopy: "the invoice's copy", applied.Entry.ID.String(): "the application", own: "the application's copy"})
}

// TestApplyReceiptConcurrently: two requests applying one receipt to one
// item at once post one application; the other is refused, whichever
// order they run in (ADR-0008), and nothing of it stays.
func TestApplyReceiptConcurrently(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	receipt, invoice := r.received(t, 10000, "EUR", "2026-03-20"), r.owed(t, north, 10000, "EUR", "2026-03-01")
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) error {
				_, err := postings().ApplyReceipt(ctx, r.application(receipt, north, "EUR", "2026-03-21", invoice))
				return err
			})
		}()
	}
	wg.Wait()
	applied := 0
	for i, err := range errs {
		switch code := problem.From(err).Code; {
		case err == nil:
			applied++
		case code != "ledger.item_already_cleared":
			t.Errorf("request %d: got %v, want the receipt applied or 409 ledger.item_already_cleared", i+1, err)
		}
	}
	var entries int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_context = 'receivables' AND document_type = 'SA'
		AND posting_date = '2026-03-21'`, []any{r.ten.ID}, &entries)
	if applied != 1 || entries != 1 {
		t.Errorf("got %d requests applied and %d clearing entries, want 1 and 1", applied, entries)
	}
}

// TestReceipts: the reader of receipts not yet applied returns the open
// credit lines the payments context posted on the accounts, one per
// entry, by posting date; a payment out, another context's line, an entry
// with two lines there, and receipts applied or reversed are left out; one
// entry is looked for, and a page is cut, in the same order.
func TestReceipts(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	later, first := r.received(t, 20000, "EUR", "2026-03-21"), r.received(t, 10000, "EUR", "2026-03-20")
	applied, undone := r.received(t, 5000, "EUR", "2026-03-19"), r.received(t, 700, "EUR", "2026-03-18")
	r.received(t, -900, "EUR", "2026-03-20")
	for _, source := range []string{"ledger", "payments"} { // another context's line; two lines of one entry
		lines := []ledger.PostingLine{{AccountID: r.bank, Amount: 300}, {AccountID: r.incoming, Amount: -300}}
		if source == "payments" {
			lines = []ledger.PostingLine{{AccountID: r.bank, Amount: 300}, {AccountID: r.incoming, Amount: -200}, {AccountID: r.incoming, Amount: -100}}
		}
		r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
			return postings().Post(ctx, ledger.Posting{LegalEntityID: r.de, DocumentType: "SA", DocumentDate: date("2026-03-20"), PostingDate: date("2026-03-20"),
				Currency: "EUR", SourceContext: source, SourceDocumentID: uuid.New(), Lines: lines})
		})
	}
	r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		got, err := postings().ApplyReceipt(ctx, r.application(applied, north, "EUR", "2026-03-21", r.owed(t, north, 5000, "EUR", "2026-03-01")))
		return got.Entry, err
	})
	r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Reverse(ctx, undone.EntryID, nil, nil)
	})
	read := func(t *testing.T, page *pagination.Keyset, f ledger.ReceiptFilter) []ledger.Receipt {
		t.Helper()
		var got []ledger.Receipt
		if err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
			got, err = ledger.OpenItems{}.Receipts(ctx, page, r.de, []uuid.UUID{r.incoming}, f)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := read(t, nil, ledger.ReceiptFilter{})
	if len(all) != 2 || all[0].Item.EntryID != first.EntryID || all[1].Item.EntryID != later.EntryID {
		t.Fatalf("receipts: got %+v, want those of 20 and 21 March in that order", all)
	}
	if got := all[0]; got.Item.FiscalYear != 2026 || got.Item.LineNo != 2 || got.AccountID != r.incoming || got.Amount != 10000 || got.Currency != "EUR" ||
		got.Exponent != 2 || got.PostingDate != date("2026-03-20") || got.Number == "" || got.HeaderText != nil {
		t.Errorf("the first receipt: got %+v, want line 2 of 2026 on the incoming account, 10000 EUR (2 digits), posted 2026-03-20, numbered, without a text", got)
	}
	if one := read(t, nil, ledger.ReceiptFilter{Entry: &later.EntryID}); len(one) != 1 || one[0].Item.EntryID != later.EntryID {
		t.Errorf("one receipt looked for: got %+v, want the later one", one)
	}
	if none := read(t, nil, ledger.ReceiptFilter{Entry: &applied.EntryID}); len(none) != 0 {
		t.Errorf("an applied receipt looked for: got %+v, want none", none)
	}
	day := date("2026-03-21")
	for name, c := range map[string]struct {
		f    ledger.ReceiptFilter
		want []uuid.UUID
	}{
		"from a date": {ledger.ReceiptFilter{From: &day}, []uuid.UUID{later.EntryID}}, "to a date": {ledger.ReceiptFilter{To: &day}, []uuid.UUID{first.EntryID, later.EntryID}},
		"in the currency": {ledger.ReceiptFilter{Currency: "EUR"}, []uuid.UUID{first.EntryID, later.EntryID}}, "in another currency": {ledger.ReceiptFilter{Currency: "USD"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			var got []uuid.UUID
			for _, v := range read(t, nil, c.f) {
				got = append(got, v.Item.EntryID)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	codec, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := codec.Keyset(ledger.ReceiptSort, "receipts", pagination.Request{PageSize: 1, PageSizeSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if page := read(t, ks, ledger.ReceiptFilter{}); len(page) != 2 || page[0].Item.EntryID != first.EntryID {
		t.Errorf("a page of one: got %+v, want the first receipt and the row past the page", page)
	}
	if _, err := (ledger.OpenItems{}).Receipts(context.Background(), nil, r.de, nil, ledger.ReceiptFilter{}); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, r.ten, r.ten.Human, "receipts", func(ctx context.Context) error {
		_, err := ledger.OpenItems{}.Receipts(ctx, nil, r.de, []uuid.UUID{r.incoming}, ledger.ReceiptFilter{})
		return err
	})
}
