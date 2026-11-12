package controlling

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of an internal order (ADR-0064 §1, §3): created, released
// for posting, and closed once it is settled, which ends its postings.
const (
	OrderCreated  = "created"
	OrderReleased = "released"
	OrderClosed   = "closed"
)

// Order is an internal order (ADR-0064 §1). CostCenterCode is the
// responsible cost center's, named by the ledger's port when the order is
// shown.
type Order struct {
	ID, LegalEntityID uuid.UUID
	Code, Name        string
	CostCenterID      uuid.UUID
	CostCenterCode    string
	Status            string
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const orderColumns = `id, legal_entity_id, code, name, responsible_cost_center_id, status, version, created_at, created_by, updated_at, updated_by`

func scanOrder(r pgx.Row) (Order, error) {
	var o Order
	err := r.Scan(&o.ID, &o.LegalEntityID, &o.Code, &o.Name, &o.CostCenterID, &o.Status, &o.Version, &o.CreatedAt, &o.CreatedBy, &o.UpdatedAt, &o.UpdatedBy)
	return o, errs.Wrapf(err, "controlling: scan internal order")
}

// getOrder reads an order the caller may see, FOR UPDATE when it is to
// change, or 404: an order of a company outside the caller's access is
// answered as missing (04 §4.9).
func getOrder(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Order, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Order{}, err
	}
	lock := ``
	if forUpdate {
		lock = ` FOR UPDATE`
	}
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM controlling.internal_order WHERE id = $1`+lock, id))
	if err := authz.Visible(err, access, o.LegalEntityID, problem.NotFoundOf("internal order"), "controlling: read internal order %s", id); err != nil {
		return Order{}, err
	}
	return o, nil
}

// named gives the orders their responsible cost centers' codes, read from
// the ledger in one call.
func (e *Endpoints) named(ctx context.Context, orders ...Order) ([]Order, error) {
	ids := make([]uuid.UUID, len(orders))
	for i, o := range orders {
		ids[i] = o.CostCenterID
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("controlling: cost centers of internal orders: %w", err)
	}
	for i := range orders {
		orders[i].CostCenterCode = refs[orders[i].CostCenterID].Code
	}
	return orders, nil
}

// recordOrder audits an order's change and publishes it.
func recordOrder(ctx context.Context, o Order, action, change string) error {
	detail := map[string]any{"legal_entity_id": o.LegalEntityID, "code": o.Code, "name": o.Name, "responsible_cost_center_id": o.CostCenterID,
		"responsible_cost_center_code": o.CostCenterCode, "status": o.Status}
	return audit.Record(ctx, "controlling.internal_order."+action, "controlling.internal_order", o.ID, detail,
		outbox.Event{Context: "controlling", Name: "internal_order_changed", AggregateType: "controlling.internal_order", AggregateID: o.ID,
			Payload: map[string]any{"internal_order_id": o.ID, "legal_entity_id": o.LegalEntityID, "code": o.Code, "name": o.Name,
				"responsible_cost_center_id": o.CostCenterID, "responsible_cost_center_code": o.CostCenterCode, "status": o.Status, "change": change}})
}

func orderToAPI(o Order) gen.InternalOrder {
	return gen.InternalOrder{ID: o.ID, Version: o.Version, CreatedAt: o.CreatedAt, CreatedBy: o.CreatedBy, UpdatedAt: o.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(o.UpdatedBy), LegalEntityID: o.LegalEntityID, Code: o.Code, Name: o.Name, ResponsibleCostCenterID: o.CostCenterID,
		ResponsibleCostCenterCode: o.CostCenterCode, Status: o.Status}
}

// responsible checks the cost center an order names: an active cost
// center of the order's company (422 naming why otherwise), and returns
// its code.
func (e *Endpoints) responsible(ctx context.Context, le, costCenter uuid.UUID) (string, error) {
	refs, err := e.Ledger.CostCenterRefs(ctx, []uuid.UUID{costCenter})
	if err != nil {
		return "", fmt.Errorf("controlling: responsible cost center %s: %w", costCenter, err)
	}
	ref, known := refs[costCenter]
	reason, message := "", ""
	switch {
	case !known:
		reason, message = "unknown", "is not known"
	case ref.LegalEntityID != le:
		reason, message = "not_in_company", "is not one of the legal entity's"
	case !ref.Active:
		reason, message = "not_active", "is not active"
	default:
		return ref.Code, nil
	}
	return "", problem.Unprocessable("controlling.internal_order_cost_center_invalid", "The responsible cost center "+message+".",
		problem.FieldError{Field: "responsible_cost_center_id", Code: reason, Message: "an active cost center of the legal entity"})
}

// CreateInternalOrder implements POST /v1/internal-orders.
func (e *Endpoints) CreateInternalOrder(ctx context.Context, req *gen.InternalOrderCreate, _ gen.CreateInternalOrderParams) (gen.CreateInternalOrderRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if err := problem.RequireText("name", req.Name); err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, req.LegalEntityID, "legal_entity_id")
	if err != nil {
		return nil, err
	}
	code, err := e.responsible(ctx, le.ID, req.ResponsibleCostCenterID)
	if err != nil {
		return nil, err
	}
	o, err := scanOrder(tx.QueryRow(ctx, `INSERT INTO controlling.internal_order (tenant_id, id, legal_entity_id, code, name, responsible_cost_center_id,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING `+orderColumns, p.TenantID, uuid.Must(uuid.NewV7()), le.ID, req.Code, req.Name,
		req.ResponsibleCostCenterID, p.ID))
	if err != nil {
		return nil, fmt.Errorf("controlling: insert internal order %s of %s: %w", req.Code, le.ID, err)
	}
	o.CostCenterCode = code
	if err := recordOrder(ctx, o, "create", "created"); err != nil {
		return nil, err
	}
	return &gen.InternalOrderHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Location: gen.NewOptString("/v1/internal-orders/" + o.ID.String()),
		Response: orderToAPI(o)}, nil
}

// GetInternalOrder implements GET /v1/internal-orders/{id}.
func (e *Endpoints) GetInternalOrder(ctx context.Context, params gen.GetInternalOrderParams) (gen.GetInternalOrderRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := getOrder(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	os, err := e.named(ctx, o)
	if err != nil {
		return nil, err
	}
	return &gen.GetInternalOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: orderToAPI(os[0])}, nil
}

var orderSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

// ListInternalOrders implements GET /v1/internal-orders.
func (e *Endpoints) ListInternalOrders(ctx context.Context, params gen.ListInternalOrdersParams) (gen.ListInternalOrdersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, cc := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.ResponsibleCostCenterID)
	scope := fmt.Sprintf("listInternalOrders|%v|%s|%s|%v", le, params.Code.Value, params.Status.Value, cc)
	ks, err := e.Pages.Keyset(orderSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+orderColumns+` FROM controlling.internal_order WHERE ($1 OR legal_entity_id = ANY($2))
		AND ($3::uuid IS NULL OR legal_entity_id = $3) AND ($4 = '' OR code = $4) AND ($5 = '' OR status = $5)
		AND ($6::uuid IS NULL OR responsible_cost_center_id = $6)`, []any{access.All, access.IDs, le, params.Code.Value, params.Status.Value, cc})
	rows, _ := tx.Query(ctx, q, args...)
	os, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list internal orders: %w", err)
	}
	if os, err = e.named(ctx, os...); err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, os, func(o Order) (string, uuid.UUID) { return o.Code, o.ID }, orderToAPI)
	return &gen.InternalOrderPage{Items: items, NextPageToken: next}, nil
}

// ReleaseInternalOrder implements POST /v1/internal-orders/{id}:release:
// the order is read FOR UPDATE at the version the caller read and moves
// from created to released once; a second release, the first's replay
// aside (Idempotency-Key), finds it released and is refused.
func (e *Endpoints) ReleaseInternalOrder(ctx context.Context, params gen.ReleaseInternalOrderParams) (gen.ReleaseInternalOrderRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, err := getOrder(ctx, tx, params.ID, true)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(o.Version); err != nil {
		return nil, err
	}
	if o.Status != OrderCreated {
		return nil, problem.Conflict("controlling.internal_order_released", "The internal order is already "+o.Status+".")
	}
	o, err = scanOrder(tx.QueryRow(ctx, `UPDATE controlling.internal_order SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 AND version = $4 RETURNING `+orderColumns, o.ID, OrderReleased, p.ID, o.Version))
	if err != nil {
		return nil, fmt.Errorf("controlling: release internal order %s: %w", params.ID, err)
	}
	os, err := e.named(ctx, o)
	if err != nil {
		return nil, err
	}
	if err := recordOrder(ctx, os[0], "release", "released"); err != nil {
		return nil, err
	}
	// The generated answer is one type for release and close, named for
	// the first of them.
	return &gen.CloseInternalOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: orderToAPI(os[0])}, nil
}
