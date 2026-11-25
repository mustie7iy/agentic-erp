package mrp

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// refs are what plans and runs are shown with: their plants, companies,
// materials and accounts.
type refs struct {
	plants    map[uuid.UUID]tenancy.Unit
	companies map[uuid.UUID]string
	materials map[uuid.UUID]materials.Material
	accounts  map[uuid.UUID]string
}

// read reads the refs of the plants, materials and accounts named; the
// plants' companies come with them.
func (e *Endpoints) read(ctx context.Context, plants, ms, accounts []uuid.UUID) (refs, error) {
	r := refs{companies: map[uuid.UUID]string{}}
	var err error
	if r.plants, err = e.Plants.OrgUnits(ctx, plants); err != nil {
		return refs{}, fmt.Errorf("mrp: plants: %w", err)
	}
	for _, u := range r.plants { // a page names few plants: one read each
		le, _, err := e.Plants.Entity(ctx, u.LegalEntityID)
		if err != nil {
			return refs{}, fmt.Errorf("mrp: company of plant %s: %w", u.Code, err)
		}
		r.companies[u.LegalEntityID] = le.Code
	}
	if r.materials, err = e.Materials.Materials(ctx, ms); err != nil {
		return refs{}, fmt.Errorf("mrp: materials: %w", err)
	}
	if r.accounts, err = e.Accounts.AccountNumbers(ctx, accounts); err != nil {
		return refs{}, fmt.Errorf("mrp: accounts: %w", err)
	}
	return r, nil
}

// planRefs reads what plans name.
func (e *Endpoints) planRefs(ctx context.Context, ps []Plan) (refs, error) {
	var plants, ms, accounts []uuid.UUID
	for _, p := range ps {
		plants, ms, accounts = append(plants, p.PlantID), append(ms, p.MaterialID), append(accounts, p.AccountID)
	}
	return e.read(ctx, plants, ms, accounts)
}

// planAPI is a plan as the API gives it.
func planAPI(p Plan, r refs) gen.MaterialPlan {
	m, plant := r.materials[p.MaterialID], r.plants[p.PlantID]
	return gen.MaterialPlan{ID: p.ID, Version: p.Version, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(p.UpdatedBy), PlantID: p.PlantID, PlantCode: plant.Code, PlantName: plant.Name, LegalEntityID: p.LegalEntityID,
		CompanyCode: r.companies[p.LegalEntityID], MaterialID: p.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
		MrpType: p.MRPType, ReorderPoint: gen.Quantity{Value: p.ReorderPoint, Unit: m.BaseUnit}, AccountID: p.AccountID, AccountNumber: r.accounts[p.AccountID],
		ReplenishmentLeadTimeDays: apitypes.OptInt32(p.LeadDays)}
}

// onePlan is a plan as the API gives it, with its ETag.
func (e *Endpoints) onePlan(ctx context.Context, p Plan, err error) (gen.MaterialPlan, gen.OptString, error) {
	if err != nil {
		return gen.MaterialPlan{}, gen.OptString{}, err
	}
	r, err := e.planRefs(ctx, []Plan{p})
	if err != nil {
		return gen.MaterialPlan{}, gen.OptString{}, err
	}
	return planAPI(p, r), gen.NewOptString(etag.Format(p.Version)), nil
}

