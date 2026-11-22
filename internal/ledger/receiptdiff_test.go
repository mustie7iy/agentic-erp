package ledger_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withDifferences gives company 2000 the receivables account and the
// payment difference accounts: 680000, an expense, as its
// payment_difference_expense, and 480000, a revenue, which the account
// determination does not name until a test sets it.
func withDifferences(tb testing.TB) *config.Config {
	tb.Helper()
	doc := withReceivables(tb)
	de := &doc.Companies[1]
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "680000", Name: "Payment differences", Type: config.Expense},
		config.Account{Number: "480000", Name: "Payment differences kept", Type: config.Revenue})
	de.AccountDetermination.PaymentDifferenceExpense = "680000"
	return doc
}

// tolerance is a limit of amount minor units of EUR and percent.
func tolerance(amount int64, percent string) config.ToleranceLimit {
	r, _ := new(big.Rat).SetString(percent)
	return config.ToleranceLimit{Amount: money.Minor(amount), Percent: r}
}

// apply applies in in a committed request.
func (r receipts) apply(t *testing.T, in ledger.ReceiptApplication) ledger.AppliedReceipt {
	t.Helper()
	var applied ledger.AppliedReceipt
	if err := apitest.Committed(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
		applied, err = postings().ApplyReceipt(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return applied
}

// TestApplyReceiptResidual (T75f, ADR-0063 §6 As built): a receipt that
// pays an invoice in part leaves a residual item: the invoice is cleared,
// and a new item of the customer for the rest is a line of the clearing
// entry on the invoice's account, with the invoice's due date and no cash
// discount; a residual on a second invoice paid in part beside a credit
// netted works the same way.
func TestApplyReceiptResidual(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	terms := &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date("2026-03-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: "2"}}}
	in := customerPosting(t, r.env, north, 10000, "2026-03-01", terms)
	invoice := r.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Post(ctx, in) })
	item := ledger.ItemRef{FiscalYear: invoice.FiscalYear, EntryID: invoice.ID, LineNo: 1}
	plain, credit := r.owed(t, north, 5000, "EUR", "2026-03-02"), r.owed(t, north, -1000, "EUR", "2026-03-03")
	app := r.application(r.received(t, 7000, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", item, plain, credit)
	disputed := "Disputed: the second visit"
	app.Residuals = []ledger.Residual{{Amount: 4000, Text: &disputed}, {Amount: 3000}, {}}
	applied := r.apply(t, app)
	if got, want := posted(t, r.env, applied.Entry.ID),
		"103000 7000/7000/7000; 120000 -10000/-10000/-10000; 120000 -5000/-5000/-5000; 120000 1000/1000/1000; 120000 4000/4000/4000; 120000 3000/3000/3000"; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(applied.ResidualLines, int64(applied.Difference), int64(applied.OnAccount), applied.OnAccountLine), "[5 6 0] 0 0 0"; got != want {
		t.Errorf("residual lines, difference, on account: got %s, want %s", got, want)
	}
	open := receivable(t, r.env, north)
	if len(open) != 2 {
		t.Fatalf("the customer's open items: got %d, want the two residual items", len(open))
	}
	for _, it := range open {
		if it.EntryID != applied.Entry.ID || it.PartnerID != north {
			t.Errorf("open item %+v: got entry %s of %s, want a line of the clearing entry of the customer", it.ItemRef, it.EntryID, it.PartnerID)
		}
		if it.LineNo == 5 && (it.Amount != 4000 || it.Terms == nil || it.Terms.NetDueDate() != date("2026-03-31") || len(it.Terms.Discounts) != 0) {
			t.Errorf("the invoice's residual: got %d with terms %+v, want 4000 due 2026-03-31 without a discount", it.Amount, it.Terms)
		}
		if it.LineNo == 6 && (it.Amount != 3000 || it.Terms != nil) {
			t.Errorf("the second invoice's residual: got %d with terms %+v, want 3000 without terms, as the item had none", it.Amount, it.Terms)
		}
	}
	var text string
	pgtest.Scan(t, `SELECT line_text FROM ledger.journal_line WHERE tenant_id = $1 AND journal_entry_id = $2 AND line_no = 5`, []any{r.ten.ID, applied.Entry.ID}, &text)
	if text != disputed {
		t.Errorf("the residual's text: got %q, want %q", text, disputed)
	}
}

// TestApplyReceiptResidualRefusals: a residual that is not more than zero
// and less than a debit item is refused, naming its field, and a residual
// that leaves the items short of the receipt is refused as before.
func TestApplyReceiptResidualRefusals(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	receipt := r.received(t, 6000, "EUR", "2026-03-20")
	invoice, credit := r.owed(t, north, 10000, "EUR", "2026-03-01"), r.owed(t, north, -1000, "EUR", "2026-03-02")
	for _, c := range []struct {
		name      string
		residuals []ledger.Residual
		code      string
		field     string
	}{
		{"a negative residual", []ledger.Residual{{Amount: -1}, {}}, "ledger.invalid_residual", "items[0].residual.amount"},
		{"the whole item", []ledger.Residual{{Amount: 10000}, {}}, "ledger.invalid_residual", "items[0].residual.amount"},
		{"of a credit", []ledger.Residual{{Amount: 4000}, {Amount: 500}}, "ledger.invalid_residual", "items[1].residual.amount"},
		{"leaving the items short of the receipt", []ledger.Residual{{Amount: 5000}, {}}, "ledger.clearing_not_balanced", "items"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := r.application(receipt, north, "EUR", "2026-03-21", invoice, credit)
			in.Residuals = c.residuals
			p := r.refusal(t, in)
			if p.Code != c.code {
				t.Errorf("got %s (%s), want %s", p.Code, p.Detail, c.code)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("field errors: got %+v, want one naming %s", p.Errors, c.field)
			}
		})
	}
	t.Run("residuals that are not one per item", func(t *testing.T) {
		in := r.application(receipt, north, "EUR", "2026-03-21", invoice, credit)
		in.Residuals = []ledger.Residual{{Amount: 4000}}
		err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) error {
			_, err := postings().ApplyReceipt(ctx, in)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "1 residuals for 2 items") {
			t.Errorf("got %v, want the internal error naming the counts", err)
		}
	})
	t.Run("cash discounts that are not one per item", func(t *testing.T) {
		in := r.application(receipt, north, "EUR", "2026-03-21", invoice, credit)
		in.CashDiscounts = []bool{true}
		err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) error {
			_, err := postings().ApplyReceipt(ctx, in)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "1 cash discounts for 2 items") {
			t.Errorf("got %v, want the internal error naming the counts", err)
		}
	})
}

