package mrp

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// sopCompanies reads the codes of the versions' companies.
func (e *Endpoints) sopCompanies(ctx context.Context, ps []SopPlan) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, p := range ps { // a page names few companies: one read each
		if _, ok := out[p.LegalEntityID]; ok {
			continue
		}
		// A version's company is one the caller may act in, so it is there.
		le, _, err := e.Plants.Entity(ctx, p.LegalEntityID)
		if err != nil {
			return nil, fmt.Errorf("mrp: company of S&OP plan %d: %w", p.Number, err)
		}
		out[p.LegalEntityID] = le.Code
	}
	return out, nil
}

// sopAPI is a version as the API gives it: each line's opening stock the
// plants' stock in a group's first month and the month before's projected
// stock after it, its projected stock the opening stock and receipts less
// the consensus.
func sopAPI(p SopPlan, companies map[uuid.UUID]string) gen.SopPlan {
	out := gen.SopPlan{ID: p.ID, Version: p.Version, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(p.UpdatedBy), LegalEntityID: p.LegalEntityID, CompanyCode: companies[p.LegalEntityID], Number: p.Number,
		Status: p.Status, AsOf: p.AsOf, FirstDay: p.FirstDay, LastDay: p.LastDay, Months: int32(p.Months), FixedAt: apitypes.OptDateTime(p.FixedAt),
		FixedBy: apitypes.OptUUID(p.FixedBy), ApprovalRequestID: apitypes.OptUUID(p.ApprovalRequestID), Lines: make([]gen.SopPlanLine, len(p.Lines))}
	var opening *big.Rat
	for i, l := range p.Lines { // by group, unit and month: a group's months follow one another
		if i == 0 || p.Lines[i-1].Group != l.Group || p.Lines[i-1].Unit != l.Unit {
			opening = money.ParseRat(l.Stock)
		}
		projected := new(big.Rat).Add(opening, money.ParseRat(l.PurchaseOrders))
		projected.Add(projected, money.ParseRat(l.StockTransports)).Add(projected, money.ParseRat(l.Proposals)).Sub(projected, money.ParseRat(l.Consensus))
		q := func(v string) gen.Quantity { return gen.Quantity{Value: v, Unit: l.Unit} }
		out.Lines[i] = gen.SopPlanLine{Key: l.Key(), MaterialGroup: l.Group, Unit: l.Unit, Month: l.Month.Format("2006-01"), FirstDay: l.FirstDay,
			LastDay: l.LastDay, Materials: l.Materials, Forecast: q(l.Forecast), Orders: q(l.Orders),
			OpeningStock: q(money.PlainQuantity(opening)), PurchaseOrders: q(l.PurchaseOrders), StockTransports: q(l.StockTransports),
			Proposals: q(l.Proposals), Consensus: q(l.Consensus), ProjectedStock: q(money.PlainQuantity(projected))}
		opening = projected
	}
	return out
}

// oneSop is a version as the API gives it, with its ETag.
func (e *Endpoints) oneSop(ctx context.Context, p SopPlan, err error) (gen.SopPlan, gen.OptString, error) {
	if err != nil {
		return gen.SopPlan{}, gen.OptString{}, err
	}
	companies, err := e.sopCompanies(ctx, []SopPlan{p})
	if err != nil {
		return gen.SopPlan{}, gen.OptString{}, err
	}
	return sopAPI(p, companies), gen.NewOptString(etag.Format(p.Version)), nil
}

// CreateSopPlan implements POST /v1/sop-plans.
func (e *Endpoints) CreateSopPlan(ctx context.Context, req *gen.SopPlanCreate, _ gen.CreateSopPlanParams) (gen.CreateSopPlanRes, error) {
	p, err := e.StartSop(ctx, SopInput{LegalEntityID: req.LegalEntityID, Months: int(req.Months.Or(DefaultMonths))})
	out, tag, err := e.oneSop(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.SopPlanHeaders{ETag: tag, Location: gen.NewOptString(sopPath(p.ID)), Response: out}, nil
}

// GetSopPlan implements GET /v1/sop-plans/{id}.
func (e *Endpoints) GetSopPlan(ctx context.Context, params gen.GetSopPlanParams) (gen.GetSopPlanRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	p, err := loadSop(ctx, tx, params.ID, false)
	out, tag, err := e.oneSop(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetSopPlanOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateSopPlan implements PATCH /v1/sop-plans/{id}.
func (e *Endpoints) UpdateSopPlan(ctx context.Context, req *gen.SopPlanUpdate, params gen.UpdateSopPlanParams) (gen.UpdateSopPlanRes, error) {
	p, err := e.SetConsensus(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Consensus)
	out, tag, err := e.oneSop(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetSopPlanOKHeaders{ETag: tag, Response: out}, nil
}

// FixSopPlan implements POST /v1/sop-plans/{id}:fix.
func (e *Endpoints) FixSopPlan(ctx context.Context, params gen.FixSopPlanParams) (gen.FixSopPlanRes, error) {
	p, err := e.FixSop(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.oneSop(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetSopPlanOKHeaders{ETag: tag, Response: out}, nil
}

// SubmitSopPlan implements POST /v1/sop-plans/{id}:submit.
func (e *Endpoints) SubmitSopPlan(ctx context.Context, params gen.SubmitSopPlanParams) (gen.SubmitSopPlanRes, error) {
	p, err := e.SubmitSop(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.oneSop(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetSopPlanOKHeaders{ETag: tag, Response: out}, nil
}

// ListSopPlans implements GET /v1/sop-plans: the versions of the
// companies the caller may act in, in the order they were started,
// filtered by company and status.
func (e *Endpoints) ListSopPlans(ctx context.Context, params gen.ListSopPlansParams) (gen.ListSopPlansRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	ks, err := e.Pages.Keyset(createdSort, fmt.Sprintf("listSopPlans|%s|%s", le, params.Status.Value), apitypes.Page(params.OrderBy, params.PageSize,
		params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("mrp: page of S&OP plans: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(sopSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR legal_entity_id = $3) AND ($4 = '' OR status = $4)`,
		[]any{access.All, access.IDs, le, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SopPlan, error) { return scanSop(r) })
	if err != nil {
		return nil, fmt.Errorf("mrp: list S&OP plans: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ps, func(p SopPlan) (string, uuid.UUID) { return pagination.Instant(p.CreatedAt), p.ID },
		func(page []SopPlan) (map[uuid.UUID]string, error) {
			if err := withSopLines(ctx, tx, page); err != nil {
				return nil, err
			}
			return e.sopCompanies(ctx, page)
		}, sopAPI)
	if err != nil {
		return nil, err
	}
	return &gen.SopPlanPage{Items: items, NextPageToken: next}, nil
}
