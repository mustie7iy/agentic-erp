package credit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/credit"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// ordered answers the sales context's read of a customer's open orders:
// so many orders coming to the gross amount in the company's currency,
// or its error.
type ordered struct {
	orders int32
	gross  money.Minor
	err    error
}

func (o ordered) OpenOrders(context.Context, uuid.UUID, uuid.UUID) (int32, money.Minor, error) {
	return o.orders, o.gross, o.err
}

// TestExposureHasTwoParts: the exposure is the customer's open items and
// its confirmed orders not yet billed together, each given beside their
// sum; what is available and whether an amount is within the limit
// follow from the sum (T77b; product owner, 2026-11-12). The orders come
// from the sales context, whose own tests read real ones through the
// API (internal/sales/credit_test.go).
func TestExposureHasTwoParts(t *testing.T) {
	e := setup(t)
	e.story(t) // Northwind owes 290.00 EUR
	e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	h := endpoints(t)
	h.Orders = ordered{orders: 2, gross: 15000}
	read := func(t *testing.T, customer uuid.UUID, amount string) *gen.CreditExposure {
		t.Helper()
		params := gen.GetCreditExposureParams{LegalEntityID: e.DE, CustomerID: customer}
		if amount != "" {
			params.Amount = gen.NewOptString(amount)
		}
		var out *gen.CreditExposure
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			res, err := h.GetCreditExposure(ctx, params)
			out, _ = res.(*gen.CreditExposure)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	parts := func(x *gen.CreditExposure) string {
		return fmt.Sprintf("%d item(s) %s %s + %d order(s) %s %s = %s %s", x.OpenItemCount, x.OpenItems.Amount, x.OpenItems.Currency,
			x.OpenOrderCount, x.OpenOrders.Amount, x.OpenOrders.Currency, x.Exposure.Amount, x.Exposure.Currency)
	}
	t.Run("the parts and their sum", func(t *testing.T) {
		x := read(t, e.north, "")
		if p := parts(x); p != "4 item(s) 290.00 EUR + 2 order(s) 150.00 EUR = 440.00 EUR" {
			t.Errorf("got %s, want 4 item(s) 290.00 EUR + 2 order(s) 150.00 EUR = 440.00 EUR", p)
		}
		if a := x.Available.Value.Amount; a != "60.00" {
			t.Errorf("available: got %s, want 60.00: the limit of 500.00 less the exposure", a)
		}
	})
	for _, c := range []struct {
		name, amount string
		within       bool
	}{
		{"the amount that reaches the limit", "60.00", true},
		{"a cent more", "60.01", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if x := read(t, e.north, c.amount); x.IsWithinLimit != c.within {
				t.Errorf("is_within_limit: got %v, want %v", x.IsWithinLimit, c.within)
			}
		})
	}
	t.Run("without a limit the orders are shown all the same", func(t *testing.T) {
		x := read(t, e.east, "")
		if p := parts(x); p != "1 item(s) -30.00 EUR + 2 order(s) 150.00 EUR = 120.00 EUR" {
			t.Errorf("got %s, want 1 item(s) -30.00 EUR + 2 order(s) 150.00 EUR = 120.00 EUR", p)
		}
	})
	t.Run("through the API, a customer without orders", func(t *testing.T) {
		x := e.exposure(t, e.DE, e.north, "")
		got := fmt.Sprintf("%s + %v order(s) %s = %s", amount(t, x, "open_items"), x["open_order_count"], amount(t, x, "open_orders"), amount(t, x, "exposure"))
		if got != "290.00 EUR + 0 order(s) 0.00 EUR = 290.00 EUR" {
			t.Errorf("got %s, want 290.00 EUR + 0 order(s) 0.00 EUR = 290.00 EUR", got)
		}
	})
}