// TestApplyReceiptDifference (T75f): a difference between the items and
// the receipt within the tolerance of its direction is written off, an
// underpayment to payment_difference_expense and an overpayment to
// payment_difference_revenue; the lower of the amount and the percent of
// the items binds; beyond it, or without the account, the application is
// refused and nothing posts.
func TestApplyReceiptDifference(t *testing.T) {
	r := receiptSetup(t, withDifferences)
	north := r.newCustomer(t, "BP00000031", true)
	under, over := tolerance(500, "1"), tolerance(200, "0.5")
	app := func(received, owed int64, day string) ledger.ReceiptApplication {
		in := r.application(r.received(t, received, "EUR", day), north, "EUR", day, r.owed(t, north, owed, "EUR", "2026-03-01"))
		in.Under, in.Over = under, over
		return in
	}
	t.Run("an underpayment within the tolerance", func(t *testing.T) {
		applied := r.apply(t, app(9995, 10000, "2026-03-20"))
		if got, want := posted(t, r.env, applied.Entry.ID), "103000 9995/9995/9995; 120000 -10000/-10000/-10000; 680000 5/5/5"; got != want {
			t.Errorf("lines: got %s, want %s", got, want)
		}
		if applied.Difference != 5 {
			t.Errorf("difference: got %d, want 5", applied.Difference)
		}
	})
	for _, c := range []struct {
		name            string
		received, owed  int64
		code, inMessage string
	}{
		{"an underpayment over the percent", 9800, 10000, "ledger.clearing_not_balanced", "by 2.00: an underpayment beyond"},
		{"an underpayment over the amount", 9999400, 10000000, "ledger.clearing_not_balanced", "by 6.00: an underpayment beyond"},
		{"an overpayment without the revenue account", 10002, 10000, "ledger.no_payment_difference_account", "payment_difference_revenue"},
		{"an overpayment over the percent", 10060, 10000, "ledger.clearing_not_balanced", "by -0.60: an overpayment beyond"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, detail := r.refused(t, app(c.received, c.owed, "2026-03-20"))
			if code != c.code || !strings.Contains(detail, c.inMessage) {
				t.Errorf("got %s (%s), want %s saying %q", code, detail, c.code, c.inMessage)
			}
		})
	}
	t.Run("without a tolerance", func(t *testing.T) {
		in := app(9995, 10000, "2026-03-20")
		in.Under = config.ToleranceLimit{}
		if code, _ := r.refused(t, in); code != "ledger.clearing_not_balanced" {
			t.Errorf("got %s, want ledger.clearing_not_balanced", code)
		}
	})
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'payment_difference_revenue', $3, $4)`,
		r.ten.ID, r.de, settleAccount(t, r.env, "2000", "480000"), r.ten.Human.ID)
	t.Run("an overpayment within the tolerance", func(t *testing.T) {
		applied := r.apply(t, app(10002, 10000, "2026-03-21"))
		if got, want := posted(t, r.env, applied.Entry.ID), "103000 10002/10002/10002; 120000 -10000/-10000/-10000; 480000 -2/-2/-2"; got != want {
			t.Errorf("lines: got %s, want %s", got, want)
		}
	})
	t.Run("failing statements", func(t *testing.T) {
		in := app(9995, 10000, "2026-03-22")
		terms := &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date("2026-03-01"), NetDays: 30}
		posting := customerPosting(t, r.env, north, 10000, "2026-03-01", terms)
		invoice := r.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Post(ctx, posting) })
		in.Items = append(in.Items, ledger.ItemRef{FiscalYear: invoice.FiscalYear, EntryID: invoice.ID, LineNo: 1})
		in.Residuals = []ledger.Residual{{}, {Amount: 10000 - 5}}
		in.Under = tolerance(500, "50")
		apitest.EveryFailure(t, r.ten, r.ten.Human, "apply a receipt with a residual and a difference", func(ctx context.Context) error {
			_, err := postings().ApplyReceipt(ctx, in)
			return err
		})
	})
	t.Run("the payment difference's account blocked", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND account_id = $3`, r.ten.ID, r.de, settleAccount(t, r.env, "2000", "680000"))
		p := r.refusal(t, app(9995, 10000, "2026-03-22"))
		if p.Code != "ledger.account_not_postable" || !strings.Contains(p.Detail, "payment difference's account (payment_difference_expense") || len(p.Errors) != 0 {
			t.Errorf("got %s (%s) with field errors %+v, want ledger.account_not_postable naming the payment difference's account", p.Code, p.Detail, p.Errors)
		}
	})
}

