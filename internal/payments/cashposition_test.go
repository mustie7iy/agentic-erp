package payments_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestCashPositionOutsideRequest: the cash position's reads run in the
// request's transaction only; their data is read end to end in
// TestForeignCurrencyHouseBankEndToEnd and the cash context's tests.
func TestCashPositionOutsideRequest(t *testing.T) {
	c, ctx := payments.CashPosition{}, context.Background()
	for name, call := range map[string]func() error{
		"banks":            func() error { _, err := c.Banks(ctx, uuid.Nil, time.Now()); return err },
		"released":         func() error { _, err := c.Released(ctx, nil); return err },
		"pending":          func() error { _, err := c.Pending(ctx, uuid.Nil); return err },
		"house banks":      func() error { _, err := c.HouseBanks(ctx, uuid.Nil); return err },
		"statement line":   func() error { _, _, err := c.StatementLine(ctx, uuid.Nil, 1); return err },
		"house bank codes": func() error { _, err := c.HouseBankCodes(ctx, []uuid.UUID{uuid.Nil}); return err },
		"house bank accounts": func() error {
			_, err := c.HouseBankAccounts(ctx, []uuid.UUID{uuid.Nil})
			return err
		},
		"payables": func() error { _, err := c.Payables(ctx, uuid.Nil, time.Now()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
		})
	}
}

// TestCashPayable: what the liquidity forecast reads of an item (ADR-0051
// §5): the days a run would pay it on, an invoice's discount tiers first,
// a credit memo's due date only; and what a run on a day pays, less the
// cash discount it then takes and the withholding tax deducted at payment,
// as the proposer computes them.
func TestCashPayable(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d } // constant test dates
	terms := &ledger.ItemTerms{BaselineDate: day("2026-04-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: "2"}, {Days: 20, Percent: "1"}}}
	wht := []ledger.Withholding{{Code: "W1", Posting: config.WithholdingAtPayment, RatePct: "10", Base: 10000}}
	invoice := payments.CashPayable{PayableItem: ledger.PayableItem{Amount: -10000, Terms: terms, Withholding: wht}, Due: terms.NetDueDate()}
	credit := payments.CashPayable{PayableItem: ledger.PayableItem{Amount: 2000, Terms: terms}, Due: day("2026-04-03")}
	want := func(what string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	dates := func(ds []time.Time) []string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = d.Format(time.DateOnly)
		}
		return out
	}
	want("an invoice's days", dates(invoice.Dates()), []string{"2026-04-11", "2026-04-21", "2026-05-01"})
	want("a credit memo's days", dates(credit.Dates()), []string{"2026-04-03"})
	for name, c := range map[string]struct {
		item payments.CashPayable
		on   string
		want money.Minor
	}{
		"an invoice in the first tier":  {invoice, "2026-04-11", 8820}, // 100.00 less 2.00, less 10% of 98.00
		"an invoice in the second tier": {invoice, "2026-04-21", 8910}, // 100.00 less 1.00, less 10% of 99.00
		"an invoice at its due date":    {invoice, "2026-05-01", 9000},
		"a credit memo nets":            {credit, "2026-04-05", -1960},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := c.item.Pays(day(c.on))
			if err != nil {
				t.Fatal(err)
			}
			want("pays", got, c.want)
		})
	}
	bad := payments.CashPayable{PayableItem: ledger.PayableItem{Amount: -100, Terms: &ledger.ItemTerms{BaselineDate: day("2026-04-01"), NetDays: 30,
		Discounts: []ledger.Discount{{Days: 10, Percent: "x"}}}}}
	if _, err := bad.Pays(day("2026-04-05")); err == nil {
		t.Error("a percent that is not one: got no error, want the payment refused")
	}
	badRate := payments.CashPayable{PayableItem: ledger.PayableItem{Amount: -100, Withholding: []ledger.Withholding{{Code: "W", Posting: config.WithholdingAtPayment,
		RatePct: "x", Base: 100}}}}
	if _, err := badRate.Pays(day("2026-04-05")); err == nil {
		t.Error("a withholding rate that is not one: got no error, want the payment refused")
	}
}
