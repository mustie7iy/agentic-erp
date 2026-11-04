package cash_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// cellsOf renders cells as "value date +lag currency/exponent item (name)
// actual forecast variance".
func cellsOf(cells []cash.AccuracyCell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = fmt.Sprintf("%s +%d %s/%d %s (%s) %d %d %d", c.ValueDate.Format(time.DateOnly), c.LagDays, c.Currency, c.Exponent, c.Item, c.ItemName,
			c.Actual, c.Forecast, c.Variance)
	}
	return out
}

// accuracySnapshots keeps Maple's forecasts against setupActuals' lines
// (read on 17 April): 15 March's (lag 30 to the 14th), 17 March's whose
// horizon ends on 15 April (so the 16th is not compared at 30), one taken
// on demand on 9 April (lag 7 to the 16th, a blocked supplier's line),
// 13 April's daily one (lag 0, and the 20th, not yet passed) beside one on
// demand that day, and one on demand on 17 April (today, and the 24th).
func (e actualsEnv) accuracySnapshots(t *testing.T) {
	t.Helper()
	e.keptSnapshot(t, "daily", "2026-03-15", "2026-04-14||main|CAD|payable|TAX|-10000")
	short := e.keptSnapshot(t, "daily", "2026-03-17", "2026-04-16||main|CAD|payable|OP_SUPPLIERS|-999")
	e.exec(t, `UPDATE cash.snapshot SET horizon_end = '2026-04-15' WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, short)
	e.keptSnapshot(t, "on_demand", "2026-04-09", "2026-04-16||main|CAD|memo|REC_CUSTOMERS|25000", "2026-04-16||none|CAD|payable_blocked|OP_SUPPLIERS|-300")
	e.keptSnapshot(t, "daily", "2026-04-13", "2026-04-13||main|CAD|payable|OP_RENT|-50000", "2026-04-13||main|CAD|payable|CAPEX|-20000",
		"2026-04-13||main|CAD|memo|REC_CUSTOMERS|500", "2026-04-20||main|CAD|payable|OP_RENT|-7")
	e.keptSnapshot(t, "on_demand", "2026-04-13", "2026-04-13||main|CAD|payable|OP_RENT|-1")
	e.keptSnapshot(t, "on_demand", "2026-04-17", "2026-04-17||main|CAD|memo|REC_CUSTOMERS|1500", "2026-04-24||main|CAD|memo|REC_CUSTOMERS|9")
}

// TestAccuracyCells: the forecast's accuracy compares, per value date at
// each lag of 0, 7 and 30 days, the actuals' shares of the day with the
// lines the company's preferred snapshot taken the lag before forecast
// for it, by currency and liquidity item (ADR-0051 §11, T53j3): only days
// that have passed, of a snapshot taken and forecasting that far, blocked
// suppliers' flows left out; a cell of an actual alone or a forecast alone
// compares it with nothing. Read as the caller in a consistent read, and
// as the extract in the context's transaction, alike. A legal entity the
// caller cannot act in is not found; one the directory does not know has
// none for the extract.
func TestAccuracyCells(t *testing.T) {
	e := setupActuals(t)
	e.accuracySnapshots(t)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	endpoints.Consistent = inRequest{}
	var asCaller, asExtract, unknown []cash.AccuracyCell
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		if asCaller, err = endpoints.AccuracyCells(ctx, e.le); err != nil {
			return err
		}
		if asExtract, err = endpoints.ExtractAccuracyCells(ctx, e.le); err != nil {
			return err
		}
		unknown, err = endpoints.ExtractAccuracyCells(ctx, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-04-13 +0 CAD/2 CAPEX (Capital expenditure) -30000 -20000 -10000",
		"2026-04-13 +0 CAD/2 OP_RENT (Rent) -50000 -50000 0",
		"2026-04-13 +0 CAD/2 REC_CUSTOMERS (Customer receipts) 0 500 -500",
		"2026-04-14 +30 CAD/2 BANK_CHARGES (Bank charges) -1500 0 -1500",
		"2026-04-14 +30 CAD/2 TAX (Taxes) -10100 -10000 -100",
		"2026-04-16 +7 CAD/2 OP_SUPPLIERS (Supplier payments) -700 0 -700",
		"2026-04-16 +7 CAD/2 REC_CUSTOMERS (Customer receipts) 30000 25000 5000",
		"2026-04-16 +7 USD/2 OP_SUPPLIERS (Supplier payments) -10 0 -10",
		"2026-04-17 +0 CAD/2 REC_CUSTOMERS (Customer receipts) 2000 1500 500",
	}
	if got := cellsOf(asCaller); !slices.Equal(got, want) {
		t.Errorf("as the caller: got %q, want %q", got, want)
	}
	if got := cellsOf(asExtract); !slices.Equal(got, want) {
		t.Errorf("as the extract: got %q, want %q", got, want)
	}
	if len(asCaller) == len(want) && (asCaller[0].ID != asExtract[0].ID || asCaller[0].ID == asCaller[1].ID) {
		t.Errorf("cell ids: got %s, %s and %s, want each cell's own, the same on both paths", asCaller[0].ID, asExtract[0].ID, asCaller[1].ID)
	}
	if len(unknown) != 0 {
		t.Errorf("an unknown legal entity: got %q, want none", cellsOf(unknown))
	}
	err := apitest.InRequest(t, e.Tenant, e.Reader(t, "reader of 1000", e.US), func(ctx context.Context) error {
		_, err := endpoints.AccuracyCells(ctx, e.le)
		return err
	})
	if p, ok := errors.AsType[*problem.Problem](err); !ok || p.Status != 404 {
		t.Errorf("a legal entity the caller cannot act in: got %v, want 404", err)
	}
}

// TestAccuracyCellsTooLarge: a cell whose forecast adds up to more than an
// amount can hold is refused, never given saturated.
func TestAccuracyCellsTooLarge(t *testing.T) {
	e := setupActuals(t)
	e.keptSnapshot(t, "daily", "2026-04-13", "2026-04-13||main|CAD|payable|OP_RENT|-9000000000000000000", "2026-04-13||main|CAD|payable|OP_RENT|-9000000000000000000")
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints.ExtractAccuracyCells(ctx, e.le)
		return err
	})
	if p, ok := errors.AsType[*problem.Problem](err); !ok || p.Code != "cash.amount_too_large" {
		t.Errorf("got %v, want 422 cash.amount_too_large", err)
	}
}

// TestAccuracyCellsFailures calls the cells outside a unit of work and with
// each of their database statements failing in turn, as the caller and as
// the extract.
func TestAccuracyCellsFailures(t *testing.T) {
	e := setupActuals(t)
	e.accuracySnapshots(t)
	endpoints := func(consistent cash.Consistency) *cash.Endpoints {
		c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
		c.Consistent = consistent
		return c
	}
	caller := func(ctx context.Context) error {
		_, err := endpoints(inRequest{}).AccuracyCells(ctx, e.le)
		return err
	}
	extract := func(ctx context.Context) error {
		_, err := endpoints(nil).ExtractAccuracyCells(ctx, e.le)
		return err
	}
	for name, read := range map[string]func(ctx context.Context) error{"as the caller": caller, "as the extract": extract} {
		t.Run(name, func(t *testing.T) {
			if err := read(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, read)
		})
	}
}