// TestApplyReceiptDifferenceInForeignCurrency: in a foreign currency a
// residual keeps its item's rate and the difference takes the receipt's,
// so only what the items paid realizes an exchange difference.
func TestApplyReceiptDifferenceInForeignCurrency(t *testing.T) {
	r := receiptSetup(t, withDifferences)
	north := r.newCustomer(t, "BP00000031", true)
	settleRate(t, r.env, "GBP", "2026-03-01", "1.20")
	settleRate(t, r.env, "GBP", "2026-04-01", "1.10")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_loss', $3, $4)`,
		r.ten.ID, r.de, settleAccount(t, r.env, "2000", "520000"), r.ten.Human.ID)
	in := r.application(r.received(t, 5995, "GBP", "2026-04-10"), north, "GBP", "2026-04-10", r.owed(t, north, 10000, "GBP", "2026-03-10"))
	in.Residuals = []ledger.Residual{{Amount: 4000}}
	in.Under = tolerance(500, "1")
	applied := r.apply(t, in)
	// The 6,000.00 paid of the item was booked at 7,200 EUR and is settled by 6,595 received and 6 written off: 599 lost on the rate.
	want := "103000 5995/6595/6595; 120000 -10000/-12000/-12000; 120000 4000/4800/4800; 680000 5/6/6; 520000 0/599/0; 520000 0/0/599"
	if got := posted(t, r.env, applied.Entry.ID); got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
}

