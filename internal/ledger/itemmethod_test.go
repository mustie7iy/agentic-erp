package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestFixedMethodKeptOnOpenItems: the payment method a posting fixes is
// kept with its supplier open item (ADR-0038, SAP BSEG-ZLSCH), which the
// payable items a run reads and the open-items listing show; an entry
// that fixes none has none, and a method for an entry with no supplier
// open item is refused rather than kept for nothing.
func TestFixedMethodKeptOnOpenItems(t *testing.T) {
	e := setup(t)
	method := "S"
	fixing := func(partner *uuid.UUID, m *string) ledger.Posting {
		in := termsPosting(t, e, nil, partner)
		in.PaymentMethod = m
		return in
	}
	var with, without ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		if with, err = postings().Post(ctx, fixing(&e.supplier, &method)); err != nil {
			return err
		}
		without, err = postings().Post(ctx, fixing(&e.vendor, nil))
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
	got := map[uuid.UUID]string{}
	for _, it := range items {
		got[it.EntryID] = "none"
		if it.PaymentMethod != nil {
			got[it.EntryID] = *it.PaymentMethod
		}
	}
	for entry, want := range map[uuid.UUID]string{with.ID: "S", without.ID: "none"} {
		if got[entry] != want {
			t.Errorf("payable item of entry %s, fixed method: got %q, want %q", entry, got[entry], want)
		}
	}
	for partner, want := range map[uuid.UUID]any{e.supplier: "S", e.vendor: nil} {
		page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+partner.String(), "", "")
		listed := page["items"].([]any)
		if len(listed) != 1 {
			t.Fatalf("open items of %s: got %d, want 1", partner, len(listed))
		}
		if shown := listed[0].(map[string]any)["payment_method"]; shown != want {
			t.Errorf("listed payment method of %s: got %v, want %v", partner, shown, want)
		}
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		in := fixing(nil, &method)
		in.Lines[1].AccountID = settleAccount(t, e, "2000", "100000") // a bank line: no supplier open item
		_, err := postings().Post(ctx, in)
		return err
	})
	pgtest.CheckErr(t, err, "no supplier open item")
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post with a fixed method", func(ctx context.Context) error {
		_, err := postings().Post(ctx, fixing(&e.supplier, &method))
		return err
	})
}
