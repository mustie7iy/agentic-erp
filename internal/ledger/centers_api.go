package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func optDate(t time.Time) gen.OptDate {
	if t.IsZero() {
		return gen.OptDate{}
	}
	return gen.NewOptDate(t)
}

func optCenter(id uuid.UUID) gen.OptUUID {
	if id == uuid.Nil {
		return gen.OptUUID{}
	}
	return gen.NewOptUUID(id)
}

func costCenterToAPI(c CostCenter) gen.CostCenter {
	from, to, _ := validity(c.Periods)
	out := gen.CostCenter{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), LegalEntityID: c.LegalEntityID, Code: c.Code, Name: c.Texts.Name,
		Description: apitypes.OptString(c.Texts.Description), Responsible: c.Texts.Responsible, Status: c.Status, ValidFrom: optDate(from),
		ValidTo: optDate(to), Periods: make([]gen.CostCenterPeriod, len(c.Periods)), PendingChangeID: apitypes.OptUUID(c.PendingChangeID)}
	for i, s := range c.Periods {
		out.Periods[i] = gen.CostCenterPeriod{ValidFrom: s.From, ValidTo: optDate(s.To), Category: s.Attr.Category,
			ProfitCenterID: optCenter(s.Attr.ProfitCenterID), LockActualCost: s.Attr.LockCost, LockActualRevenue: s.Attr.LockRevenue, LockPlan: s.Attr.LockPlan}
	}
	return out
}

func profitCenterToAPI(c ProfitCenter) gen.ProfitCenter {
	from, to, _ := validity(c.Periods)
	out := gen.ProfitCenter{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), Code: c.Code, Name: c.Texts.Name, Description: apitypes.OptString(c.Texts.Description),
		Responsible: c.Texts.Responsible, SegmentID: apitypes.OptUUID(c.SegmentID), Status: c.Status, LegalEntityIds: c.LegalEntityIDs,
		ValidFrom: optDate(from), ValidTo: optDate(to), Periods: make([]gen.ProfitCenterPeriod, len(c.Periods)),
		PendingChangeID: apitypes.OptUUID(c.PendingChangeID)}
	for i, s := range c.Periods {
		out.Periods[i] = gen.ProfitCenterPeriod{ValidFrom: s.From, ValidTo: optDate(s.To), Locked: s.Attr.Locked}
	}
	return out
}

func segmentToAPI(s Segment) gen.Segment {
	return gen.Segment{ID: s.ID, Version: s.Version, CreatedAt: s.CreatedAt, CreatedBy: s.CreatedBy, UpdatedAt: s.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(s.UpdatedBy), Code: s.Code, Name: s.Name, Description: apitypes.OptString(s.Description)}
}

func changeToAPI(c CenterChange) gen.CenterChange {
	out := gen.CenterChange{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), CostCenterID: apitypes.OptUUID(c.CostCenterID), ProfitCenterID: apitypes.OptUUID(c.ProfitCenterID),
		Kind: c.Kind, Payload: gen.CenterChangePayload(apitypes.EncodeObject(c.Payload)), Reason: c.Reason, Status: c.Status,
		WorkflowRequestID: apitypes.OptUUID(c.WorkflowRequestID), RequestedBy: c.RequestedBy}
	if c.AppliedAt != nil {
		out.AppliedAt.SetTo(*c.AppliedAt)
	}
	return out
}

func texts(name, responsible string, description gen.OptString) Texts {
	return Texts{Name: name, Description: apitypes.Ptr[string](description), Responsible: responsible}
}

func textChange(req *gen.CenterTextsUpdate) TextChange {
	return TextChange{Name: apitypes.Ptr[string](req.Name), Description: apitypes.OptNilString(req.Description),
		Responsible: apitypes.Ptr[string](req.Responsible)}
}

func dateOr(d gen.OptDate) time.Time {
	if v, ok := d.Get(); ok {
		return dayOrOpen(v)
	}
	return time.Time{}
}

// redate is a requested validity, completed from the current one (from,
// to): a date not given stays, a null end opens it.
func redate(req *gen.CenterRedate, from, to time.Time) (string, string) {
	if v, ok := req.ValidFrom.Get(); ok {
		from = day(v)
	}
	if req.ValidTo.Set {
		to = time.Time{}
		if !req.ValidTo.Null {
			to = dayOrOpen(req.ValidTo.Value)
		}
	}
	return dateText(from), dateText(to)
}

func optBool(b gen.OptBool) *bool { return apitypes.Ptr[bool](b) }