// TestApplyReceiptOnAccount (T75g, ADR-0063 §6 As built): what a receipt
// leaves after its items, or the whole receipt without items, goes on the
// customer's account as a credit at the receipt's rate; a later
// application applies that credit to items as a receipt is applied, the
// rest of it staying on account as a new credit.
func TestApplyReceiptOnAccount(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	account := settleAccount(t, r.env, "2000", "120000")
	whole := r.application(r.received(t, 10000, "EUR", "2026-03-20"), north, "EUR", "2026-03-20")
	whole.OnAccount = account
	applied := r.apply(t, whole)
	if got, want := posted(t, r.env, applied.Entry.ID), "103000 10000/10000/10000; 120000 -10000/-10000/-10000"; got != want {
		t.Errorf("a receipt on account: got %s, want %s", got, want)
	}
	if applied.OnAccount != 10000 || applied.OnAccountLine != 2 || applied.Entry.Lines[1].PartnerID == nil || *applied.Entry.Lines[1].PartnerID != north {
		t.Errorf("on account: got %d on line %d of %v, want 10000 on line 2 of the customer", applied.OnAccount, applied.OnAccountLine, applied.Entry.Lines[1].PartnerID)
	}
	credit := ledger.ItemRef{FiscalYear: applied.Entry.FiscalYear, EntryID: applied.Entry.ID, LineNo: applied.OnAccountLine}
	part := r.application(r.received(t, 10000, "EUR", "2026-03-21"), north, "EUR", "2026-03-21", r.owed(t, north, 6000, "EUR", "2026-03-01"))
	part.OnAccount = account
	if got, want := posted(t, r.env, r.apply(t, part).Entry.ID), "103000 10000/10000/10000; 120000 -6000/-6000/-6000; 120000 -4000/-4000/-4000"; got != want {
		t.Errorf("the rest of a receipt on account: got %s, want %s", got, want)
	}
	invoice := r.owed(t, north, 3000, "EUR", "2026-03-02")
	later := r.application(credit, north, "EUR", "2026-03-22", invoice)
	later.Credit, later.ReceiptAccountID, later.OnAccount = true, account, account
	again := r.apply(t, later)
	if got, want := posted(t, r.env, again.Entry.ID), "120000 10000/10000/10000; 120000 -3000/-3000/-3000; 120000 -7000/-7000/-7000"; got != want {
		t.Errorf("the credit applied: got %s, want %s", got, want)
	}
	wantCleared(t, map[string]string{credit.EntryID.String(): "1,2", invoice.EntryID.String(): "1", again.Entry.ID.String(): "1,2"},
		map[string]string{credit.EntryID.String(): "the credit", invoice.EntryID.String(): "the invoice", again.Entry.ID.String(): "the application"})
	if again.OnAccount != 7000 || again.OnAccountLine != 3 {
		t.Errorf("left on account: got %d on line %d, want 7000 on line 3", again.OnAccount, again.OnAccountLine)
	}
	t.Run("which lines are closed", func(t *testing.T) {
		left := ledger.ItemRef{FiscalYear: again.Entry.FiscalYear, EntryID: again.Entry.ID, LineNo: again.OnAccountLine}
		var closed map[ledger.ItemRef]bool
		if err := apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) (err error) {
			closed, err = ledger.OpenItems{}.Closed(ctx, []ledger.ItemRef{credit, left, invoice})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(closed[credit], closed[left], closed[invoice], len(closed)), "true false true 2"; got != want {
			t.Errorf("closed (credit, what is left, invoice, count): got %s, want %s", got, want)
		}
		if _, err := (ledger.OpenItems{}).Closed(context.Background(), nil); err == nil {
			t.Error("outside a unit of work: got nil error, want an error")
		}
		apitest.EveryFailure(t, r.ten, r.ten.Human, "read which lines are closed", func(ctx context.Context) error {
			_, err := ledger.OpenItems{}.Closed(ctx, []ledger.ItemRef{credit})
			return err
		})
	})
	t.Run("the credit applied twice", func(t *testing.T) {
		if code, _ := r.refused(t, later); code != "ledger.item_already_cleared" {
			t.Errorf("got %s, want ledger.item_already_cleared", code)
		}
	})
}

