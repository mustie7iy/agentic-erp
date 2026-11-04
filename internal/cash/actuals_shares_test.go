package cash_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// sharesOf renders shares as "line date bank match item (name) amount
// currency/exponent".
func sharesOf(shares []cash.ActualShare) []string {
	out := make([]string, len(shares))
	for i, s := range shares {
		out[i] = fmt.Sprintf("%d %s %s %s %s (%s) %d %s/%d", s.LineNo, s.ValueDate.Format(time.DateOnly), s.HouseBankCode, s.Match, s.Item, s.ItemName, s.Amount,
			s.Currency, s.Exponent)
	}
	return out
}

// TestActualShares: the shares the analytical store holds are every
// statement line's, of every value date, classified as the actuals
// classify them (ADR-0051 §11, T53j1): read as the caller in a consistent
// read, and as the extract in the context's transaction, alike. A legal
// entity the caller cannot act in is not found; one the directory does
// not know has none for the extract.
func TestActualShares(t *testing.T) {
	e := setupActuals(t)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	endpoints.Consistent = inRequest{}
	var asCaller, asExtract, unknown []cash.ActualShare
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		if asCaller, err = endpoints.ActualShares(ctx, e.le); err != nil {
			return err
		}
		if asExtract, err = endpoints.ExtractActualShares(ctx, e.le); err != nil {
			return err
		}
		unknown, err = endpoints.ExtractActualShares(ctx, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"1 2026-04-12 MAIN unmatched REC_CUSTOMERS (Customer receipts) 1000 CAD/2",
		"2 2026-04-13 MAIN payment OP_RENT (Rent) -50000 CAD/2",
		"2 2026-04-13 MAIN payment CAPEX (Capital expenditure) -30000 CAD/2",
		"3 2026-04-14 MAIN payment TAX (Taxes) -10100 CAD/2",
		"4 2026-04-14 MAIN posting_rule BANK_CHARGES (Bank charges) -1500 CAD/2",
		"5 2026-04-15 MAIN unmatched REC_CUSTOMERS (Customer receipts) 200000 CAD/2",
		"6 2026-04-15 MAIN unmatched OP_SUPPLIERS (Supplier payments) -5000 CAD/2",
		"7 2026-04-16 MAIN excluded OP_SUPPLIERS (Supplier payments) -700 CAD/2",
		"8 2026-04-16 MAIN incoming REC_CUSTOMERS (Customer receipts) 30000 CAD/2",
		"1 2026-04-16 USD unmatched OP_SUPPLIERS (Supplier payments) -10 USD/2",
		"9 2026-04-17 MAIN unmatched REC_CUSTOMERS (Customer receipts) 2000 CAD/2",
	}
	if got := sharesOf(asCaller); !slices.Equal(got, want) {
		t.Errorf("as the caller: got %q, want %q", got, want)
	}
	if got := sharesOf(asExtract); !slices.Equal(got, want) {
		t.Errorf("as the extract: got %q, want %q", got, want)
	}
	if len(unknown) != 0 {
		t.Errorf("an unknown legal entity: got %q, want none", sharesOf(unknown))
	}
	err := apitest.InRequest(t, e.Tenant, e.Reader(t, "reader of 1000", e.US), func(ctx context.Context) error {
		_, err := endpoints.ActualShares(ctx, e.le)
		return err
	})
	if p, ok := errors.AsType[*problem.Problem](err); !ok || p.Status != 404 {
		t.Errorf("a legal entity the caller cannot act in: got %v, want 404", err)
	}
}

// TestActualSharesFailures calls the shares outside a unit of work,
// without a consistent read or with one that cannot begin, and with each
// of their database statements failing in turn.
func TestActualSharesFailures(t *testing.T) {
	e := setupActuals(t)
	endpoints := func(consistent cash.Consistency) *cash.Endpoints {
		c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
		c.Consistent = consistent
		return c
	}
	caller := func(ctx context.Context) error {
		_, err := endpoints(inRequest{}).ActualShares(ctx, e.le)
		return err
	}
	extract := func(ctx context.Context) error {
		_, err := endpoints(nil).ExtractActualShares(ctx, e.le)
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
	for name, c := range map[string]struct {
		consistent cash.Consistency
		want       string
	}{"no consistent read": {nil, "need a consistent read"}, "not begun": {notBegun{}, "pool exhausted"}} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := endpoints(c.consistent).ActualShares(ctx, e.le)
				return err
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}
