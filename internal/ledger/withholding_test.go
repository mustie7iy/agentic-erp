package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// withholding is the document with company 2000's withholding tax codes
// on a new liability account 236000, the payable.
func withholding(t *testing.T, codes ...config.WithholdingTaxCode) *config.Config {
	t.Helper()
	doc := document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "236000", Name: "Withholding tax payable", Type: config.Liability})
	de.WithholdingTaxCodes = codes
	return doc
}

// TestWithholdingTaxCodes: the setup keeps a company's withholding tax
// codes in its legal entity (T46e), updates changed ones, deactivates those
// the document no longer has and activates them again when it has them.
func TestWithholdingTaxCodes(t *testing.T) {
	e := setup(t)
	w1 := config.WithholdingTaxCode{Code: "W1", Description: "10% of net at invoice", RatePercent: "10", Account: "236000", Posting: config.WithholdingAtInvoice}
	w2 := config.WithholdingTaxCode{Code: "W2", Description: "15% of gross at payment", RatePercent: "15.125", Base: config.WithholdingGross, Account: "236000",
		Posting: config.WithholdingAtPayment}
	apply := func(doc *config.Config) {
		t.Helper()
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
		}); err != nil {
			t.Fatal(err)
		}
	}
	lookup := func(code string) (ledger.WithholdingTaxCode, bool) {
		t.Helper()
		var w ledger.WithholdingTaxCode
		var ok bool
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			w, ok, err = ledger.Accounts{}.WithholdingTaxCode(ctx, e.de, code)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return w, ok
	}
	apply(withholding(t, w1, w2))
	got, ok := lookup("W2")
	if !ok {
		t.Fatal("W2: got none, want the code")
	}
	for name, c := range map[string][2]string{"rate": {got.RatePct, "15.125"}, "base": {got.Base, "gross"}, "posting": {got.Posting, "payment"}} {
		if c[0] != c[1] {
			t.Errorf("W2 %s: got %s, want %s", name, c[0], c[1])
		}
	}
	if got.AccountID != settleAccount(t, e, "2000", "236000") {
		t.Errorf("W2 account: got %s, want 236000", got.AccountID)
	}
	w1.RatePercent = "12"
	apply(withholding(t, w1))
	if got, _ := lookup("W1"); got.RatePct != "12.000" {
		t.Errorf("W1 rate after the change: got %s, want 12.000", got.RatePct)
	}
	if _, ok := lookup("W2"); ok {
		t.Error("W2 once the document no longer has it: got the code, want none")
	}
	apply(withholding(t, w1, w2))
	if _, ok := lookup("W2"); !ok {
		t.Error("W2 in the document again: got none, want the code")
	}
	call := func(ctx context.Context) error {
		_, _, err := ledger.Accounts{}.WithholdingTaxCode(ctx, e.de, "W1")
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "withholding tax code", call)
}

// TestWithholdingOnOpenItems: a posting's withholding taxes are kept with
// its supplier open item (T46f), and its payable item carries those
// deducted at payment; withholding taxes for an entry without one supplier
// open item are refused.
func TestWithholdingOnOpenItems(t *testing.T) {
	e := setup(t)
	account := settleAccount(t, e, "2000", "520000")
	taxes := []ledger.Withholding{{Code: "W1", Posting: config.WithholdingAtInvoice, RatePct: "10", AccountID: account, Base: 10000, Amount: 1000},
		{Code: "W2", Posting: config.WithholdingAtPayment, RatePct: "15.5", AccountID: account, Base: 10000}}
	in := termsPosting(t, e, nil, &e.supplier)
	in.Withholding = taxes
	var posted ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		posted, err = postings().Post(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var items []ledger.PayableItem
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-03-01"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].EntryID != posted.ID {
		t.Fatalf("payable items: got %+v, want the posting's", items)
	}
	if got, want := fmt.Sprintf("%+v", items[0].Withholding), fmt.Sprintf("%+v", taxes[1:]); got != want {
		t.Errorf("payable item withholding: got %s, want %s", got, want)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		bad := termsPosting(t, e, nil, nil)
		bad.Lines[1].AccountID, bad.Withholding = settleAccount(t, e, "2000", "100000"), taxes // a bank line: no supplier open item
		_, err := postings().Post(ctx, bad)
		return err
	})
	pgtest.CheckErr(t, err, "no single supplier open item")
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post with withholding", func(ctx context.Context) error {
		_, err := postings().Post(ctx, in)
		return err
	})
}