// TestApplyCreditRefusals: a credit applied must be an open credit line
// of the customer on the account named, and not among the items.
func TestApplyCreditRefusals(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north, south := r.newCustomer(t, "BP00000031", true), r.newCustomer(t, "BP00000032", true)
	account := settleAccount(t, r.env, "2000", "120000")
	credit, souths, debit := r.owed(t, north, -3000, "EUR", "2026-03-01"), r.owed(t, south, -3000, "EUR", "2026-03-01"), r.owed(t, north, 3000, "EUR", "2026-03-01")
	undone := r.owed(t, north, -3000, "EUR", "2026-03-02")
	r.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Reverse(ctx, undone.EntryID, nil, nil)
	})
	invoice := r.owed(t, north, 3000, "EUR", "2026-03-03")
	app := func(c ledger.ItemRef, items ...ledger.ItemRef) ledger.ReceiptApplication {
		in := r.application(c, north, "EUR", "2026-03-21", items...)
		in.Credit, in.ReceiptAccountID = true, account
		return in
	}
	onBank := app(credit, invoice)
	onBank.ReceiptAccountID = r.bank
	for _, c := range []struct {
		name   string
		in     ledger.ReceiptApplication
		code   string
		detail string
	}{
		{"another customer's", app(souths, invoice), "ledger.not_a_receipt", "is not a line of the customer's"},
		{"a debit", app(debit, invoice), "ledger.not_a_receipt", "it is no credit"},
		{"on another account", onBank, "ledger.not_a_receipt", "is not a line of the customer's"},
		{"reversed", app(undone, invoice), "ledger.entry_reversed", "The credit's journal entry"},
		{"among the items", app(credit, invoice, credit), "ledger.duplicate_item", "Item 2 is the credit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, detail := r.refused(t, c.in)
			if code != c.code || !strings.Contains(detail, c.detail) {
				t.Errorf("got %s (%s), want %s saying %q", code, detail, c.code, c.detail)
			}
		})
	}
	t.Run("an account that takes no partner", func(t *testing.T) {
		in := r.application(r.received(t, 100, "EUR", "2026-03-20"), north, "EUR", "2026-03-21")
		in.OnAccount = r.bank
		p := r.refusal(t, in)
		if p.Code != "ledger.partner_not_allowed" || !strings.Contains(p.Detail, "reconciliation account the credit is left on") {
			t.Errorf("got %s (%s), want ledger.partner_not_allowed naming the account the credit is left on", p.Code, p.Detail)
		}
	})
}

// TestApplyReceiptOnAccountInForeignCurrency: a receipt left on account
// keeps the receipt's local and group amounts, so none of it realizes an
// exchange difference.
func TestApplyReceiptOnAccountInForeignCurrency(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	settleRate(t, r.env, "GBP", "2026-03-01", "1.10")
	in := r.application(r.received(t, 9995, "GBP", "2026-03-20"), north, "GBP", "2026-03-20")
	in.OnAccount = settleAccount(t, r.env, "2000", "120000")
	if got, want := posted(t, r.env, r.apply(t, in).Entry.ID), "103000 9995/10995/10995; 120000 -9995/-10995/-10995"; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
}

