package ledger_test

import (
	"context"
	"maps"
	"reflect"
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

// TestLineMethodsKeptOnOpenItems: an entry whose supplier items do not
// share a payment method keeps each line's own (ADR-0038 As built, a
// migrated item's fixed payment method), as a migration's opening entry
// needs: the payable items show each item's, and the migration's report
// counts and sums the open items per partner and method. A method on a
// line that is no supplier open item, or beside the entry's own, is
// refused rather than dropped.
func TestLineMethodsKeptOnOpenItems(t *testing.T) {
	e := setup(t)
	s, c := "S", "C"
	payable, expense := settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "520000")
	opening := func() ledger.Posting {
		in := termsPosting(t, e, nil, &e.supplier)
		in.Lines = []ledger.PostingLine{{AccountID: expense, Amount: 17500},
			{AccountID: payable, Amount: -10000, PartnerID: &e.supplier, PaymentMethod: &s},
			{AccountID: payable, Amount: -2500, PartnerID: &e.supplier, PaymentMethod: &s},
			{AccountID: payable, Amount: -4000, PartnerID: &e.vendor, PaymentMethod: &c},
			{AccountID: payable, Amount: -1000, PartnerID: &e.vendor}}
		return in
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, opening())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		items, err := ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-03-01"))
		if err != nil {
			return err
		}
		got := map[int32]string{}
		for _, it := range items {
			got[it.LineNo] = "none"
			if it.PaymentMethod != nil {
				got[it.LineNo] = *it.PaymentMethod
			}
		}
		if want := map[int32]string{2: "S", 3: "S", 4: "C", 5: "none"}; !maps.Equal(got, want) {
			t.Errorf("fixed methods of the payable items by line: got %v, want %v", got, want)
		}
		byMethod, err := ledger.Reports{}.OpenItemsByMethod(ctx, e.de, 2026)
		want := map[uuid.UUID]map[string]ledger.FixedMethodItems{e.supplier: {"S": {Count: 2, LC: -12500}}, e.vendor: {"C": {Count: 1, LC: -4000}}}
		if err == nil && !reflect.DeepEqual(byMethod, want) {
			t.Errorf("open items by fixed method: got %v, want %v", byMethod, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		change func(*ledger.Posting)
		want   string
	}{
		"a method on an expense line": {func(in *ledger.Posting) { in.Lines[0].PaymentMethod = &s }, "no supplier open item"},
		"beside the entry's method":   {func(in *ledger.Posting) { in.PaymentMethod = &s }, "fixes one for all its items"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				in := opening()
				tc.change(&in)
				_, err := postings().Post(ctx, in)
				return err
			})
			pgtest.CheckErr(t, err, tc.want)
		})
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post with a line's fixed method", func(ctx context.Context) error {
		_, err := postings().Post(ctx, opening())
		return err
	})
	apitest.EveryFailure(t, e.ten, e.ten.Human, "open items by fixed method", func(ctx context.Context) error {
		_, err := ledger.Reports{}.OpenItemsByMethod(ctx, e.de, 2026)
		return err
	})
}
