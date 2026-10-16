package payables

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestSimulatedPriceDifferenceMoves: a proposal that changes the price
// difference account moves the order items an invoice books through GR/IR,
// with their amounts; items of a directly valued order, which post their
// own account, move nothing.
func TestSimulatedPriceDifferenceMoves(t *testing.T) {
	item, other := int32(1), uuid.New()
	before, after := &config.Company{}, &config.Company{}
	before.AccountDetermination.PriceDifference, after.AccountDetermination.PriceDifference = "590000", "591000"
	invoice := Invoice{Items: []Item{{POItemNo: &item, Amount: 1200}, {AccountID: &other, Amount: 500}}}
	for valuation, want := range map[string]int{procurement.ValuationGRIR: 1, procurement.ValuationDirect: 0} {
		t.Run(valuation, func(t *testing.T) {
			ta := &tally{moves: map[string]*Posting{}}
			ta.postingMoves("2000", before, after, replayed{Invoice: invoice, Order: procurement.Order{Valuation: valuation}, Conversion: inEUR})
			p := ta.moves["2000/account_determination.price_difference"]
			if got := map[bool]int{true: 1, false: 0}[p != nil]; got != want {
				t.Fatalf("price difference moves: got %d, want %d", got, want)
			}
			if p != nil && (p.Invoices != 1 || p.amounts[0] != 1200) {
				t.Errorf("price difference move: got %d invoices of %v, want 1 of 1200", p.Invoices, p.amounts)
			}
		})
	}
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
			if got := problem.From(err).Code; err != nil && got != c.err {
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
	_, err := grirItem(procurement.Order{DocumentNumber: "4500000001"}, Item{POItemNo: &item})
	if got := problem.From(err).Code; got != "payables.order_item_missing" {
		t.Errorf("order item missing: got %v, want payables.order_item_missing", err)
	}
}
