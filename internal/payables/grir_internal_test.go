package payables

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestSimulatedPriceDifferenceMoves: a proposal that changes the price
// difference account moves an invoice on an order valued through GR/IR
// once, with the amount of its order items (what the change exposes);
// items of a directly valued order, which post their own account, move
// nothing; an exposed amount out of range fails the simulation.
func TestSimulatedPriceDifferenceMoves(t *testing.T) {
	item, other := int32(1), uuid.New()
	before, after := &config.Company{}, &config.Company{}
	before.AccountDetermination.PriceDifference, after.AccountDetermination.PriceDifference = "590000", "591000"
	invoice := Invoice{Items: []Item{{POItemNo: &item, Amount: 1200}, {POItemNo: &item, Amount: 300}, {AccountID: &other, Amount: 500}}}
	t.Run("gr_ir", func(t *testing.T) {
		ta := &tally{moves: map[string]*Posting{}}
		ta.postingMoves("2000", before, after, replayed{Invoice: invoice, Order: procurement.Order{Valuation: procurement.ValuationGRIR}, Conversion: inEUR})
		p := ta.moves["2000/account_determination.price_difference"]
		if p == nil {
			t.Fatal("price difference move: got none, want one")
		}
		if p.Invoices != 1 {
			t.Errorf("invoices: got %d, want 1", p.Invoices)
		}
		if !slices.Equal(p.amounts, []money.Minor{1500}) {
			t.Errorf("amounts: got %v, want [1500], the order items'", p.amounts)
		}
	})
	t.Run("direct", func(t *testing.T) {
		ta := &tally{moves: map[string]*Posting{}}
		ta.postingMoves("2000", before, after, replayed{Invoice: invoice, Order: procurement.Order{Valuation: procurement.ValuationDirect}, Conversion: inEUR})
		if p := ta.moves["2000/account_determination.price_difference"]; p != nil {
			t.Errorf("price difference move: got %+v, want none", p)
		}
	})
	t.Run("out of range", func(t *testing.T) {
		huge := Invoice{Items: []Item{{POItemNo: &item, Amount: math.MaxInt64}, {POItemNo: &item, Amount: 1}}}
		ta := &tally{moves: map[string]*Posting{}}
		for range 2 { // the first overflow is the one kept
			ta.postingMoves("2000", before, after, replayed{Invoice: huge, Order: procurement.Order{Valuation: procurement.ValuationGRIR}, Conversion: inEUR})
		}
		if !errors.Is(ta.err, money.ErrOverflow) {
			t.Errorf("error: got %v, want %v", ta.err, money.ErrOverflow)
		}
		if p := ta.moves["2000/account_determination.price_difference"]; p != nil {
			t.Errorf("price difference move: got %+v, want none", p)
		}
	})
}

// orders answers with one order, or with err.
type orders struct {
	order procurement.Order
	found bool
	err   error
}

func (o orders) Order(context.Context, uuid.UUID) (procurement.Order, bool, error) {
	return o.order, o.found, o.err
}

func (orders) LockItems(context.Context, uuid.UUID, []int32) error { return nil }

// TestInvoiceOrder: an invoice clears GR/IR only on an order valued
// through GR/IR; an order its context does not have, or cannot read, is
// refused, as is an item on an order item the order no longer has.
func TestInvoiceOrder(t *testing.T) {
	po, item := uuid.New(), int32(3)
	v := Invoice{PurchaseOrderID: &po, DocumentNumber: "SI1"}
	broken := errors.New("broken")
	for name, c := range map[string]struct {
		orders orders
		grir   bool
		err    string
	}{
		"gr_ir":     {orders{procurement.Order{Valuation: procurement.ValuationGRIR}, true, nil}, true, ""},
		"direct":    {orders{procurement.Order{Valuation: procurement.ValuationDirect}, true, nil}, false, ""},
		"not found": {orders{}, false, "payables.order_not_found"},
		"failing":   {orders{err: broken}, false, "platform.internal"},
	} {
		t.Run(name, func(t *testing.T) {
			o, err := (&Endpoints{Orders: c.orders}).invoiceOrder(context.Background(), v)
			if c.err == "" && err != nil {
				t.Errorf("error: got %v, want none", err)
			}
			if got := problem.From(err).Code; c.err != "" && (err == nil || got != c.err) {
				t.Errorf("error: got %v, want %s", err, c.err)
			}
			if got := o != nil; got != c.grir {
				t.Errorf("order through GR/IR: got %v, want %v", got, c.grir)
			}
		})
	}
	if o, err := (&Endpoints{}).invoiceOrder(context.Background(), Invoice{}); o != nil || err != nil {
		t.Errorf("invoice without an order: got %v %v, want nil nil", o, err)
	}
	_, err := grirItem(procurement.Order{DocumentNumber: "4500000001"}, KindInvoice, Item{POItemNo: &item}, 0)
	if got := problem.From(err).Code; got != "payables.order_item_missing" {
		t.Errorf("order item missing: got %v, want payables.order_item_missing", err)
	}
}

// TestTaxDateHumanOnly: an agent may not set a tax date (decision P6); a
// change that keeps a person's is not the agent's, one that sets, changes
// or clears it is.
func TestTaxDateHumanOnly(t *testing.T) {
	agent := reqctx.WithPrincipal(context.Background(), reqctx.Principal{Kind: reqctx.KindAgent})
	day := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	_, err := (&Endpoints{}).validTaxDate(agent, nil, Input{TaxDate: &day})
	if got := problem.From(err); got == nil || got.Code != "payables.tax_date_human_only" {
		t.Errorf("an agent's tax date: got %v, want payables.tax_date_human_only", err)
	}
	if got, err := (&Endpoints{}).validTaxDate(agent, nil, Input{TaxDate: &day, change: true, storedTaxDate: &day}); err != nil || !got.Equal(day) {
		t.Errorf("a kept tax date: got %v %v, want %s", got, err, day)
	}
	other := day.AddDate(0, 0, 1)
	for name, in := range map[string]Input{
		"changed": {TaxDate: &other, change: true, storedTaxDate: &day},
		"cleared": {change: true, storedTaxDate: &day},
		"set":     {TaxDate: &day, change: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&Endpoints{}).validTaxDate(agent, nil, in)
			if got := problem.From(err); got == nil || got.Code != "payables.tax_date_human_only" {
				t.Errorf("an agent's change of the tax date: got %v, want payables.tax_date_human_only", err)
			}
		})
	}
}