// TestApplyReceiptWriteOff (T75o, ADR-0063 §6 As built): what of an item
// its reason writes off is a debit of the reason's account at the
// receipt's rate, the item cleared in full and nothing left open; together
// with the payment difference, or alone when the rest is left on account,
// it is held to the underpayment tolerance; the account must be postable,
// a refusal naming it by what it is.
func TestApplyReceiptWriteOff(t *testing.T) {
	r := receiptSetup(t, withDifferences)
	north := r.newCustomer(t, "BP00000031", true)
	charges := settleAccount(t, r.env, "2000", "680000")
	under := tolerance(500, "5")
	for _, c := range []struct {
		name, lines, shown string
		receipt, owed      int64
		writeOff           int64
		onAccount          bool
	}{
		{"a write-off alone", "103000 9600/9600/9600; 120000 -10000/-10000/-10000; 680000 400/400/400", "[0] [3] 0 0", 9600, 10000, 400, false},
		{"a write-off and a difference within the tolerance", "103000 9599/9599/9599; 120000 -10000/-10000/-10000; 680000 400/400/400; 680000 1/1/1",
			"[0] [3] 1 0", 9599, 10000, 400, false},
		{"a write-off with the rest on account", "103000 10000/10000/10000; 120000 -10000/-10000/-10000; 680000 400/400/400; 120000 -400/-400/-400",
			"[0] [3] 0 400", 10000, 10000, 400, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := r.application(r.received(t, c.receipt, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", r.owed(t, north, c.owed, "EUR", "2026-03-01"))
			in.Under, in.Residuals = under, []ledger.Residual{{Amount: money.Minor(c.writeOff), WriteOff: charges}}
			if c.onAccount {
				in.OnAccount = settleAccount(t, r.env, "2000", "120000")
			}
			applied := r.apply(t, in)
			if got := posted(t, r.env, applied.Entry.ID); got != c.lines {
				t.Errorf("lines: got %s, want %s", got, c.lines)
			}
			if got := fmt.Sprint(applied.ResidualLines, applied.WriteOffLines, int64(applied.Difference), int64(applied.OnAccount)); got != c.shown {
				t.Errorf("residual and write-off lines, difference, on account: got %s, want %s", got, c.shown)
			}
		})
	}
	if open := receivable(t, r.env, north); len(open) != 1 || open[0].Amount != -400 {
		t.Errorf("the customer's open items: got %+v, want the credit left on account alone", open)
	}
	for _, c := range []struct {
		name              string
		receipt, writeOff int64
		onAccount         bool
		want              string
	}{
		{"a write-off beyond the tolerance", 9400, 600, false, "by 6.00: an underpayment beyond"},
		{"a write-off and a difference beyond it together", 9499, 450, false, "by 5.01: an underpayment beyond"},
		{"a write-off with the rest on account beyond it", 10000, 600, true, "by 6.00: an underpayment beyond"},
		// Neither direction nets the other away.
		{"a write-off an overpayment would cancel", 10000, 9999, false, "by 99.99: an underpayment beyond"},
		{"an overpayment beside a write-off within the tolerance", 10600, 400, false, "by -10.00: an overpayment beyond"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := r.application(r.received(t, c.receipt, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", r.owed(t, north, 10000, "EUR", "2026-03-01"))
			in.Under, in.Over, in.Residuals = under, under, []ledger.Residual{{Amount: money.Minor(c.writeOff), WriteOff: charges}}
			if c.onAccount {
				in.OnAccount = settleAccount(t, r.env, "2000", "120000")
			}
			p := r.refusal(t, in)
			if p.Code != "ledger.clearing_not_balanced" || !strings.Contains(p.Detail, c.want) {
				t.Errorf("got %s (%s), want ledger.clearing_not_balanced %q", p.Code, p.Detail, c.want)
			}
		})
	}
	t.Run("failing statements", func(t *testing.T) {
		in := r.application(r.received(t, 9600, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", r.owed(t, north, 10000, "EUR", "2026-03-01"))
		in.Under, in.Residuals = under, []ledger.Residual{{Amount: 400, WriteOff: charges}}
		apitest.EveryFailure(t, r.ten, r.ten.Human, "apply a receipt with a write-off", func(ctx context.Context) error {
			_, err := postings().ApplyReceipt(ctx, in)
			return err
		})
	})
	t.Run("the write-off account blocked", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND account_id = $3`, r.ten.ID, r.de, charges)
		in := r.application(r.received(t, 9600, "EUR", "2026-03-20"), north, "EUR", "2026-03-21", r.owed(t, north, 10000, "EUR", "2026-03-01"))
		in.Under, in.Residuals = under, []ledger.Residual{{Amount: 400, WriteOff: charges}}
		p := r.refusal(t, in)
		if p.Code != "ledger.account_not_postable" || !strings.Contains(p.Detail, "Item 1's write-off account, its reason's cannot be posted") {
			t.Errorf("got %s (%s), want ledger.account_not_postable naming item 1's write-off account", p.Code, p.Detail)
		}
	})
}

// TestApplyReceiptWriteOffTradingPartner (T75o, ADR-0059 §4): a reason's
// write-off is a difference line of the clearing, so it takes the
// trading partner of the item it was written off of.
func TestApplyReceiptWriteOffTradingPartner(t *testing.T) {
	r := receiptSetup(t, withDifferences)
	north := r.newCustomer(t, "BP00000031", true)
	posting := customerPosting(t, r.env, north, 10000, "2026-03-01", nil)
	posting.Lines[0].TradingPartner = &r.us
	invoice := r.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Post(ctx, posting) })
	in := r.application(r.received(t, 9600, "EUR", "2026-03-20"), north, "EUR", "2026-03-21",
		ledger.ItemRef{FiscalYear: invoice.FiscalYear, EntryID: invoice.ID, LineNo: 1})
	in.Under, in.Residuals = tolerance(500, "5"), []ledger.Residual{{Amount: 400, WriteOff: settleAccount(t, r.env, "2000", "680000")}}
	applied := r.apply(t, in)
	var partner *uuid.UUID
	pgtest.Scan(t, `SELECT trading_partner_id FROM ledger.journal_line WHERE tenant_id = $1 AND journal_entry_id = $2 AND line_no = $3
		AND ledger_id = (SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND is_leading)`, []any{r.ten.ID, applied.Entry.ID, applied.WriteOffLines[0]}, &partner)
	if partner == nil || *partner != r.us {
		t.Errorf("the write-off's trading partner: got %v, want %s, the item's", partner, r.us)
	}
}