// CreateMaterialPlan implements POST /v1/material-plans.
func (e *Endpoints) CreateMaterialPlan(ctx context.Context, req *gen.MaterialPlanCreate, _ gen.CreateMaterialPlanParams) (gen.CreateMaterialPlanRes, error) {
	p, err := e.CreatePlan(ctx, PlanInput{PlantID: req.PlantID, MaterialID: req.MaterialID, AccountID: req.AccountID, MRPType: req.MrpType,
		ReorderPoint: req.ReorderPoint, LeadDays: apitypes.Ptr[int32](req.ReplenishmentLeadTimeDays)})
	out, tag, err := e.onePlan(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialPlanHeaders{ETag: tag, Location: gen.NewOptString("/v1/material-plans/" + p.ID.String()), Response: out}, nil
}

// GetMaterialPlan implements GET /v1/material-plans/{id}.
func (e *Endpoints) GetMaterialPlan(ctx context.Context, params gen.GetMaterialPlanParams) (gen.GetMaterialPlanRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	p, err := loadPlan(ctx, tx, params.ID, false)
	out, tag, err := e.onePlan(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetMaterialPlanOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateMaterialPlan implements PATCH /v1/material-plans/{id}.
func (e *Endpoints) UpdateMaterialPlan(ctx context.Context, req *gen.MaterialPlanUpdate, params gen.UpdateMaterialPlanParams) (gen.UpdateMaterialPlanRes, error) {
	p, err := e.ChangePlan(ctx, params.ID, etag.IfMatch(params.IfMatch), PlanChange{MRPType: apitypes.Ptr[string](req.MrpType),
		ReorderPoint: apitypes.Ptr[string](req.ReorderPoint), AccountID: apitypes.Ptr[uuid.UUID](req.AccountID),
		LeadDays: apitypes.OptNil[int32](req.ReplenishmentLeadTimeDays)})
	out, tag, err := e.onePlan(ctx, p, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetMaterialPlanOKHeaders{ETag: tag, Response: out}, nil
}

var createdSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListMaterialPlans implements GET /v1/material-plans: the plans of the
// companies the caller may act in, in the order they were created,
// filtered by plant and material.
func (e *Endpoints) ListMaterialPlans(ctx context.Context, params gen.ListMaterialPlansParams) (gen.ListMaterialPlansRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant, material := apitypes.Ptr[uuid.UUID](params.PlantID), apitypes.Ptr[uuid.UUID](params.MaterialID)
	ks, err := e.Pages.Keyset(createdSort, fmt.Sprintf("listMaterialPlans|%s|%s", plant, material), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("mrp: page of material plans: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(planSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR plant_id = $3) AND ($4::uuid IS NULL OR material_id = $4)`,
		[]any{access.All, access.IDs, plant, material})
	rows, _ := tx.Query(ctx, q, args...)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Plan, error) { return scanPlan(r) })
	if err != nil {
		return nil, fmt.Errorf("mrp: list material plans: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, ps, func(p Plan) (string, uuid.UUID) { return pagination.Instant(p.CreatedAt), p.ID },
		func(page []Plan) (refs, error) { return e.planRefs(ctx, page) }, planAPI)
	if err != nil {
		return nil, err
	}
	return &gen.MaterialPlanPage{Items: items, NextPageToken: next}, nil
}

// StartMrpRun implements POST /v1/mrp-runs.
func (e *Endpoints) StartMrpRun(ctx context.Context, req *gen.MrpRunStart, _ gen.StartMrpRunParams) (gen.StartMrpRunRes, error) {
	op, err := e.StartRun(ctx, req.PlantID)
	if err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op}, nil
}

// runRefs reads what runs name: their plants and their lines' and
// replacements' materials.
func (e *Endpoints) runRefs(ctx context.Context, rs []Run) (refs, error) {
	var plants, ms []uuid.UUID
	for _, r := range rs {
		plants = append(plants, r.PlantID)
		for _, l := range r.Lines {
			ms = append(ms, l.MaterialID)
		}
		for _, x := range r.Replacements {
			ms = append(ms, x.MaterialID)
		}
	}
	return e.read(ctx, plants, ms, nil)
}

// runAPI is a run as the API gives it, its lines by material number.
func runAPI(r Run, x refs) gen.MrpRun {
	plant := x.plants[r.PlantID]
	out := gen.MrpRun{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt, UpdatedBy: gen.NewOptUUID(r.UpdatedBy),
		PlantID: r.PlantID, PlantCode: plant.Code, PlantName: plant.Name, LegalEntityID: r.LegalEntityID, CompanyCode: x.companies[r.LegalEntityID],
		Status: r.Status, OperationID: r.OperationID, RunDate: apitypes.OptDate(r.RunDate), MaterialsPlanned: optCount(r.Planned),
		ProposalsCreated: optCount(r.Created), ProposalsReplaced: optCount(r.Replaced), Lines: make([]gen.MrpRunLine, len(r.Lines)),
		Replaced: make([]gen.MrpRunReplaced, len(r.Replacements))}
	if r.Error != nil {
		out.Error = gen.NewOptMrpRunError(gen.MrpRunError{Code: r.Error.Code, Detail: r.Error.Detail})
	}
	for i, l := range r.Lines {
		m := x.materials[l.MaterialID]
		q := func(v string) gen.Quantity { return gen.Quantity{Value: v, Unit: l.Unit} }
		out.Lines[i] = gen.MrpRunLine{MaterialID: l.MaterialID, MaterialNumber: m.Number, MaterialDescription: m.Description,
			ReorderPoint: q(l.ReorderPoint), Demand: q(l.Demand), Stock: q(l.Stock), Receipts: q(l.Receipts), Shortage: q(l.Shortage),
			RequisitionID: apitypes.OptUUID(l.RequisitionID), RequisitionNumber: apitypes.OptString(l.RequisitionNumber)}
	}
	slices.SortStableFunc(out.Lines, func(a, b gen.MrpRunLine) int { return cmp.Compare(a.MaterialNumber, b.MaterialNumber) })
	for i, p := range r.Replacements {
		out.Replaced[i] = gen.MrpRunReplaced{RequisitionID: p.RequisitionID, RequisitionNumber: p.RequisitionNumber, MaterialID: p.MaterialID,
			MaterialNumber: x.materials[p.MaterialID].Number}
	}
	return out
}

// optCount is a count once known.
func optCount(n *int32) gen.OptInt32 {
	if n == nil {
		return gen.OptInt32{}
	}
	return gen.NewOptInt32(*n)
}

// GetMrpRun implements GET /v1/mrp-runs/{id}.
func (e *Endpoints) GetMrpRun(ctx context.Context, params gen.GetMrpRunParams) (gen.GetMrpRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	x, err := e.runRefs(ctx, []Run{r})
	if err != nil {
		return nil, err
	}
	return &gen.MrpRunHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: runAPI(r, x)}, nil
}

// ListMrpRuns implements GET /v1/mrp-runs: the runs of the companies the
// caller may act in, in the order they were started, filtered by plant
// and status.
func (e *Endpoints) ListMrpRuns(ctx context.Context, params gen.ListMrpRunsParams) (gen.ListMrpRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	plant := apitypes.Ptr[uuid.UUID](params.PlantID)
	ks, err := e.Pages.Keyset(createdSort, fmt.Sprintf("listMrpRuns|%s|%s", plant, params.Status.Value), apitypes.Page(params.OrderBy, params.PageSize,
		params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("mrp: page of planning runs: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(runSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR plant_id = $3) AND ($4 = '' OR status = $4)`,
		[]any{access.All, access.IDs, plant, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("mrp: list planning runs: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, rs, func(r Run) (string, uuid.UUID) { return pagination.Instant(r.CreatedAt), r.ID },
		func(page []Run) (refs, error) {
			if err := withDetails(ctx, tx, page); err != nil {
				return refs{}, err
			}
			return e.runRefs(ctx, page)
		}, runAPI)
	if err != nil {
		return nil, err
	}
	return &gen.MrpRunPage{Items: items, NextPageToken: next}, nil
}