// CreateCostCenter implements POST /v1/cost-centers.
func (e *Endpoints) CreateCostCenter(ctx context.Context, req *gen.CostCenterCreate, _ gen.CreateCostCenterParams) (gen.CreateCostCenterRes, error) {
	c, err := e.RequestCostCenter(ctx, NewCostCenter{LegalEntityID: req.LegalEntityID, Code: req.Code, Texts: texts(req.Name, req.Responsible, req.Description),
		Category: req.Category, ProfitCenterID: apitypes.Ptr[uuid.UUID](req.ProfitCenterID), ValidFrom: req.ValidFrom, ValidTo: dateOr(req.ValidTo), Reason: req.Reason})
	if err != nil {
		return nil, err
	}
	return &gen.CostCenterHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/cost-centers/" + c.ID.String()),
		Response: costCenterToAPI(c)}, nil
}

// GetCostCenter implements GET /v1/cost-centers/{id}.
func (e *Endpoints) GetCostCenter(ctx context.Context, params gen.GetCostCenterParams) (gen.GetCostCenterRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getCostCenter(ctx, tx, access, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetCostCenterOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: costCenterToAPI(c)}, nil
}

var codeSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "c.code", Type: "text"}}}

// ListCostCenters implements GET /v1/cost-centers.
func (e *Endpoints) ListCostCenters(ctx context.Context, params gen.ListCostCentersParams) (gen.ListCostCentersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	scope := fmt.Sprintf("listCostCenters|%s|%s|%s", le, params.Code.Value, params.Status.Value)
	ks, err := e.Pages.Keyset(codeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(costCenterSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR c.legal_entity_id = $3)
		AND ($4 = '' OR c.code = $4) AND ($5 = '' OR c.status = $5)`, []any{access.All, access.IDs, le, params.Code.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CostCenter, error) { return scanCostCenter(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list cost centers: %w", err)
	}
	periods, err := costCenterPeriods(ctx, tx, ids(cs, func(c CostCenter) uuid.UUID { return c.ID }))
	if err != nil {
		return nil, err
	}
	for i := range cs {
		cs[i].Periods = periods[cs[i].ID]
	}
	items, next := apitypes.PageOf(ks, cs, func(c CostCenter) (string, uuid.UUID) { return c.Code, c.ID }, costCenterToAPI)
	return &gen.CostCenterPageHeaders{Response: gen.CostCenterPage{Items: items, NextPageToken: next}}, nil
}

func ids[T any](rows []T, id func(T) uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = id(r)
	}
	return out
}

// UpdateCostCenter implements PATCH /v1/cost-centers/{id}.
func (e *Endpoints) UpdateCostCenter(ctx context.Context, req *gen.CenterTextsUpdate, params gen.UpdateCostCenterParams) (gen.UpdateCostCenterRes, error) {
	c, err := UpdateCostCenterTexts(ctx, params.ID, etag.IfMatch(params.IfMatch), textChange(req))
	if err != nil {
		return nil, err
	}
	return &gen.UpdateCostCenterOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: costCenterToAPI(c)}, nil
}

func changeOK(c CenterChange, err error) (*gen.LockCostCenterOKHeaders, error) {
	if err != nil {
		return nil, err
	}
	return &gen.LockCostCenterOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: changeToAPI(c)}, nil
}

// RedateCostCenter implements POST /v1/cost-centers/{id}:redate.
func (e *Endpoints) RedateCostCenter(ctx context.Context, req *gen.CenterRedate, params gen.RedateCostCenterParams) (gen.RedateCostCenterRes, error) {
	return changeOK(e.RequestCostCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindRedate, func(from, to time.Time) costCenterPayload {
		f, t := redate(req, from, to)
		return costCenterPayload{ValidFrom: f, ValidTo: t}
	}, req.Reason))
}

// LockCostCenter implements POST /v1/cost-centers/{id}:lock.
func (e *Endpoints) LockCostCenter(ctx context.Context, req *gen.CostCenterLock, params gen.LockCostCenterParams) (gen.LockCostCenterRes, error) {
	return changeOK(e.RequestCostCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindLock, func(time.Time, time.Time) costCenterPayload {
		return costCenterPayload{ValidFrom: dateText(day(req.ValidFrom)), ValidTo: dateText(dateOr(req.ValidTo)), LockCost: optBool(req.LockActualCost),
			LockRevenue: optBool(req.LockActualRevenue), LockPlan: optBool(req.LockPlan)}
	}, req.Reason))
}

// MoveCostCenter implements POST /v1/cost-centers/{id}:move.
func (e *Endpoints) MoveCostCenter(ctx context.Context, req *gen.CostCenterMove, params gen.MoveCostCenterParams) (gen.MoveCostCenterRes, error) {
	return changeOK(e.RequestCostCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindMove, func(time.Time, time.Time) costCenterPayload {
		p := costCenterPayload{ValidFrom: dateText(day(req.ValidFrom)), Category: req.Category.Value}
		if req.ProfitCenterID.Set {
			p.ProfitCenter = &pcChoice{}
			if !req.ProfitCenterID.Null {
				p.ProfitCenter.ID = &req.ProfitCenterID.Value
			}
		}
		return p
	}, req.Reason))
}

// CreateProfitCenter implements POST /v1/profit-centers.
func (e *Endpoints) CreateProfitCenter(ctx context.Context, req *gen.ProfitCenterCreate, _ gen.CreateProfitCenterParams) (gen.CreateProfitCenterRes, error) {
	c, err := e.RequestProfitCenter(ctx, NewProfitCenter{Code: req.Code, Texts: texts(req.Name, req.Responsible, req.Description),
		SegmentID: apitypes.Ptr[uuid.UUID](req.SegmentID), LegalEntityIDs: req.LegalEntityIds, ValidFrom: req.ValidFrom, ValidTo: dateOr(req.ValidTo),
		Reason: req.Reason})
	if err != nil {
		return nil, err
	}
	return &gen.ProfitCenterHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/profit-centers/" + c.ID.String()),
		Response: profitCenterToAPI(c)}, nil
}

// GetProfitCenter implements GET /v1/profit-centers/{id}.
func (e *Endpoints) GetProfitCenter(ctx context.Context, params gen.GetProfitCenterParams) (gen.GetProfitCenterRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getProfitCenter(ctx, tx, params.ID, noLock)
	if err != nil {
		return nil, err
	}
	return &gen.GetProfitCenterOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: profitCenterToAPI(c)}, nil
}

// ListProfitCenters implements GET /v1/profit-centers.
func (e *Endpoints) ListProfitCenters(ctx context.Context, params gen.ListProfitCentersParams) (gen.ListProfitCentersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, seg := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SegmentID)
	scope := fmt.Sprintf("listProfitCenters|%s|%s|%s|%s", le, seg, params.Code.Value, params.Status.Value)
	ks, err := e.Pages.Keyset(codeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(profitCenterSelect+` WHERE ($1::uuid IS NULL OR EXISTS (SELECT FROM ledger.profit_center_company pc
			WHERE pc.tenant_id = c.tenant_id AND pc.profit_center_id = c.id AND pc.legal_entity_id = $1))
		AND ($2::uuid IS NULL OR c.segment_id = $2) AND ($3 = '' OR c.code = $3) AND ($4 = '' OR c.status = $4)`,
		[]any{le, seg, params.Code.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProfitCenter, error) { return scanProfitCenter(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list profit centers: %w", err)
	}
	periods, err := profitCenterPeriods(ctx, tx, ids(cs, func(c ProfitCenter) uuid.UUID { return c.ID }))
	if err != nil {
		return nil, err
	}
	for i := range cs {
		cs[i].Periods = periods[cs[i].ID]
	}
	items, next := apitypes.PageOf(ks, cs, func(c ProfitCenter) (string, uuid.UUID) { return c.Code, c.ID }, profitCenterToAPI)
	return &gen.ProfitCenterPageHeaders{Response: gen.ProfitCenterPage{Items: items, NextPageToken: next}}, nil
}

// UpdateProfitCenter implements PATCH /v1/profit-centers/{id}.
func (e *Endpoints) UpdateProfitCenter(ctx context.Context, req *gen.CenterTextsUpdate, params gen.UpdateProfitCenterParams) (gen.UpdateProfitCenterRes, error) {
	c, err := UpdateProfitCenterTexts(ctx, params.ID, etag.IfMatch(params.IfMatch), textChange(req))
	if err != nil {
		return nil, err
	}
	return &gen.UpdateProfitCenterOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: profitCenterToAPI(c)}, nil
}

// RedateProfitCenter implements POST /v1/profit-centers/{id}:redate.
func (e *Endpoints) RedateProfitCenter(ctx context.Context, req *gen.CenterRedate, params gen.RedateProfitCenterParams) (gen.RedateProfitCenterRes, error) {
	return changeOK(e.RequestProfitCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindRedate, func(from, to time.Time) profitCenterPayload {
		f, t := redate(req, from, to)
		return profitCenterPayload{ValidFrom: f, ValidTo: t}
	}, req.Reason))
}

// LockProfitCenter implements POST /v1/profit-centers/{id}:lock.
func (e *Endpoints) LockProfitCenter(ctx context.Context, req *gen.ProfitCenterLock, params gen.LockProfitCenterParams) (gen.LockProfitCenterRes, error) {
	return changeOK(e.RequestProfitCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindLock, func(time.Time, time.Time) profitCenterPayload {
		return profitCenterPayload{ValidFrom: dateText(day(req.ValidFrom)), ValidTo: dateText(dateOr(req.ValidTo)), Locked: &req.Locked}
	}, req.Reason))
}

// MoveProfitCenter implements POST /v1/profit-centers/{id}:move.
func (e *Endpoints) MoveProfitCenter(ctx context.Context, req *gen.ProfitCenterMove, params gen.MoveProfitCenterParams) (gen.MoveProfitCenterRes, error) {
	return changeOK(e.RequestProfitCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindMove, func(time.Time, time.Time) profitCenterPayload {
		p := profitCenterPayload{Segment: &segmentRef{}}
		if !req.SegmentID.Null {
			p.Segment.ID = &req.SegmentID.Value
		}
		return p
	}, req.Reason))
}

// AssignProfitCenter implements POST /v1/profit-centers/{id}:assign.
func (e *Endpoints) AssignProfitCenter(ctx context.Context, req *gen.ProfitCenterAssign, params gen.AssignProfitCenterParams) (gen.AssignProfitCenterRes, error) {
	return changeOK(e.RequestProfitCenterChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindAssign, func(time.Time, time.Time) profitCenterPayload {
		return profitCenterPayload{LegalEntityIDs: req.LegalEntityIds}
	}, req.Reason))
}

// CreateSegment implements POST /v1/segments.
func (e *Endpoints) CreateSegment(ctx context.Context, req *gen.SegmentCreate, _ gen.CreateSegmentParams) (gen.CreateSegmentRes, error) {
	s, err := CreateSegment(ctx, NewSegment{Code: req.Code, Name: req.Name, Description: apitypes.Ptr[string](req.Description)})
	if err != nil {
		return nil, err
	}
	return &gen.SegmentHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Location: gen.NewOptString("/v1/segments/" + s.ID.String()),
		Response: segmentToAPI(s)}, nil
}

// GetSegment implements GET /v1/segments/{id}.
func (e *Endpoints) GetSegment(ctx context.Context, params gen.GetSegmentParams) (gen.GetSegmentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	s, err := getSegment(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetSegmentOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: segmentToAPI(s)}, nil
}

var segmentSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

// ListSegments implements GET /v1/segments.
func (e *Endpoints) ListSegments(ctx context.Context, params gen.ListSegmentsParams) (gen.ListSegmentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(segmentSort, "listSegments|"+params.Code.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+segmentColumns+` FROM ledger.segment WHERE ($1 = '' OR code = $1)`, []any{params.Code.Value})
	rows, _ := tx.Query(ctx, q, args...)
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Segment, error) { return scanSegment(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list segments: %w", err)
	}
	items, next := apitypes.PageOf(ks, ss, func(s Segment) (string, uuid.UUID) { return s.Code, s.ID }, segmentToAPI)
	return &gen.SegmentPageHeaders{Response: gen.SegmentPage{Items: items, NextPageToken: next}}, nil
}

// UpdateSegment implements PATCH /v1/segments/{id}.
func (e *Endpoints) UpdateSegment(ctx context.Context, req *gen.SegmentUpdate, params gen.UpdateSegmentParams) (gen.UpdateSegmentRes, error) {
	s, err := UpdateSegment(ctx, params.ID, etag.IfMatch(params.IfMatch),
		TextChange{Name: apitypes.Ptr[string](req.Name), Description: apitypes.OptNilString(req.Description)})
	if err != nil {
		return nil, err
	}
	return &gen.UpdateSegmentOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: segmentToAPI(s)}, nil
}

// GetCenterChange implements GET /v1/center-changes/{id}.
func (e *Endpoints) GetCenterChange(ctx context.Context, params gen.GetCenterChangeParams) (gen.GetCenterChangeRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getChange(ctx, tx, access, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.CenterChangeHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: changeToAPI(c)}, nil
}

var changeSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "ch.created_at", Type: "timestamptz"}}}

// ListCenterChanges implements GET /v1/center-changes.
func (e *Endpoints) ListCenterChanges(ctx context.Context, params gen.ListCenterChangesParams) (gen.ListCenterChangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	cc, pc := apitypes.Ptr[uuid.UUID](params.CostCenterID), apitypes.Ptr[uuid.UUID](params.ProfitCenterID)
	scope := fmt.Sprintf("listCenterChanges|%s|%s|%s", cc, pc, params.Status.Value)
	ks, err := e.Pages.Keyset(changeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+changeColumns+` FROM ledger.center_change ch WHERE `+visibleChanges+`
		AND ($3::uuid IS NULL OR ch.cost_center_id = $3) AND ($4::uuid IS NULL OR ch.profit_center_id = $4) AND ($5 = '' OR ch.status = $5)`,
		[]any{access.All, access.IDs, cc, pc, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CenterChange, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list center changes: %w", err)
	}
	items, next := apitypes.PageOf(ks, cs, func(c CenterChange) (string, uuid.UUID) { return c.CreatedAt.Format(time.RFC3339Nano), c.ID }, changeToAPI)
	return &gen.CenterChangePageHeaders{Response: gen.CenterChangePage{Items: items, NextPageToken: next}}, nil
}
