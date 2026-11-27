package sales

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CashOrder is a confirmed sales order's part not yet billed as the
// liquidity forecast reads it (ADR-0051 §4, §5, As built T53l1): the
// order, its customer, its currency, the day it was ordered and the
// delivery date it asks for (nil for none), its payment term with the
// term's net days, what is open of it with its output tax (Gross, as the
// customer's credit exposure values it), and its open lines by revenue
// account, from which the forecast derives its liquidity items.
type CashOrder struct {
	ID              uuid.UUID
	Number          string
	Customer        uuid.UUID
	Currency        string
	OrderDate       time.Time
	DeliveryDate    *time.Time
	PaymentTermCode string
	NetDays         int
	Gross           money.Minor
	Lines           []CashOrderLine
}

// CashOrderLine is an open line of a CashOrder: its revenue account's
// number, its net value not yet billed signed as billing would post it
// (a credit, negative) and its output tax code ("" for none).
type CashOrderLine struct {
	Account string
	Net     money.Minor
	TaxCode string
}

// cashOrders reads the company's confirmed orders that are not blocked
// for credit, with what is open of their items as openOrders reads it
// for one customer, and the revenue account of each open item.
const cashOrders = `SELECT o.id, o.document_number, o.customer_id, o.order_date, o.requested_delivery_date, o.currency, o.payment_term_code, x.lines
	FROM sales.sales_order o CROSS JOIN LATERAL (
		SELECT json_agg(json_build_object('item_no', t.item_no, 'net_amount', t.net_amount - p.posted, 'tax_code', t.tax_code,
				'account_id', t.account_id) ORDER BY t.item_no) AS lines
		FROM sales.sales_order_item t CROSS JOIN LATERAL (
			SELECT coalesce(sum(i.net_amount), 0)::bigint AS posted
			FROM sales.billing_document_item i JOIN sales.billing_document b ON b.tenant_id = i.tenant_id AND b.id = i.billing_document_id
			WHERE i.tenant_id = t.tenant_id AND i.sales_order_id = t.sales_order_id AND i.order_item_no = t.item_no AND b.status = 'posted') p
		WHERE t.tenant_id = o.tenant_id AND t.sales_order_id = o.id AND t.removed_in_change IS NULL AND t.net_amount > p.posted) x
	WHERE o.legal_entity_id = $1 AND o.status = 'confirmed' AND o.credit_block <> 'blocked' AND x.lines IS NOT NULL
	ORDER BY o.order_date, o.id`

// cashOrder is a CashOrder as read, before it is valued.
type cashOrder struct {
	CashOrder
	open openOrder
}

// CashOrders is the sales context's published read for the liquidity
// forecast (ADR-0051 §4, As built T53l1): the company's confirmed orders
// with a part not yet posted as a customer invoice, each valued as the
// customer's credit exposure values it (OpenOrders: the open net values
// plus their output tax on the order's date, in the order's currency,
// never converted). An order blocked for credit takes no delivery and is
// left out, as the exposure leaves it out; once released it counts. A
// line whose tax code is no longer an output code of the company on the
// order's date adds no tax rather than refuse the forecast, as a purchase
// order's code that cannot be calculated adds none (ADR-0051 As built
// T53g); the order's other lines keep theirs. Company access is the
// caller's to check. A value out of range (422 sales.amount_too_large) or
// a payment term not configured (422 sales.unknown_payment_term) refuses
// the read.
func (e *Endpoints) CashOrders(ctx context.Context, legalEntity uuid.UUID) ([]CashOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	orders, err := readCashOrders(ctx, tx, legalEntity)
	if err != nil || len(orders) == 0 {
		return nil, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, legalEntity) // a stored order's legal entity exists
	if err != nil {
		return nil, fmt.Errorf("sales: company of the open orders of legal entity %s: %w", legalEntity, err)
	}
	v, err := e.valuation(ctx, le)
	if err != nil {
		return nil, err
	}
	if err := e.cashLines(ctx, orders); err != nil {
		return nil, err
	}
	netDays := map[string]int{}
	out := make([]CashOrder, len(orders))
	for i, o := range orders {
		if o.Gross, err = v.cashGross(ctx, o.open); err != nil {
			return nil, err
		}
		if o.NetDays, err = e.netDays(ctx, netDays, o); err != nil {
			return nil, err
		}
		out[i] = o.CashOrder
	}
	return out, nil
}

