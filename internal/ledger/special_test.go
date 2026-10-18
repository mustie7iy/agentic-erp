package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// downPayments gives company 2000 a down payment account, 159000 (an asset
// vendor reconciliation account, ADR-0037), and returns the document.
func downPayments(t *testing.T, e env) *config.Config {
	t.Helper()
	doc := document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	return doc
}

// special is a line's special G/L indicator, or "" without one.
func special(l ledger.Line) string {
	if l.SpecialGL == nil {
		return ""
	}
	return *l.SpecialGL
}

// TestDownPaymentItems: a down payment posts as the supplier's special
// G/L item A on the down payment account (ADR-0037, T46a), which the
// payable items leave out and the open items and the entry show; its
// reversal is a special item too. A line on the down payment account
// without the indicator, or with it on another account, is refused, and
// the down payment account cannot change once activated. A manual
// clearing of the down payment is refused, and the down payment account is
// no partner's own reconciliation account.
func TestDownPaymentItems(t *testing.T) {
	e := setup(t)
	doc := downPayments(t, e)
	dp, bank, ap := settleAccount(t, e, "2000", "159000"), settleAccount(t, e, "2000", "100000"), settleAccount(t, e, "2000", "211000")
	a := "A"
	post := func(account uuid.UUID, special *string) (ledger.Entry, error) {
		var entry ledger.Entry
		err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			ref := "DP-1"
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"),
				Currency: "EUR", Reference: &ref, SourceContext: "payments", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					{AccountID: account, Amount: money.Minor(5000), PartnerID: &e.supplier, SpecialGL: special}, {AccountID: bank, Amount: -5000}}})
			return err
		})
		return entry, err
	}
	entry, err := post(dp, &a)
	if err != nil {
		t.Fatal(err)
	}
	if got := special(entry.Lines[0]); got != "A" {
		t.Errorf("posted line's special G/L indicator: got %q, want A", got)
	}
	var payable []ledger.PayableItem
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		payable, err = ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-12-31"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, it := range payable {
		if it.EntryID == entry.ID {
			t.Errorf("payable items: got the down payment %v, want it left out", it.ItemRef)
		}
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.supplier.String(), "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("open items: got %d, want the down payment", len(items))
	}
	if got := items[0].(map[string]any)["special_gl"]; got != "A" {
		t.Errorf("open item special G/L indicator: got %v, want A", got)
	}
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+entry.ID.String(), "", "")
	if got := je["lines"].([]any)[0].(map[string]any)["special_gl"]; got != "A" {
		t.Errorf("journal entry line special G/L indicator: got %v, want A", got)
	}
	posted := events(t, e.ten, "journal_entry_posted")
	if got := posted[len(posted)-1]["lines"].([]any)[0].(map[string]any)["special_gl"]; got != "A" {
		t.Errorf("JournalEntryPosted line special G/L indicator: got %v, want A", got)
	}
	// A down payment is not netted by hand either: it leaves its account
	// only by clearing against the invoice or by a reversal.
	code, m, _ := e.in(t).Do("POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{je, 1}, ref{je, 2}), "")
	if code != 409 {
		t.Errorf("clearing the down payment: got status %d, want 409", code)
	}
	if m["code"] != "ledger.special_gl_item" {
		t.Errorf("clearing the down payment: got code %v, want ledger.special_gl_item", m["code"])
	}
	var reversal ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		reversal, err = postings().Reverse(ctx, entry.ID, nil, nil)
		return err
	}); err != nil {
		t.Fatalf("reversal of the down payment: %v", err)
	}
	if got := special(reversal.Lines[0]); got != "A" {
		t.Errorf("reversal line's special G/L indicator: got %q, want A", got)
	}
	for name, c := range map[string]struct {
		account uuid.UUID
		special *string
		code    string
	}{
		"the down payment account without the indicator": {dp, nil, "ledger.special_gl_required"},
		"the indicator on the payables account":          {ap, &a, "ledger.special_gl_account"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := post(c.account, c.special)
			pgtest.WantErr(t, err, c.code)
		})
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		ok, err := ledger.Accounts{}.IsReconciliationAccount(ctx, e.de, dp, "supplier")
		if ok {
			t.Error("down payment account as a supplier's reconciliation account: got true, want false")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "journal_line_special_gl_account"})
	if p.Code != "ledger.special_gl_account" {
		t.Errorf("trigger journal_line_special_gl_account code: got %s, want ledger.special_gl_account", p.Code)
	}
	if p.Status != 422 {
		t.Errorf("trigger journal_line_special_gl_account status: got %d, want 422", p.Status)
	}
	changed := document(t) // the account stays in the chart, no longer the down payment account
	changed.Company("2000").ChartOfAccounts = append(changed.Company("2000").ChartOfAccounts,
		config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		fs, err := ledger.Setup{LegalEntities: tenancy.Directory{}}.Check(ctx, doc, changed)
		var paths []string
		for _, f := range fs {
			paths = append(paths, f.Path)
		}
		if !slices.Contains(paths, "companies[2000].account_determination.down_payment") {
			t.Errorf("removing the down payment account: got findings %v, want one on companies[2000].account_determination.down_payment", paths)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPayDownPayment: a down payment request's payment posts the down
// payment on the down payment account as special G/L item A, net of its
// input tax, the tax, and the bank (T46c); posting the same payment again
// returns its entry. A legal entity without a down payment account, or a
// tax code not valid on the posting date, is refused.
func TestPayDownPayment(t *testing.T) {
	e := setup(t)
	downPayments(t, e)
	bank := settleAccount(t, e, "2000", "100000")
	v1 := "V1"
	in := ledger.DownPaymentPayment{LegalEntityID: e.de, SupplierID: e.supplier, BankAccountID: bank, DocumentType: "SA", PostingDate: date("2026-03-20"),
		Currency: "EUR", Amount: money.Minor(11900), TaxCode: &v1, Tax: money.Minor(1900), SourceDocumentID: uuid.New()}
	var first, again ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		first, err = postings().PayDownPayment(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		amount  money.Minor
		special string
	}{{10000, "A"}, {1900, ""}, {-11900, ""}} {
		t.Run(fmt.Sprintf("line %d", i+1), func(t *testing.T) {
			if got := first.Lines[i].TC; got != want.amount {
				t.Errorf("amount: got %v, want %v", got, want.amount)
			}
			if got := special(first.Lines[i]); got != want.special {
				t.Errorf("special G/L: got %q, want %q", got, want.special)
			}
		})
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		again, err = postings().PayDownPayment(ctx, in)
		if err != nil {
			return err
		}
		untaxed := in
		untaxed.SourceDocumentID, untaxed.Tax = uuid.New(), 0
		entry, err := postings().PayDownPayment(ctx, untaxed)
		if err != nil {
			return err
		}
		if len(entry.Lines) != 2 {
			t.Errorf("a payment without tax: got %d lines, want 2", len(entry.Lines))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Errorf("posting again: got entry %s, want %s", again.ID, first.ID)
	}
	unknown := "ZZ"
	for name, c := range map[string]struct {
		mutate func(*ledger.DownPaymentPayment)
		code   string
	}{
		"no down payment account": {func(p *ledger.DownPaymentPayment) { p.LegalEntityID = e.us }, "ledger.no_down_payment_account"},
		"unknown tax code":        {func(p *ledger.DownPaymentPayment) { p.TaxCode = &unknown }, "ledger.unknown_tax_code"},
		"tax of the whole amount": {func(p *ledger.DownPaymentPayment) { p.Tax = p.Amount }, "ledger.invalid_down_payment"},
	} {
		t.Run(name, func(t *testing.T) {
			p := in
			p.SourceDocumentID = uuid.New()
			c.mutate(&p)
			err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, err := postings().PayDownPayment(ctx, p)
				return err
			})
			pgtest.WantErr(t, err, c.code)
		})
	}
	call := func(ctx context.Context) error {
		p := in
		p.SourceDocumentID = uuid.New()
		_, err := postings().PayDownPayment(ctx, p)
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "pay down payment", call)
	apitest.EveryFailure(t, e.ten, e.ten.Human, "pay down payment again", func(ctx context.Context) error {
		_, err := postings().PayDownPayment(ctx, in)
		return err
	})
}
