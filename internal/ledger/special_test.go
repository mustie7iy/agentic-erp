package ledger_test

import (
	"context"
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
