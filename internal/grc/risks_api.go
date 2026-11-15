package grc

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// riskNames reads the names of the people the risks and ratings name by
// id (owners, raters, closers), in one call.
func (e *Endpoints) riskNames(ctx context.Context, risks []Risk, ratings []Rating) (map[uuid.UUID]Person, error) {
	var ids []uuid.UUID
	for _, r := range risks {
		ids = append(ids, r.OwnerID, r.RatedBy)
		if r.ClosedBy != nil {
			ids = append(ids, *r.ClosedBy)
		}
	}
	for _, r := range ratings {
		ids = append(ids, r.RatedBy)
	}
	people, err := e.People.Named(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("grc: names of %d people: %w", len(ids), err)
	}
	return people, nil
}

// riskToAPI is a risk as the API gives it, with its people's names and
// its controls.
func riskToAPI(r Risk, people map[uuid.UUID]Person) gen.Risk {
	out := gen.Risk{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.UpdatedBy), Title: r.Title, Category: r.Category, OwnerID: r.OwnerID, OwnerName: people[r.OwnerID].Name,
		Likelihood: int32(r.Likelihood), Impact: int32(r.Impact), Score: int32(r.Score), RatedAt: r.RatedAt, RatedBy: r.RatedBy,
		RatedByName: people[r.RatedBy].Name, Status: r.Status, Controls: make([]gen.RiskControl, len(r.Controls))}
	for i, c := range r.Controls {
		out.Controls[i] = gen.RiskControl{ControlID: c.ControlID, Code: c.Code, Status: c.Status}
	}
	if r.ClosedBy != nil {
		out.CloseReason.SetTo(*r.CloseReason)
		out.ClosedAt.SetTo(*r.ClosedAt)
		out.ClosedBy.SetTo(*r.ClosedBy)
		out.ClosedByName.SetTo(people[*r.ClosedBy].Name)
	}
	return out
}

// ratingToAPI is a rating as the API gives it, with its rater's name.
func ratingToAPI(r Rating, people map[uuid.UUID]Person) gen.RiskRating {
	return gen.RiskRating{ID: r.ID, RiskID: r.RiskID, Likelihood: int32(r.Likelihood), Impact: int32(r.Impact), Score: int32(r.Score()),
		RatedBy: r.RatedBy, RatedByName: people[r.RatedBy].Name, RatedAt: r.RatedAt}
}

// oneRisk is a single risk as the API gives it, with its ETag.
func (e *Endpoints) oneRisk(ctx context.Context, r Risk, err error) (gen.Risk, gen.OptString, error) {
	if err != nil {
		return gen.Risk{}, gen.OptString{}, err
	}
	people, err := e.riskNames(ctx, []Risk{r}, nil)
	if err != nil {
		return gen.Risk{}, gen.OptString{}, err
	}
	return riskToAPI(r, people), gen.NewOptString(etag.Format(r.Version)), nil
}

