package cash

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The forecast's receipts from customers (ADR-0051 §4, §5, As built
// T53l1): the customer open items receivables publishes and the
// confirmed sales orders not yet billed sales publishes, read through the
// two ports below, which the composition root answers from those
// contexts' published reads (and collections', for what it holds back),
// so none of them imports cash and cash imports none of them.

// Receivables is what the forecast reads of customers' open items
// (receivables and collections contexts).
type Receivables interface {
	// Receivables are the legal entity's ordinary customer open items,
	// each marked Held when collections holds it back on the day (a
	// dunning block of it or its customer in force then, or an open
	// dispute case).
	Receivables(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]Receivable, error)
}

// SalesOrders is what the forecast reads of sales orders (sales context).
type SalesOrders interface {
	// Orders are the legal entity's confirmed sales orders not blocked for
	// credit with a part not yet billed.
	Orders(ctx context.Context, legalEntity uuid.UUID) ([]SalesOrder, error)
}

// Receivable is a customer open item as the forecast reads it: its
// customer and currency, its net due date, its open amount signed as
// booked (a credit negative), whether collections holds it back, and its
// journal entry.
type Receivable struct {
	Customer uuid.UUID
	Currency string
	Due      time.Time
	Amount   money.Minor
	Held     bool
	Document Document
}

// SalesOrder is a confirmed sales order's part not yet billed: its
// customer and currency, its order date and the delivery date it asks for
// (nil for none), its payment term's net days, what is open of it with
// its output tax, and the order.
type SalesOrder struct {
	Customer     uuid.UUID
	Currency     string
	OrderDate    time.Time
	DeliveryDate *time.Time
	NetDays      int
	Gross        money.Minor
	Document     Document
}

// Sources and the document kind of the customers' receipts.
const (
	SourceReceivable        = "receivable"
	SourceReceivableBlocked = "receivable_blocked"
	SourceSalesOrder        = "sales_order"
	DocSalesOrder           = "sales_order"
)

// blockedSource reports whether a flow's source is one the totals leave
// out unless asked (include_blocked): what blocked suppliers are owed and
// what customers held back owe.
func blockedSource(source string) bool {
	return source == SourcePayableBlocked || source == SourceReceivableBlocked
}

// receiptFlows are the legal entity's expected receipts: its customers'
// open items (receivableFlows) and its open sales orders (orderFlows).
func (e *Endpoints) receiptFlows(ctx context.Context, r reader, legalEntity uuid.UUID) ([]datedFlow, error) {
	items, err := e.Receivables.Receivables(ctx, legalEntity, r.read)
	if err != nil {
		return nil, fmt.Errorf("cash: customer open items of %s: %w", legalEntity, err)
	}
	orders, err := e.SalesOrders.Orders(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: open sales orders of %s: %w", legalEntity, err)
	}
	return append(r.receivableFlows(items), r.orderFlows(orders)...), nil
}

// receiving is the customer's items one receipt is expected to pay: of
// one customer, currency and source.
type receiving struct {
	customer         uuid.UUID
	currency, source string
}

// receivableFlows are the receipts expected for the customer open items
// (ADR-0051 §5, As built T53l1): each item on the working day on or after
// its net due date, an overdue one on or after the read date, and per
// customer, currency and source the items of that day received together,
// those carried from before with them: the day the money moves is fixed
// before the items are netted, so a credit dated on the working day an
// invoice of the weekend moves to is deducted from it. A day whose items come to nothing
// or less (a credit memo, a payment on account, more credits than
// invoices) is carried to the customer's next day, as the customer would
// deduct the credit from its next payment; what is still a credit after
// its last day is no receipt and is left out. Items collections holds
// back are netted alike as receivable_blocked, on their day as it is
// (the read date when overdue), not moved. Each receipt is classified by
// the inflow default (no lines, no planning group), as the actuals
// classify a statement line matched as incoming, so forecast and actuals
// compare the same liquidity items (As built T53l1).
func (r reader) receivableFlows(items []Receivable) []datedFlow {
	groups := map[receiving][]int{}
	var order []receiving
	for i, it := range items {
		k := receiving{customer: it.Customer, currency: it.Currency, source: SourceReceivable}
		if it.Held {
			k.source = SourceReceivableBlocked
		}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	var out []datedFlow
	for _, k := range order {
		out = append(out, r.received(k, items, groups[k])...)
	}
	return out
}

// received are the receipts of one customer's items (receiving), in the
// order of their days.
func (r reader) received(k receiving, items []Receivable, at []int) []datedFlow {
	day := func(i int) time.Time {
		d := later(items[i].Due, r.read)
		if k.source == SourceReceivable {
			d = r.onOrAfter(d)
		}
		return d
	}
	slices.SortStableFunc(at, func(a, b int) int { return day(a).Compare(day(b)) })
	var out []datedFlow
	var carried []int
	for n, i := range at {
		carried = append(carried, i)
		if n+1 < len(at) && day(at[n+1]).Equal(day(i)) {
			continue
		}
		var total money.Minor
		parts, docs := make([]part, len(carried)), make([]paidDocument, len(carried))
		for j, c := range carried {
			total = r.sums.add(total, items[c].Amount)
			parts[j] = part{amount: items[c].Amount}
			docs[j] = paidDocument{Document: items[c].Document, amount: items[c].Amount}
		}
		if total <= 0 {
			continue
		}
		customer := k.customer
		out = append(out, datedFlow{flow: flow{date: day(i), source: k.source, certainty: CertaintyExpected, amount: total}, currency: k.currency,
			parts: parts, customer: &customer, documents: docs})
		carried = nil
	}
	return out
}

// orderFlows are the receipts expected for the open sales orders (ADR-0051
// §5, As built T53l1), one per order: billed on the delivery date the
// order asks for, else on its order date plus the company's
// order_lead_days (as a purchase order's invoice is expected), due its
// payment term's net days after (no cash discount is assumed: whether a
// customer takes one is the customer's choice), the read date when that
// is past, received on the working day on or after it; at what is open
// of the order with its output tax, planned, classified by the inflow
// default as a customer item's receipt is.
func (r reader) orderFlows(orders []SalesOrder) []datedFlow {
	out := make([]datedFlow, 0, len(orders))
	for _, o := range orders {
		billed := o.OrderDate.AddDate(0, 0, r.settings.OrderLead())
		if o.DeliveryDate != nil {
			billed = *o.DeliveryDate
		}
		customer := o.Customer
		out = append(out, datedFlow{flow: flow{date: r.onOrAfter(later(billed.AddDate(0, 0, o.NetDays), r.read)), source: SourceSalesOrder,
			certainty: CertaintyPlanned, amount: o.Gross}, currency: o.Currency, parts: []part{{amount: o.Gross}}, customer: &customer,
			documents: []paidDocument{{Document: o.Document, amount: o.Gross}}})
	}
	return out
}

// later is the later of two days.
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
