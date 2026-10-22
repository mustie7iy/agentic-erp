package payments_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestCashPositionOutsideRequest: the cash position's reads run in the
// request's transaction only; their data is read end to end in
// TestForeignCurrencyHouseBankEndToEnd and the cash context's tests.
func TestCashPositionOutsideRequest(t *testing.T) {
	c, ctx := payments.CashPosition{}, context.Background()
	for name, call := range map[string]func() error{
		"banks":          func() error { _, err := c.Banks(ctx, uuid.Nil, time.Now()); return err },
		"released":       func() error { _, err := c.Released(ctx, nil); return err },
		"pending":        func() error { _, err := c.Pending(ctx, uuid.Nil); return err },
		"house banks":    func() error { _, err := c.HouseBanks(ctx, uuid.Nil); return err },
		"statement line": func() error { _, _, err := c.StatementLine(ctx, uuid.Nil, 1); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
		})
	}
}
