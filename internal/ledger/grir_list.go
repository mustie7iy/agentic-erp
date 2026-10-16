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
	Unit                   string
	TC, LC, GC             money.Minor
	CurrencyTC, CurrencyLC string
	CurrencyGC             string
	ExpTC, ExpLC, ExpGC    int
	LastPostingDate        time.Time
}

// grirBalanceSelect sums each order item's lines on the legal entity's
// GR/IR account in the leading ledger up to a key date. Receipts are the
// procurement context's lines (their quantity credited, so negated here),
// invoices the payables context's; a reversal is its document's context's,
// and a reversal's adjustment line moves no quantity. Only those two
// contexts post order items today; another's would count in neither.
// $1, $2: company access; $3 legal entity; $4 order or NULL; $5 key date or
// NULL; $6 open positions only.
const grirBalanceSelect = `SELECT * FROM (SELECT md5(l.purchase_order_id::text || '/' || l.po_item_no)::uuid AS id, l.legal_entity_id, l.purchase_order_id,
		l.po_item_no, l.account_id,
		trim_scale(coalesce(-sum(l.quantity) FILTER (WHERE e.source_context = 'procurement'), 0))::text AS received,
		trim_scale(coalesce(sum(l.quantity) FILTER (WHERE e.source_context = 'payables'), 0))::text AS invoiced,
		min(l.uom_code) AS unit, sum(l.amount_tc)::bigint AS tc, sum(l.amount_lc)::bigint AS lc, sum(l.amount_gc)::bigint AS gc,
		min(e.currency_tc) AS currency_tc, min(e.currency_lc) AS currency_lc, min(e.currency_gc) AS currency_gc,
		min(ctc.exponent) AS exp_tc, min(clc.exponent) AS exp_lc, min(cgc.exponent) AS exp_gc, max(l.posting_date) AS last_posting_date,
		sum(l.quantity) AS net
	FROM ledger.journal_line l
	JOIN ledger.account_determination d ON d.tenant_id = l.tenant_id AND d.legal_entity_id = l.legal_entity_id AND d.key = 'gr_ir' AND d.account_id = l.account_id
	JOIN ledger.ledger lg ON lg.tenant_id = l.tenant_id AND lg.id = l.ledger_id AND lg.is_leading
	JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
	JOIN ref.currency ctc ON ctc.code = e.currency_tc
	JOIN ref.currency clc ON clc.code = e.currency_lc
	JOIN ref.currency cgc ON cgc.code = e.currency_gc
	WHERE ($1 OR l.legal_entity_id = ANY($2)) AND l.legal_entity_id = $3 AND l.purchase_order_id IS NOT NULL
		AND ($4::uuid IS NULL OR l.purchase_order_id = $4) AND ($5::date IS NULL OR l.posting_date <= $5)
	GROUP BY l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id) b
	WHERE (NOT $6 OR net <> 0 OR tc <> 0 OR lc <> 0 OR gc <> 0)`

var grirBalanceSort = pagination.Sort{Default: "purchase_order_id", Fields: []pagination.Field{
	{Name: "purchase_order_id", Column: "purchase_order_id", Type: "uuid"}, {Name: "last_posting_date", Column: "last_posting_date", Type: "date"},
}}

// ListGrIrBalances implements GET /v1/gr-ir-balances: the GR/IR positions
// of a legal entity's order items (T33). A legal entity the caller cannot
// act in has none.
func (e *Endpoints) ListGrIrBalances(ctx context.Context, params gen.ListGrIrBalancesParams) (gen.ListGrIrBalancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	order, to, openOnly := apitypes.Ptr[uuid.UUID](params.PurchaseOrderID), apitypes.Ptr[time.Time](params.PostingDateTo), params.OpenOnly.Or(true)
	scope := fmt.Sprintf("listGrIrBalances|%s|%s|%v|%t", params.LegalEntityID, order, to, openOnly)
	ks, err := e.Pages.Keyset(grirBalanceSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(grirBalanceSelect, []any{access.All, access.IDs, params.LegalEntityID, order, to, openOnly})
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
	return &gen.GrIrBalancePageHeaders{Response: gen.GrIrBalancePage{Items: items, NextPageToken: next}}, nil
}

func grirBalanceToAPI(b GRIRBalance) gen.GrIrBalance {
	return gen.GrIrBalance{LegalEntityID: b.LegalEntityID, PurchaseOrderID: b.PurchaseOrderID, PoItemNo: b.POItemNo, AccountID: b.AccountID,
		ReceivedQuantity: gen.Quantity{Value: b.Received, Unit: b.Unit}, InvoicedQuantity: gen.Quantity{Value: b.Invoiced, Unit: b.Unit},
		Balance: apitypes.Money(b.TC, b.CurrencyTC, b.ExpTC), BalanceLc: apitypes.Money(b.LC, b.CurrencyLC, b.ExpLC),
		BalanceGc: apitypes.Money(b.GC, b.CurrencyGC, b.ExpGC), LastPostingDate: b.LastPostingDate}
}

func scanGRIRBalance(row pgx.Row) (GRIRBalance, error) {
	var b GRIRBalance
	var net string // the filter's net quantity, not shown
	err := row.Scan(&b.ID, &b.LegalEntityID, &b.PurchaseOrderID, &b.POItemNo, &b.AccountID, &b.Received, &b.Invoiced, &b.Unit, &b.TC, &b.LC, &b.GC,
		&b.CurrencyTC, &b.CurrencyLC, &b.CurrencyGC, &b.ExpTC, &b.ExpLC, &b.ExpGC, &b.LastPostingDate, &net)
	return b, errs.Wrapf(err, "scan GR/IR balance")
}
