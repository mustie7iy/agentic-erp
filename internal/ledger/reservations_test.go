package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestReservations: open items an owner (a payment run) reserved are
// refused to every other clearing, settlement, reservation and reversal,
// settled by the owner, and free again once released.
func TestReservations(t *testing.T) {
	e := setup(t)
	a := settlePost(t, e, e.de, "2000", "EUR", 10000, "INV-A")
	b := settlePost(t, e, e.de, "2000", "EUR", 5000, "INV-B")
	memo := settlePost(t, e, e.de, "2000", "EUR", -10000, "CM-A") // a debit item that clears a
	run := ledger.Owner{Context: "payments", ID: uuid.New(), Reference: "PR100001"}
	other := ledger.Owner{Context: "payments", ID: uuid.New(), Reference: "PR100002"}
	in := func(fn func(ctx context.Context) error) error { return apitest.Committed(t, e.ten, e.ten.Human, fn) }
	if err := in(func(ctx context.Context) error { return postings().Reserve(ctx, run, []ledger.ItemRef{a, b}) }); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	expense := ledger.ItemRef{FiscalYear: a.FiscalYear, EntryID: a.EntryID, LineNo: 1}
	for name, c := range map[string]struct {
		call func(ctx context.Context) error
		code string
	}{
		"reserve for another owner": {func(ctx context.Context) error { return postings().Reserve(ctx, other, []ledger.ItemRef{a}) }, "ledger.item_reserved"},
		"reserve an expense line":   {func(ctx context.Context) error { return postings().Reserve(ctx, other, []ledger.ItemRef{expense}) }, "ledger.not_an_open_item"},
		"settle for another owner": {func(ctx context.Context) error {
			s := settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", a)
			s.ReservedFor = other.ID
			_, err := postings().Settle(ctx, s)
			return err
		}, "ledger.item_reserved"},
		"settle for no owner": {func(ctx context.Context) error {
			_, err := postings().Settle(ctx, settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", a))
			return err
		}, "ledger.item_reserved"},
		"clear by migration": {func(ctx context.Context) error {
			_, err := postings().Clear(ctx, e.de, date("2026-04-15"), memo, []ledger.ItemRef{a, memo})
			return err
		}, "ledger.item_reserved"},
		"reverse": {func(ctx context.Context) error { _, err := postings().Reverse(ctx, a.EntryID, nil, nil); return err }, "ledger.item_reserved"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := problem.From(apitest.InRequest(t, e.ten, e.ten.Human, c.call)).Code; got != c.code {
				t.Errorf("code: got %s, want %s", got, c.code)
			}
		})
	}
	body := fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-04-15", "items": [{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 2},
		{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 2}]}`, e.de, a.EntryID, memo.EntryID)
	e.Problem(409, "ledger.item_reserved", "POST", "/v1/clearings", body, "")

	// The owner frees b and settles a, which ends a's reservation.
	if err := in(func(ctx context.Context) error {
		if err := postings().Release(ctx, run.ID, []ledger.ItemRef{b}); err != nil {
			return err
		}
		s := settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", a)
		s.ReservedFor = run.ID
		_, err := postings().Settle(ctx, s)
		return err
	}); err != nil {
		t.Fatalf("settle by the owner: %v", err)
	}
	var held int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.item_reservation WHERE tenant_id = $1 AND released_at IS NULL`, []any{e.ten.ID}, &held)
	if held != 0 {
		t.Errorf("reservations held after the release and the settlement: got %d, want 0", held)
	}
	// b is free for another owner; a, now cleared, is not reserved again.
	if err := in(func(ctx context.Context) error { return postings().Reserve(ctx, other, []ledger.ItemRef{b}) }); err != nil {
		t.Errorf("reserve a freed item: got %v, want nil", err)
	}
	if got := problem.From(in(func(ctx context.Context) error { return postings().Reserve(ctx, run, []ledger.ItemRef{a}) })).Code; got != "ledger.item_already_cleared" {
		t.Errorf("reserve a cleared item: got %s, want ledger.item_already_cleared", got)
	}
	// Releasing everything the owner holds, twice, changes nothing the second time.
	for i := range 2 {
		if err := in(func(ctx context.Context) error { return postings().Release(ctx, other.ID, nil) }); err != nil {
			t.Fatalf("release %d: %v", i+1, err)
		}
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"reserve": func(ctx context.Context) error {
			return postings().Reserve(ctx, ledger.Owner{Context: "payments", ID: uuid.New()}, []ledger.ItemRef{b})
		},
		"release":     func(ctx context.Context) error { return postings().Release(ctx, run.ID, nil) },
		"release one": func(ctx context.Context) error { return postings().Release(ctx, run.ID, []ledger.ItemRef{b}) },
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { return postings().Reserve(ctx, other, nil) }); err != nil {
		t.Errorf("reserve nothing: got %v, want nil", err)
	}
}

// TestUnpayable says which items can no longer be settled, and why.
func TestUnpayable(t *testing.T) {
	e := setup(t)
	open := settlePost(t, e, e.de, "2000", "EUR", 1000, "INV-O")
	reversed := settlePost(t, e, e.de, "2000", "EUR", 2000, "INV-R")
	cleared := settlePost(t, e, e.de, "2000", "EUR", 3000, "INV-C")
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		if _, err := postings().Reverse(ctx, reversed.EntryID, nil, nil); err != nil {
			return err
		}
		_, err := postings().Settle(ctx, settlementOf(t, e, e.de, "2000", "EUR", "2026-04-15", cleared))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expense := ledger.ItemRef{FiscalYear: open.FiscalYear, EntryID: open.EntryID, LineNo: 1}
	call := func(ctx context.Context) error {
		got, err := ledger.OpenItems{}.Unpayable(ctx, []ledger.ItemRef{open, reversed, cleared, expense})
		if err != nil {
			return err
		}
		want := map[ledger.ItemRef]string{reversed: ledger.ItemReversed, cleared: ledger.ItemNotOpen, expense: ledger.ItemNotOpen}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("unpayable: got %v, want %v", got, want)
		}
		return nil
	}
	if call(context.Background()) == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "unpayable", call)
}
