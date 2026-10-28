package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// GRIRBalance is an order item's GR/IR position as the listing shows it.
type GRIRBalance struct {
	ID                     uuid.UUID // derived from the order item, the keyset's tie-breaker
	LegalEntityID          uuid.UUID
	PurchaseOrderID        uuid.UUID
	POItemNo               int32
	AccountID              uuid.UUID
	Received, Invoiced     string
	Cleared                string // what GR/IR clearings cleared (T72e), signed as the position
	Unit                   string
	TC, LC, GC             money.Minor
	CurrencyTC, CurrencyLC string
	CurrencyGC             string
	ExpTC, ExpLC, ExpGC    int
	LastPostingDate        time.Time
}

// grirPositionSelect reads the projected positions (ledger.gr_ir_position,
// T33f) of a legal entity: a keyset page is an index range scan on
// (legal entity, sort column, id), over the open positions' partial index
// by default. $1, $2: company access; $3 legal entity.
const grirPositionSelect = `SELECT * FROM (SELECT p.id, p.legal_entity_id, p.purchase_order_id, p.po_item_no, p.account_id,
		trim_scale(p.received)::text AS received, trim_scale(p.invoiced)::text AS invoiced, trim_scale(p.cleared)::text AS cleared, p.uom_code, p.amount_tc, p.amount_lc, p.amount_gc,
		p.currency_tc, p.currency_lc, p.currency_gc, ctc.exponent AS exp_tc, clc.exponent AS exp_lc, cgc.exponent AS exp_gc, p.last_posting_date, p.is_open
	FROM ledger.gr_ir_position p
	JOIN ref.currency ctc ON ctc.code = p.currency_tc
	JOIN ref.currency clc ON clc.code = p.currency_lc
	JOIN ref.currency cgc ON cgc.code = p.currency_gc
	WHERE ($1 OR p.legal_entity_id = ANY($2)) AND p.legal_entity_id = $3) b WHERE true`

// grirKeyDateSelect sums one order's lines on the legal entity's GR/IR
// account up to a key date: a report over the order's history, bounded by
// the order (journal_line_order_item), which the projection, current only,
// cannot answer. Receipts are the procurement context's lines (credited,
// so negated here), invoices the payables context's, clearings those of
// entries recorded as GR/IR clearings (T72e); a reversal is its document's
// context's. $1, $2: company access; $3 legal entity; $4 order;
// $5 key date.
const grirKeyDateSelect = `SELECT * FROM (SELECT md5(l.purchase_order_id::text || '/' || l.po_item_no::text)::uuid AS id, l.legal_entity_id, l.purchase_order_id,
		l.po_item_no, l.account_id,
		trim_scale(coalesce(-sum(l.quantity) FILTER (WHERE e.source_context = 'procurement' AND c.journal_entry_id IS NULL), 0))::text AS received,
		trim_scale(coalesce(sum(l.quantity) FILTER (WHERE e.source_context = 'payables'), 0))::text AS invoiced,
		trim_scale(coalesce(sum(l.quantity) FILTER (WHERE c.journal_entry_id IS NOT NULL), 0))::text AS cleared,
		min(l.uom_code) AS unit, sum(l.amount_tc)::bigint AS tc, sum(l.amount_lc)::bigint AS lc, sum(l.amount_gc)::bigint AS gc,
		min(e.currency_tc) AS currency_tc, min(e.currency_lc) AS currency_lc, min(e.currency_gc) AS currency_gc,
		min(ctc.exponent) AS exp_tc, min(clc.exponent) AS exp_lc, min(cgc.exponent) AS exp_gc, max(l.posting_date) AS last_posting_date,
		(sum(l.quantity) <> 0 OR sum(l.amount_tc) <> 0 OR sum(l.amount_lc) <> 0 OR sum(l.amount_gc) <> 0) AS is_open
	FROM ledger.journal_line l
	JOIN ledger.account_determination d ON d.tenant_id = l.tenant_id AND d.legal_entity_id = l.legal_entity_id AND d.key = 'gr_ir' AND d.account_id = l.account_id
	JOIN ledger.ledger lg ON lg.tenant_id = l.tenant_id AND lg.id = l.ledger_id AND lg.is_leading
	JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
	LEFT JOIN ledger.gr_ir_clearing_entry c ON c.tenant_id = e.tenant_id AND c.fiscal_year = e.fiscal_year AND c.journal_entry_id = e.id
	JOIN ref.currency ctc ON ctc.code = e.currency_tc
	JOIN ref.currency clc ON clc.code = e.currency_lc
	JOIN ref.currency cgc ON cgc.code = e.currency_gc
	WHERE ($1 OR l.legal_entity_id = ANY($2)) AND l.legal_entity_id = $3 AND l.purchase_order_id = $4 AND l.posting_date <= $5
	GROUP BY l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id) b WHERE true`

