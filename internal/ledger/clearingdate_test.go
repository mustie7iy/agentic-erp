package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// refusedBefore checks err, a clearing's refusal: 422
// ledger.clearing_before_item naming the request's date field and the
// latest posting date of the items.
func refusedBefore(t *testing.T, err error, field, latest string) {
	t.Helper()
	p := problem.From(err)
	if p.Status != 422 || p.Code != "ledger.clearing_before_item" {
		t.Fatalf("got %v, want 422 ledger.clearing_before_item", err)
	}
	if len(p.Errors) != 1 || p.Errors[0].Field != field || p.Errors[0].Code != "before_item" {
		t.Errorf("fields: got %+v, want %s before_item", p.Errors, field)
	}
	if want := "on or after " + latest + ", the latest posting date of the items"; len(p.Errors) == 1 && p.Errors[0].Message != want {
		t.Errorf("message: got %q, want %q", p.Errors[0].Message, want)
	}
}

// TestClearingDate (review:T75c, PO 2026-11-12; ADR-0006 As built): a
// clearing is not dated before the latest posting date of the items it
// clears. Every path that posts a clearing entry refuses a date a day
// earlier with 422 ledger.clearing_before_item naming its own date field,
// and accepts the day itself: a manual clearing, a settlement, a receipt
// applied (the receipt's own line counts too), a bank posting that clears
// payments, and a down payment clearing (TestClearDownPayments). A
// migrated clearing keeps the source system's date and a reversal its
// own: neither is held to the rule.
func TestClearingDate(t *testing.T) {
	r := receiptSetup(t, withReceivables)
	north := r.newCustomer(t, "BP00000031", true)
	// attempt runs fn dated day in a request that is rolled back.
	attempt := func(fn func(ctx context.Context, day string) error, day string) error {
		return apitest.InRequest(t, r.ten, r.ten.Human, func(ctx context.Context) error { return fn(ctx, day) })
	}
	// edges refuses the path a day before the latest item and accepts it on the day.
	edges := func(t *testing.T, field, latest, before string, fn func(ctx context.Context, day string) error) {
		t.Helper()
		refusedBefore(t, attempt(fn, before), field, latest)
		if err := attempt(fn, latest); err != nil {
			t.Errorf("dated %s, the latest item's posting date: got %v, want it cleared", latest, err)
		}
	}

	t.Run("a manual clearing", func(t *testing.T) {
		invoice, credit := r.owed(t, north, 10000, "EUR", "2026-03-10"), r.owed(t, north, -10000, "EUR", "2026-03-04")
		body := func(day string) string {
			return clearing(r.de, day, ref{map[string]any{"fiscal_year": 2026, "id": invoice.EntryID.String()}, 1},
				ref{map[string]any{"fiscal_year": 2026, "id": credit.EntryID.String()}, 1})
		}
		p := r.in(t).Problem(422, "ledger.clearing_before_item", "POST", "/v1/clearings", body("2026-03-09"), "")
		errs, _ := p["errors"].([]any)
		if len(errs) != 1 || errs[0].(map[string]any)["field"] != "clearing_date" {
			t.Errorf("fields: got %v, want clearing_date", p["errors"])
		}
		r.in(t).Must(201, "POST", "/v1/clearings", body("2026-03-10"), "")
	})
	t.Run("a settlement", func(t *testing.T) {
		item := settlePost(t, r.env, r.de, "2000", "EUR", 10000, "INV-DATE") // posted 1 March
		edges(t, "posting_date", "2026-03-01", "2026-02-28", func(ctx context.Context, day string) error {
			_, err := postings().Settle(ctx, settlementOf(t, r.env, r.de, "2000", "EUR", day, item))
			return err
		})
	})
	t.Run("a receipt applied before its item was posted", func(t *testing.T) {
		receipt, item := r.received(t, 5000, "EUR", "2026-03-20"), r.owed(t, north, 5000, "EUR", "2026-03-22")
		edges(t, "posting_date", "2026-03-22", "2026-03-21", func(ctx context.Context, day string) error {
			_, err := postings().ApplyReceipt(ctx, r.application(receipt, north, "EUR", day, item))
			return err
		})
	})
	t.Run("a receipt applied before the receipt was posted", func(t *testing.T) {
		receipt, item := r.received(t, 7000, "EUR", "2026-03-20"), r.owed(t, north, 7000, "EUR", "2026-03-02")
		edges(t, "posting_date", "2026-03-20", "2026-03-19", func(ctx context.Context, day string) error {
			_, err := postings().ApplyReceipt(ctx, r.application(receipt, north, "EUR", day, item))
			return err
		})
	})
	t.Run("a bank posting that clears a payment", func(t *testing.T) {
		expense := r.account(t, "2000", "520000")
		payment, _ := r.in(t).Must(201, "POST", "/v1/journal-entries",
			entry(r.de, "2026-03-21", "EUR", line{expense, "20.00", ""}, line{r.incoming.String(), "-20.00", ""}), "")
		edges(t, "posting_date", "2026-03-21", "2026-03-20", func(ctx context.Context, day string) error {
			_, err := postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: r.de, DocumentType: "SA", PostingDate: date(day), Currency: "EUR",
				BankAccountID: r.bank, Amount: -2000, ClearingAccountID: r.incoming,
				Entries: []ledger.ItemRef{{FiscalYear: 2026, EntryID: uuid.MustParse(payment["id"].(string))}}, SourceContext: "payments", SourceDocumentID: uuid.New()})
			return err
		})
	})
	t.Run("a migrated clearing keeps its date", func(t *testing.T) {
		bank, expense, ap, sup := r.account(t, "2000", "100000"), r.account(t, "2000", "520000"), r.account(t, "2000", "211000"), r.supplier.String()
		inv, _ := r.in(t).Must(201, "POST", "/v1/journal-entries", entry(r.de, "2026-03-10", "EUR", line{expense, "10.00", ""}, line{ap, "-10.00", sup}), "")
		pay, _ := r.in(t).Must(201, "POST", "/v1/journal-entries", entry(r.de, "2026-03-20", "EUR", line{ap, "10.00", sup}, line{bank, "-10.00", ""}), "")
		p := migratedPair{uuid.MustParse(inv["id"].(string)), uuid.MustParse(pay["id"].(string))}
		// The source system's clearing date, before both items' posting dates.
		if err := attempt(func(ctx context.Context, day string) error {
			_, err := postings().Clear(ctx, r.de, date(day), ledger.ItemRef{FiscalYear: 2026, EntryID: p.payment, LineNo: 1}, p.items())
			return err
		}, "2026-03-05"); err != nil {
			t.Errorf("a migrated clearing dated before its items: got %v, want it recorded with the source system's date", err)
		}
	})
	t.Run("a reversal keeps its date", func(t *testing.T) {
		item := r.owed(t, north, 900, "EUR", "2026-03-10")
		if err := attempt(func(ctx context.Context, day string) error {
			on := date(day)
			_, err := postings().Reverse(ctx, item.EntryID, &on, nil)
			return err
		}, "2026-03-09"); err != nil {
			t.Errorf("a reversal dated before its original: got %v, want it posted as before: the rule is a clearing's", err)
		}
	})
}