// TestCheck: the check the sales context asks at an order's confirmation
// compares the amount and the exposure, both parts, with the limit; a
// customer without a limit is within it, and nothing is read for it.
func TestCheck(t *testing.T) {
	e := setup(t)
	e.story(t) // Northwind owes 290.00 EUR
	e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	check := func(t *testing.T, h *credit.Endpoints, customer uuid.UUID, amount money.Minor) (credit.Exposure, error) {
		t.Helper()
		var x credit.Exposure
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			x, err = h.Check(ctx, e.DE, customer, amount)
			return err
		})
		return x, err
	}
	h := endpoints(t)
	h.Orders = ordered{orders: 1, gross: 15000}
	for _, c := range []struct {
		name   string
		amount money.Minor
		within bool
	}{
		{"the amount that reaches the limit", 6000, true},
		{"a cent more", 6001, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			x, err := check(t, h, e.north, c.amount)
			if err != nil {
				t.Fatal(err)
			}
			if x.Within != c.within {
				t.Errorf("within: got %v, want %v", x.Within, c.within)
			}
			if got := fmt.Sprintf("%d + %d = %d of %d", x.Owed, x.Ordered, x.Total, x.Limit.Amount); got != "29000 + 15000 = 44000 of 50000" {
				t.Errorf("exposure: got %s, want 29000 + 15000 = 44000 of 50000", got)
			}
		})
	}
	t.Run("without a limit the exposure is not read", func(t *testing.T) {
		unread := endpoints(t)
		unread.Orders = ordered{err: errors.New("the orders are not to be read")}
		x, err := check(t, unread, e.east, 1_000_000_00)
		if err != nil {
			t.Fatalf("got %v, want it answered without reading the exposure", err)
		}
		if !x.Within || x.Limit != nil {
			t.Errorf("got within %v, limit %v, want within the limit and no limit", x.Within, x.Limit)
		}
	})
	t.Run("the orders cannot be read", func(t *testing.T) {
		failing := endpoints(t)
		failing.Orders = ordered{err: problem.Unprocessable("sales.unknown_tax_code", "An order's value cannot be calculated.")}
		_, err := check(t, failing, e.north, 1)
		if p := problem.From(err); p.Code != "sales.unknown_tax_code" {
			t.Errorf("got %v, want the sales context's refusal, sales.unknown_tax_code", err)
		}
	})
	t.Run("outside a unit of work", func(t *testing.T) {
		if _, err := h.Check(context.Background(), e.DE, e.north, 1); err == nil {
			t.Error("got nil error, want an error")
		}
	})
}

// TestCheckHoldsTheCustomer: a check holds the customer in the company
// until its transaction ends, so a second check of the customer waits
// for it: two orders of one customer are checked one after the other.
// It holds a customer without a limit too, so a limit is not set beside
// a confirmation under way, which the next check would then not count;
// and a change of the limit waits for a check (ADR-0066, As built T77b;
// ADR-0008).
func TestCheckHoldsTheCustomer(t *testing.T) {
	e := setup(t)
	e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	h := endpoints(t)
	check := func(ctx context.Context) error {
		_, err := h.Check(ctx, e.DE, e.north, 100)
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, check, check)
	if first != nil {
		t.Errorf("the first check: got %v, want it answered", first)
	}
	if second != nil {
		t.Errorf("the second check: got %v, want it answered once the first ended", second)
	}
	t.Run("a limit set while a customer without one is checked", func(t *testing.T) {
		unlimited := func(ctx context.Context) error {
			_, err := h.Check(ctx, e.DE, e.east, 100)
			return err
		}
		set := func(ctx context.Context) error {
			_, err := h.SetLimit(ctx, credit.LimitInput{LegalEntityID: e.DE, CustomerID: e.east, Amount: "100.00", Currency: "EUR"})
			return err
		}
		checked, limited := apitest.Serialized(t, e.Tenant, e.Tenant.Human, unlimited, set)
		if checked != nil {
			t.Errorf("the check: got %v, want it answered", checked)
		}
		if limited != nil {
			t.Errorf("setting the limit: got %v, want it set once the check ended", limited)
		}
	})
	t.Run("a limit changed while the customer is checked", func(t *testing.T) {
		var id uuid.UUID
		pgtest.Scan(t, `SELECT id FROM credit.credit_limit WHERE tenant_id = $1 AND customer_id = $2`, []any{e.Tenant.ID, e.north}, &id)
		change := func(ctx context.Context) error {
			_, err := h.ChangeLimit(ctx, id, func(int32) error { return nil }, "600.00", "EUR")
			return err
		}
		checked, changed := apitest.Serialized(t, e.Tenant, e.Tenant.Human, check, change)
		if checked != nil {
			t.Errorf("the check: got %v, want it answered", checked)
		}
		if changed != nil {
			t.Errorf("the change: got %v, want it made once the check ended", changed)
		}
	})
}
