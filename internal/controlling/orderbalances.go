package controlling

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Costs posted to internal orders (ADR-0064 §2, T60b). The journal line
// names its order without a foreign key; the ledger asks this context
// about the orders through its port ledger.CostObjects, which CostObjects
// implements, and reads an order's actuals from its own lines
// (ledger.AssignmentBalances.OrderActuals), which the order balances show.

// CostObjects is the controlling context's side of ledger.CostObjects: the
// internal orders the lines of a posting name, read in the request's
// transaction under row-level security.
type CostObjects struct{}

// Orders reads the orders with the ids, keyed by id; an id that is no
// order has no entry. hold keeps them FOR SHARE until the transaction
// ends, in id order: a posting holds the orders it posts to, so a status
// change, which takes the order FOR UPDATE, waits for postings under way
// and a posting after it sees it (ADR-0008).
func (CostObjects) Orders(ctx context.Context, ids []uuid.UUID, hold bool) (map[uuid.UUID]ledger.CostOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	lock := ``
	if hold {
		lock = ` ORDER BY id FOR SHARE`
	}
	rows, _ := tx.Query(ctx, `SELECT id, legal_entity_id, code, status = $2, responsible_cost_center_id FROM controlling.internal_order
		WHERE id = ANY($1)`+lock, ids, OrderReleased)
	out := map[uuid.UUID]ledger.CostOrder{}
	var id uuid.UUID
	var o ledger.CostOrder
	_, err = pgx.ForEachRow(rows, []any{&id, &o.LegalEntityID, &o.Code, &o.Postable, &o.CostCenterID}, func() error { out[id] = o; return nil })
	return out, errs.Wrapf(err, "controlling: internal orders named by journal lines")
}

// obCursor is the key of the last row of a page of order balances.
type obCursor struct {
	Number  string    `json:"n"`
	Account uuid.UUID `json:"a"`
	Period  int16     `json:"p"`
}

// obQuery is a page's request, resolved: the order, its company's
// currency, the ledger's query and the page.
type obQuery struct {
	order    Order
	currency string
	exp      int
	actuals  ledger.OrderActualsQuery
	scope    string
	size     int
}

func orderMissing() error {
	return problem.ReferenceNotFound("internal_order_id", "internal order", "no internal order with this id that you can access")
}

// ListInternalOrderBalances implements GET /v1/internal-order-balances.
func (e *Endpoints) ListInternalOrderBalances(ctx context.Context, params gen.ListInternalOrderBalancesParams) (gen.ListInternalOrderBalancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	q, err := e.orderBalancesQuery(ctx, tx, params)
	if err != nil {
		return nil, err
	}
	actuals, err := e.Ledger.OrderActuals(ctx, q.actuals)
	if err != nil {
		return nil, fmt.Errorf("controlling: actuals of internal order %s: %w", q.order.ID, err)
	}
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, q.currency, q.exp) }
	page := &gen.InternalOrderBalancePage{InternalOrderID: q.order.ID, InternalOrderCode: q.order.Code, InternalOrderName: q.order.Name,
		LegalEntityID: q.order.LegalEntityID, FiscalYear: params.FiscalYear, FiscalPeriodFrom: int32(q.actuals.PeriodFrom),
		FiscalPeriodTo: int32(q.actuals.PeriodTo),
		Totals:         gen.InternalOrderBalanceTotals{Debit: amount(actuals.Debit), Credit: amount(actuals.Credit), Balance: amount(actuals.Balance)}}
	rows := actuals.Rows
	if len(rows) > q.size {
		rows = rows[:q.size]
		last := rows[q.size-1]
		tok, _ := e.Pages.Encode(q.scope, obCursor{Number: last.AccountNumber, Account: last.AccountID, Period: last.Period}) // a string, a uuid and a number always encode
		page.NextPageToken.SetTo(tok)
	}
	page.Items = make([]gen.InternalOrderBalance, len(rows))
	for i, r := range rows {
		page.Items[i] = gen.InternalOrderBalance{AccountID: r.AccountID, AccountNumber: r.AccountNumber, AccountName: r.AccountName,
			FiscalPeriod: int32(r.Period), Debit: amount(r.Debit), Credit: amount(r.Credit), Balance: amount(r.Balance)}
	}
	return page, nil
}

// orderBalancesQuery resolves the request: the page, the periods, and the
// order, which is answered as missing when it does not exist or its
// company is one the caller may not act in (04 §4.9).
func (e *Endpoints) orderBalancesQuery(ctx context.Context, tx pgx.Tx, params gen.ListInternalOrderBalancesParams) (obQuery, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return obQuery{}, err
	}
	from, to, err := periodRange(params.FiscalPeriodFrom, params.FiscalPeriodTo)
	if err != nil {
		return obQuery{}, err
	}
	q := obQuery{size: size, scope: fmt.Sprintf("listInternalOrderBalances|%s|%d|%d|%d", params.InternalOrderID, params.FiscalYear, from, to),
		actuals: ledger.OrderActualsQuery{OrderID: params.InternalOrderID, FiscalYear: int16(params.FiscalYear), PeriodFrom: from, PeriodTo: to, Limit: size + 1}}
	if params.PageToken.Set {
		var c obCursor
		if err := e.Pages.Decode(q.scope, params.PageToken.Value, &c); err != nil {
			return obQuery{}, err
		}
		q.actuals.After = &ledger.OrderActualKey{AccountNumber: c.Number, AccountID: c.Account, Period: c.Period}
	}
	q.order, err = scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM controlling.internal_order WHERE id = $1`, params.InternalOrderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return obQuery{}, orderMissing()
	}
	if err != nil {
		return obQuery{}, fmt.Errorf("controlling: read internal order %s: %w", params.InternalOrderID, err)
	}
	le, exp, ok, err := e.company(ctx, tx, q.order.LegalEntityID)
	if err != nil {
		return obQuery{}, err
	}
	if !ok {
		return obQuery{}, orderMissing()
	}
	q.currency, q.exp, q.actuals.LegalEntityID = le.LocalCurrency, exp, le.ID
	return q, nil
}
