package controlling

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
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Line is a plan line as stored.
type Line struct {
	ID, BatchID, CategoryID, LegalEntityID uuid.UUID
	FiscalYear, FiscalPeriod               int16
	CostCenterID, ProfitCenterID           *uuid.UUID
	AccountID                              uuid.UUID
	Amount                                 money.Minor
	Quantity, Unit                         *string
	CreatedAt                              time.Time
	CreatedBy                              uuid.UUID
}

const lineColumns = `id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period, cost_center_id, profit_center_id, account_id, amount,
	trim_scale(quantity)::text, uom_code, created_at, created_by`

func scanLine(r pgx.CollectableRow) (Line, error) {
	var l Line
	err := r.Scan(&l.ID, &l.BatchID, &l.CategoryID, &l.LegalEntityID, &l.FiscalYear, &l.FiscalPeriod, &l.CostCenterID, &l.ProfitCenterID, &l.AccountID,
		&l.Amount, &l.Quantity, &l.Unit, &l.CreatedAt, &l.CreatedBy)
	return l, errs.Wrapf(err, "controlling: scan plan line")
}

var lineSort = pagination.Sort{Default: "fiscal_period", Fields: []pagination.Field{{Name: "fiscal_period", Column: "fiscal_period", Type: "smallint"}}}

// company is the legal entity a read is about, when the caller may act in
// it, and the minor-unit digits of its currency.
func (e *Endpoints) company(ctx context.Context, tx pgx.Tx, id uuid.UUID) (tenancy.Entity, int, bool, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, 0, false, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, id)
	if err != nil || !ok || !access.Allows(le.ID) {
		return tenancy.Entity{}, 0, false, err
	}
	exp, err := currency.Exponent(ctx, tx, le.LocalCurrency, nil)
	if err != nil {
		return tenancy.Entity{}, 0, false, err
	}
	return le, exp, true, nil
}

// ListPlanLines implements GET /v1/plan-lines.
func (e *Endpoints) ListPlanLines(ctx context.Context, params gen.ListPlanLinesParams) (gen.ListPlanLinesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	cat, cc, pc := apitypes.Ptr[uuid.UUID](params.PlanCategoryID), apitypes.Ptr[uuid.UUID](params.CostCenterID), apitypes.Ptr[uuid.UUID](params.ProfitCenterID)
	account, batch := apitypes.Ptr[uuid.UUID](params.AccountID), apitypes.Ptr[uuid.UUID](params.PlanBatchID)
	scope := fmt.Sprintf("listPlanLines|%s|%d|%v|%v|%v|%v|%v", params.LegalEntityID, params.FiscalYear, cat, cc, pc, account, batch)
	ks, err := e.Pages.Keyset(lineSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	le, exp, ok, err := e.company(ctx, tx, params.LegalEntityID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &gen.PlanLinePage{Items: []gen.PlanLine{}}, nil
	}
	q, args := ks.SQL(`SELECT `+lineColumns+` FROM controlling.plan_line WHERE legal_entity_id = $1 AND fiscal_year = $2 AND replaced_by_batch_id IS NULL
		AND ($3::uuid IS NULL OR category_id = $3) AND ($4::uuid IS NULL OR cost_center_id = $4) AND ($5::uuid IS NULL OR profit_center_id = $5)
		AND ($6::uuid IS NULL OR account_id = $6) AND ($7::uuid IS NULL OR batch_id = $7)`, []any{le.ID, params.FiscalYear, cat, cc, pc, account, batch})
	rows, _ := tx.Query(ctx, q, args...)
	ls, err := pgx.CollectRows(rows, scanLine)
	if err != nil {
		return nil, fmt.Errorf("controlling: list plan lines of %s: %w", le.ID, err)
	}
	items, next := apitypes.PageOf(ks, ls, func(l Line) (string, uuid.UUID) { return fmt.Sprint(l.FiscalPeriod), l.ID }, func(l Line) gen.PlanLine {
		out := gen.PlanLine{ID: l.ID, PlanBatchID: l.BatchID, PlanCategoryID: l.CategoryID, LegalEntityID: l.LegalEntityID, FiscalYear: int32(l.FiscalYear),
			FiscalPeriod: int32(l.FiscalPeriod), CostCenterID: apitypes.OptUUID(l.CostCenterID), ProfitCenterID: apitypes.OptUUID(l.ProfitCenterID),
			AccountID: l.AccountID, Amount: apitypes.Money(l.Amount, le.LocalCurrency, exp), CreatedAt: l.CreatedAt, CreatedBy: l.CreatedBy}
		if l.Quantity != nil {
			out.Quantity.SetTo(gen.Quantity{Value: *l.Quantity, Unit: *l.Unit})
		}
		return out
	})
	return &gen.PlanLinePage{Items: items, NextPageToken: next}, nil
}
