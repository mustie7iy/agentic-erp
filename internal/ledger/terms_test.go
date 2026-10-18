package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// termsPosting is a supplier invoice of 100.00 EUR in company 2000 with the
// terms given for its open item.
func termsPosting(t *testing.T, e env, terms *ledger.ItemTerms, partner *uuid.UUID) ledger.Posting {
	t.Helper()
	ref := "INV-T"
	return ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"), Currency: "EUR",
		Reference: &ref, SourceContext: "payables", SourceDocumentID: uuid.New(), Terms: terms,
		Lines: []ledger.PostingLine{{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 10000},
			{AccountID: settleAccount(t, e, "2000", "211000"), Amount: -10000, PartnerID: partner}}}
}

// TestTermsFrozenOnOpenItems: a posting's payment terms are kept with its
// supplier open item (T36b), which the payable items and the open-items
// listing show with the net due date and each discount's last day; an
// entry without terms has none, and terms for an entry with no supplier
// open item are refused.
func TestTermsFrozenOnOpenItems(t *testing.T) {
	e := setup(t)
	terms := &ledger.ItemTerms{PaymentTermCode: "ZB01", BaselineDate: date("2026-03-01"), NetDays: 45,
		Discounts: []ledger.Discount{{Days: 14, Percent: "3"}, {Days: 30, Percent: "2.5"}}}
	var with, without ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		if with, err = postings().Post(ctx, termsPosting(t, e, terms, &e.supplier)); err != nil {
			return err
		}
		without, err = postings().Post(ctx, termsPosting(t, e, nil, &e.vendor))
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
	got := map[uuid.UUID]*ledger.ItemTerms{}
	for _, it := range items {
		got[it.EntryID] = it.Terms
	}
	if got[with.ID] == nil {
		t.Fatal("payable item terms: got none, want the posting's")
	}
	if got, want := fmt.Sprintf("%+v", *got[with.ID]), fmt.Sprintf("%+v", *terms); got != want {
		t.Errorf("payable item terms: got %s, want %s", got, want)
	}
	if got[without.ID] != nil {
		t.Errorf("payable item of an entry without terms: got %+v, want none", got[without.ID])
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.supplier.String(), "", "")
	listed := page["items"].([]any)
	if len(listed) != 1 {
		t.Fatalf("open items of the supplier: got %d, want 1", len(listed))
	}
	shown := fmt.Sprint(listed[0].(map[string]any)["payment_terms"])
	if want := "map[baseline_date:2026-03-01 code:ZB01 discounts:[map[percent:3 until:2026-03-15] map[percent:2.5 until:2026-03-31]] net_due_date:2026-04-15]"; shown != want {
		t.Errorf("listed payment terms: got %s, want %s", shown, want)
	}
	page, _ = e.Must(200, "GET", "/v1/open-items?partner_id="+e.vendor.String(), "", "")
	if shown := page["items"].([]any)[0].(map[string]any)["payment_terms"]; shown != nil {
		t.Errorf("listed payment terms of an item without terms: got %v, want none", shown)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		in := termsPosting(t, e, terms, nil)
		in.Lines[1].AccountID = settleAccount(t, e, "2000", "100000") // a bank line: no supplier open item
		_, err := postings().Post(ctx, in)
		return err
	})
	pgtest.CheckErr(t, err, "no supplier open item")
	three := *terms
	three.Discounts = append(three.Discounts, ledger.Discount{Days: 40, Percent: "1"})
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, termsPosting(t, e, &three, &e.supplier))
		return err
	})
	pgtest.CheckErr(t, err, "3 discount tiers")
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post with terms", func(ctx context.Context) error {
		_, err := postings().Post(ctx, termsPosting(t, e, terms, &e.supplier))
		return err
	})
}