var grirBalanceSort = pagination.Sort{Default: "purchase_order_id", Fields: []pagination.Field{
	{Name: "purchase_order_id", Column: "purchase_order_id", Type: "uuid"}, {Name: "last_posting_date", Column: "last_posting_date", Type: "date"},
}}

// ListGrIrBalances implements GET /v1/gr-ir-balances: the GR/IR positions
// of a legal entity's order items (T33), current ones from their
// projection, or one order's on a key date from its lines. A legal entity
// the caller cannot act in has none.
func (e *Endpoints) ListGrIrBalances(ctx context.Context, params gen.ListGrIrBalancesParams) (gen.ListGrIrBalancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	order, to, openOnly := apitypes.Ptr[uuid.UUID](params.PurchaseOrderID), apitypes.Ptr[time.Time](params.PostingDateTo), params.OpenOnly.Or(true)
	if to != nil && order == nil {
		return nil, problem.BadRequest("request.invalid_parameter", "A key date (posting_date_to) reports one purchase order's positions from its history; name the order.",
			problem.FieldError{Field: "purchase_order_id", Code: "required", Message: "the order whose positions on the key date are reported"})
	}
	scope := fmt.Sprintf("listGrIrBalances|%s|%s|%v|%t", params.LegalEntityID, order, to, openOnly)
	ks, err := e.Pages.Keyset(grirBalanceSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	query, args := grirPositionSelect, []any{access.All, access.IDs, params.LegalEntityID}
	switch {
	case to != nil:
		query, args = grirKeyDateSelect, append(args, *order, *to)
	case order != nil:
		// Only when given, so a cached generic plan keeps it an index condition.
		query, args = query+` AND purchase_order_id = $4`, append(args, *order)
	}
	if openOnly {
		// A literal condition, so the planner uses the open positions' partial index.
		query += ` AND is_open`
	}
	q, args := ks.SQL(query, args)
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (GRIRBalance, error) { return scanGRIRBalance(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list GR/IR balances of %s: %w", params.LegalEntityID, err)
	}
	items, next := apitypes.PageOf(ks, list, func(b GRIRBalance) (string, uuid.UUID) {
		if ks.Field() == "last_posting_date" {
			return b.LastPostingDate.Format(time.DateOnly), b.ID
		}
		return b.PurchaseOrderID.String(), b.ID
	}, grirBalanceToAPI)
	return &gen.GrIrBalancePage{Items: items, NextPageToken: next}, nil
}

func grirBalanceToAPI(b GRIRBalance) gen.GrIrBalance {
	return gen.GrIrBalance{LegalEntityID: b.LegalEntityID, PurchaseOrderID: b.PurchaseOrderID, PoItemNo: b.POItemNo, AccountID: b.AccountID,
		ReceivedQuantity: gen.Quantity{Value: b.Received, Unit: b.Unit}, InvoicedQuantity: gen.Quantity{Value: b.Invoiced, Unit: b.Unit},
		ClearedQuantity: gen.Quantity{Value: b.Cleared, Unit: b.Unit},
		Balance:         apitypes.Money(b.TC, b.CurrencyTC, b.ExpTC), BalanceLc: apitypes.Money(b.LC, b.CurrencyLC, b.ExpLC),
		BalanceGc: apitypes.Money(b.GC, b.CurrencyGC, b.ExpGC), LastPostingDate: b.LastPostingDate}
}

func scanGRIRBalance(row pgx.Row) (GRIRBalance, error) {
	var b GRIRBalance
	var open bool // the filter's, not shown
	err := row.Scan(&b.ID, &b.LegalEntityID, &b.PurchaseOrderID, &b.POItemNo, &b.AccountID, &b.Received, &b.Invoiced, &b.Cleared, &b.Unit, &b.TC, &b.LC, &b.GC,
		&b.CurrencyTC, &b.CurrencyLC, &b.CurrencyGC, &b.ExpTC, &b.ExpLC, &b.ExpGC, &b.LastPostingDate, &open)
	return b, errs.Wrapf(err, "scan GR/IR balance")
}