// readCashOrders reads the company's orders with a part not yet billed.
func readCashOrders(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID) ([]cashOrder, error) {
	rows, _ := tx.Query(ctx, cashOrders, legalEntity)
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cashOrder, error) {
		var o cashOrder
		err := r.Scan(&o.ID, &o.Number, &o.Customer, &o.OrderDate, &o.DeliveryDate, &o.Currency, &o.PaymentTermCode, &o.open.Lines)
		o.open.Number, o.open.Date, o.open.Currency = o.Number, o.OrderDate, o.Currency
		return o, errs.Wrapf(err, "scan a sales order's open part")
	})
	if err != nil {
		return nil, fmt.Errorf("sales: read the open orders of legal entity %s: %w", legalEntity, err)
	}
	return orders, nil
}

// cashLines gives each order its open lines by their accounts' numbers,
// the accounts read once for every order.
func (e *Endpoints) cashLines(ctx context.Context, orders []cashOrder) error {
	var ids []uuid.UUID
	for _, o := range orders {
		for _, l := range o.open.Lines {
			ids = append(ids, l.AccountID)
		}
	}
	accounts, err := e.References.AccountNames(ctx, ids)
	if err != nil {
		return fmt.Errorf("sales: accounts of the open orders: %w", err)
	}
	for i := range orders {
		o := &orders[i]
		o.Lines = make([]CashOrderLine, len(o.open.Lines))
		for j, l := range o.open.Lines {
			o.Lines[j] = CashOrderLine{Account: accounts[l.AccountID].Number, Net: -l.Net}
			if l.TaxCode != nil {
				o.Lines[j].TaxCode = *l.TaxCode
			}
		}
	}
	return nil
}

// netDays is the order's payment term's net days, each term read once; a
// term no longer configured refuses the read (422
// sales.unknown_payment_term naming the order): its due date is not
// guessed.
func (e *Endpoints) netDays(ctx context.Context, read map[string]int, o cashOrder) (int, error) {
	if days, ok := read[o.PaymentTermCode]; ok {
		return days, nil
	}
	term, ok, err := e.Terms.PaymentTerm(ctx, o.PaymentTermCode)
	if err != nil {
		return 0, fmt.Errorf("sales: payment term %s of sales order %s: %w", o.PaymentTermCode, o.Number, err)
	}
	if !ok {
		return 0, problem.Unprocessable("sales.unknown_payment_term", "Payment term "+o.PaymentTermCode+" of sales order "+o.Number+
			" is not configured, so when it is due cannot be told.")
	}
	read[o.PaymentTermCode] = term.NetDays
	return term.NetDays, nil
}

// cashGross is the order's gross value for the forecast: gross's, its
// lines whose code is no longer an output code of the company on the
// order's date taken without tax.
func (v *valuation) cashGross(ctx context.Context, o openOrder) (money.Minor, error) {
	o.Lines = slices.Clone(o.Lines)
	for i, l := range o.Lines {
		if l.TaxCode == nil {
			continue
		}
		_, err := v.code(ctx, o, l)
		if p := problem.From(err); err != nil && p.Code == "sales.unknown_tax_code" {
			o.Lines[i].TaxCode = nil
		} else if err != nil {
			return 0, err
		}
	}
	gross, err := v.gross(ctx, o)
	if errors.Is(err, money.ErrOverflow) {
		return 0, problem.Unprocessable("sales.amount_too_large", "The value of sales order "+o.Number+" with its tax is too large.")
	}
	return gross, err
}
