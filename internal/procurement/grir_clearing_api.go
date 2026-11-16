package procurement

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func clearingToAPI(c GRIRClearing) gen.GrIrClearing {
	total, _, _ := c.Total() // in range: the proposal refused it otherwise
	out := gen.GrIrClearing{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), PurchaseOrderID: c.PurchaseOrderID, PurchaseOrderNumber: c.OrderNumber, LegalEntityID: c.LegalEntityID,
		Status: c.Status, Reason: c.Reason, PostingDate: c.PostingDate,
		OffsetAccount: gen.GrIrClearingAccount{ID: c.OffsetAccount.ID, Number: c.OffsetAccount.Number, Name: c.OffsetAccount.Name},
		Items:         make([]gen.GrIrClearingItem, len(c.Items)), Total: apitypes.Money(total, c.Currency, c.Exponent),
		ApprovalRequestID: c.ApprovalRequestID, DecisionComment: apitypes.OptString(c.DecisionComment)}
	for i, it := range c.Items {
		out.Items[i] = gen.GrIrClearingItem{PoItemNo: it.POItemNo, Description: it.Description, Quantity: gen.Quantity{Value: it.Quantity, Unit: it.Unit},
			Amount:        apitypes.Money(it.Amount, c.Currency, c.Exponent),
			OffsetAccount: gen.GrIrClearingAccount{ID: it.OffsetAccount.ID, Number: it.OffsetAccount.Number, Name: it.OffsetAccount.Name}}
	}
	if c.JournalEntryID != nil {
		out.JournalEntry.SetTo(gen.GrIrClearingEntry{ID: *c.JournalEntryID, FiscalYear: int32(*c.JournalEntryYear), DocumentNumber: *c.JournalEntryNumber})
	}
	return out
}

// CreateGrIrClearing implements POST /v1/purchase-orders/{id}/gr-ir-clearings.
func (e *Endpoints) CreateGrIrClearing(ctx context.Context, req *gen.GrIrClearingCreate, params gen.CreateGrIrClearingParams) (gen.CreateGrIrClearingRes, error) {
	c, err := e.createGRIRClearing(ctx, params.ID, GRIRClearingInput{Reason: req.Reason, PostingDate: req.PostingDate, Items: req.PoItemNos})
	if err != nil {
		return nil, err
	}
	return &gen.CreateGrIrClearingCreatedHeaders{ETag: gen.NewOptString(etag.Format(c.Version)),
		Location: gen.NewOptString("/v1/gr-ir-clearings/" + c.ID.String()), Response: clearingToAPI(c)}, nil
}

// GetGrIrClearing implements GET /v1/gr-ir-clearings/{id}.
func (e *Endpoints) GetGrIrClearing(ctx context.Context, params gen.GetGrIrClearingParams) (gen.GetGrIrClearingRes, error) {
	c, err := GetGRIRClearing(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GrIrClearingHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: clearingToAPI(c)}, nil
}

// clearingList is clearingSelect as a table, so the keyset's condition on
// its unqualified id names the clearing's.
const clearingList = `SELECT * FROM (` + clearingSelect + `) c WHERE true`

var clearingSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"},
}}

// ListGrIrClearings implements GET /v1/purchase-orders/{id}/gr-ir-clearings.
func (e *Endpoints) ListGrIrClearings(ctx context.Context, params gen.ListGrIrClearingsParams) (gen.ListGrIrClearingsRes, error) {
	page := &gen.GrIrClearingPage{}
	var err error
	if page.Items, page.NextPageToken, err = listOfOrder(ctx, e, params.ID, clearingListOf(params)); err != nil {
		return nil, err
	}
	return page, nil
}

func clearingListOf(params gen.ListGrIrClearingsParams) orderList[GRIRClearing, gen.GrIrClearing] {
	return orderList[GRIRClearing, gen.GrIrClearing]{sort: clearingSort, scope: "listGrIrClearings",
		page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken), query: clearingList + ` AND purchase_order_id = $1`,
		what: "GR/IR clearings", scan: scanClearing, key: func(c GRIRClearing) (string, uuid.UUID) { return timestamp(c.CreatedAt), c.ID }, toAPI: clearingToAPI}
}

// orderList is a keyset-paged list of an order's documents: their query,
// whose $1 is the order, and how a row is read, keyed and shown.
type orderList[R, A any] struct {
	sort  pagination.Sort
	scope string
	page  pagination.Request
	query string
	what  string
	scan  func(pgx.Row) (R, error)
	key   func(R) (string, uuid.UUID)
	toAPI func(R) A
}

// listOfOrder pages the documents of an order the caller can see (404
// otherwise).
func listOfOrder[R, A any](ctx context.Context, e *Endpoints, order uuid.UUID, l orderList[R, A]) ([]A, gen.OptString, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, gen.OptString{}, err
	}
	if _, err := getOrder(ctx, tx, order, false); err != nil {
		return nil, gen.OptString{}, err
	}
	ks, err := e.Pages.Keyset(l.sort, l.scope+"|"+order.String(), l.page)
	if err != nil {
		return nil, gen.OptString{}, err
	}
	q, args := ks.SQL(l.query, []any{order})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (R, error) { return l.scan(r) })
	if err != nil {
		return nil, gen.OptString{}, fmt.Errorf("procurement: list %s of purchase order %s: %w", l.what, order, err)
	}
	items, next := apitypes.PageOf(ks, list, l.key, l.toAPI)
	return items, next, nil
}