// CreateRisk implements POST /v1/risks.
func (e *Endpoints) CreateRisk(ctx context.Context, req *gen.RiskCreate, _ gen.CreateRiskParams) (gen.CreateRiskRes, error) {
	r, err := e.RecordRisk(ctx, NewRisk{Title: req.Title, Category: req.Category, OwnerID: req.OwnerID, Likelihood: int16(req.Likelihood),
		Impact: int16(req.Impact), ControlIDs: req.ControlIds})
	out, tag, err := e.oneRisk(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.RiskHeaders{ETag: tag, Location: gen.NewOptString("/v1/risks/" + r.ID.String()), Response: out}, nil
}

// GetRisk implements GET /v1/risks/{id}.
func (e *Endpoints) GetRisk(ctx context.Context, params gen.GetRiskParams) (gen.GetRiskRes, error) {
	r, err := GetRisk(ctx, params.ID)
	out, tag, err := e.oneRisk(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetRiskOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateRisk implements PATCH /v1/risks/{id}.
func (e *Endpoints) UpdateRisk(ctx context.Context, req *gen.RiskUpdate, params gen.UpdateRiskParams) (gen.UpdateRiskRes, error) {
	ch := RiskChange{Title: apitypes.Ptr[string](req.Title), Category: apitypes.Ptr[string](req.Category), OwnerID: apitypes.Ptr[uuid.UUID](req.OwnerID)}
	if req.ControlIds != nil { // absent from the patch: nil; given, even empty: a list
		ch.ControlIDs = &req.ControlIds
	}
	r, err := e.ChangeRisk(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	out, tag, err := e.oneRisk(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateRiskOKHeaders{ETag: tag, Response: out}, nil
}

// CloseRisk implements POST /v1/risks/{id}:close.
func (e *Endpoints) CloseRisk(ctx context.Context, req *gen.RiskClose, params gen.CloseRiskParams) (gen.CloseRiskRes, error) {
	r, err := CloseRisk(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	out, tag, err := e.oneRisk(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateRiskOKHeaders{ETag: tag, Response: out}, nil
}

// CreateRiskRating implements POST /v1/risks/{id}/ratings.
func (e *Endpoints) CreateRiskRating(ctx context.Context, req *gen.RiskRatingCreate, params gen.CreateRiskRatingParams) (gen.CreateRiskRatingRes, error) {
	r, err := Rate(ctx, params.ID, int16(req.Likelihood), int16(req.Impact))
	if err != nil {
		return nil, err
	}
	people, err := e.riskNames(ctx, nil, []Rating{r})
	if err != nil {
		return nil, err
	}
	return &gen.RiskRatingHeaders{Response: ratingToAPI(r, people)}, nil
}

// registerCursor is where a page of the register ended: its last risk's
// place in the register's order.
type registerCursor struct {
	Score int16     `json:"s"`
	Title string    `json:"t"`
	ID    uuid.UUID `json:"i"`
}

// ListRisks implements GET /v1/risks: the tenant's register, the highest
// score first, then by title and id, filtered by status, category, owner
// and a control that mitigates the risk now. A page continues after the
// last risk's score, title and id.
func (e *Endpoints) ListRisks(ctx context.Context, params gen.ListRisksParams) (gen.ListRisksRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	status, category := apitypes.Ptr[string](params.Status), apitypes.Ptr[string](params.Category)
	owner, control := apitypes.Ptr[uuid.UUID](params.OwnerID), apitypes.Ptr[uuid.UUID](params.ControlID)
	scope := fmt.Sprintf("listRisks|%s|%q|%s|%s", params.Status.Value, params.Category.Value, owner, control)
	var after *registerCursor
	if params.PageToken.Set {
		after = &registerCursor{}
		if err := e.Pages.Decode(scope, params.PageToken.Value, after); err != nil {
			return nil, err
		}
	}
	risks, err := register(ctx, tx, []any{status, category, owner, control}, after, size)
	if err != nil {
		return nil, err
	}
	out := &gen.RiskPage{Items: []gen.Risk{}}
	if len(risks) > size {
		risks = risks[:size]
		last := risks[size-1]
		next, _ := e.Pages.Encode(scope, registerCursor{Score: last.Score, Title: last.Title, ID: last.ID}) // a number, a text and a uuid always encode
		out.NextPageToken.SetTo(next)
	}
	people, err := e.riskNames(ctx, risks, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range risks {
		out.Items = append(out.Items, riskToAPI(r, people))
	}
	return out, nil
}

// register reads one page of the register and one risk more, after the
// cursor when there is one, with their controls; filters are the status,
// category, owner and control, each nil for none.
func register(ctx context.Context, tx pgx.Tx, filters []any, after *registerCursor, size int) ([]Risk, error) {
	var score *int16
	var title *string
	var id *uuid.UUID
	if after != nil {
		score, title, id = &after.Score, &after.Title, &after.ID
	}
	rows, _ := tx.Query(ctx, riskSelect+` WHERE ($1::text IS NULL OR r.status = $1) AND ($2::text IS NULL OR r.category = $2)
		AND ($3::uuid IS NULL OR r.owner_id = $3)
		AND ($4::uuid IS NULL OR EXISTS (SELECT FROM grc.risk_control rc WHERE rc.tenant_id = r.tenant_id AND rc.risk_id = r.id
			AND rc.control_id = $4 AND rc.status = 'active'))
		AND ($5::smallint IS NULL OR r.score < $5 OR (r.score = $5 AND (r.title, r.id) > ($6::text, $7::uuid)))
		ORDER BY r.score DESC, r.title, r.id LIMIT $8`, append([]any{pagination.PlanPerCall}, append(filters, score, title, id, size+1)...)...)
	risks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Risk, error) { return scanRisk(r) })
	if err != nil {
		return nil, fmt.Errorf("grc: list risks: %w", err)
	}
	if err := mitigatedBy(ctx, tx, risks); err != nil {
		return nil, err
	}
	return risks, nil
}

var ratingSort = pagination.Sort{Default: "id", DefaultDesc: true, Fields: []pagination.Field{{Name: "id", Column: "id", Type: "uuid"}}}

// ListRiskRatings implements GET /v1/risks/{id}/ratings: one risk's
// ratings, the latest first (a rating's id is a UUIDv7), so the first is
// the risk's.
func (e *Endpoints) ListRiskRatings(ctx context.Context, params gen.ListRiskRatingsParams) (gen.ListRiskRatingsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := risk(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(ratingSort, "listRiskRatings|"+r.ID.String(), apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("grc: page of the ratings of risk %s: %w", r.ID, err)
	}
	q, args := ks.SQL(`SELECT id, likelihood, impact, rated_by, created_at FROM grc.risk_rating WHERE risk_id = $1`, []any{r.ID})
	rows, _ := tx.Query(ctx, q, args...)
	ratings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rating, error) {
		x := Rating{RiskID: r.ID}
		return x, errs.Wrapf(row.Scan(&x.ID, &x.Likelihood, &x.Impact, &x.RatedBy, &x.RatedAt), "scan risk rating")
	})
	if err != nil {
		return nil, fmt.Errorf("grc: list the ratings of risk %s: %w", r.ID, err)
	}
	items, next, err := apitypes.PageWith(ks, ratings, func(x Rating) (string, uuid.UUID) { return x.ID.String(), x.ID },
		func(page []Rating) (map[uuid.UUID]Person, error) { return e.riskNames(ctx, nil, page) }, ratingToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RiskRatingPage{Items: items, NextPageToken: next}, nil
}
