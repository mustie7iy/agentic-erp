package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Endpoints implements the configuration part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
	Dependencies
}

func ifMatch(header string) func(int32) error {
	return func(v int32) error { return etag.Check(header, v) }
}

// rawObject turns a generated free-form object into JSON.
func rawObject[M ~map[string]jx.Raw](m M) json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = json.RawMessage(v)
	}
	b, _ := json.Marshal(out) // the generated decoder only passes valid JSON values
	return b
}

// object turns a stored JSON object into a generated free-form object.
func object[M ~map[string]jx.Raw](doc json.RawMessage) M {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(doc, &fields) // stored documents are JSON objects
	out := make(M, len(fields))
	for k, v := range fields {
		out[k] = jx.Raw(v)
	}
	return out
}

func crToAPI(c ChangeRequest) gen.ChangeRequest {
	out := gen.ChangeRequest{
		ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt, UpdatedBy: gen.NewOptUUID(c.UpdatedBy),
		Number: c.Number, Title: c.Title, Rationale: c.Rationale, AuthorID: c.AuthorID, BaseVersion: c.BaseVersion,
		Proposed: object[gen.ChangeRequestProposed](c.Proposed), Tier: int32(c.Tier), Status: c.Status,
		Items: make([]gen.ChangeItem, 0, len(c.Items)), Evidence: make([]gen.Evidence, 0, len(c.Evidence)),
	}
	if c.WorkflowRequestID != nil {
		out.WorkflowRequestID.SetTo(*c.WorkflowRequestID)
	}
	if c.ActivatedVersion != nil {
		out.ActivatedVersion.SetTo(*c.ActivatedVersion)
	}
	for _, it := range c.Items {
		item := gen.ChangeItem{ItemNo: it.No, Path: it.Path, Tier: int32(it.Tier)}
		if it.Old != nil {
			item.OldValue.SetTo(*it.Old)
		}
		if it.New != nil {
			item.NewValue.SetTo(*it.New)
		}
		out.Items = append(out.Items, item)
	}
	for _, e := range c.Evidence {
		out.Evidence = append(out.Evidence, gen.Evidence{Kind: e.Kind, Passed: e.Passed, Summary: e.Summary,
			Detail: object[gen.EvidenceDetail](e.Detail), RecordedAt: e.RecordedAt, RecordedBy: e.RecordedBy})
	}
	return out
}

func crOK(c ChangeRequest) *gen.UpdateChangeRequestOKHeaders {
	return &gen.UpdateChangeRequestOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: crToAPI(c)}
}

func pageRequest(orderBy gen.OptString, size gen.OptInt32, token gen.OptString) pagination.Request {
	return pagination.Request{OrderBy: orderBy.Value, PageSize: size.Value, PageSizeSet: size.Set, PageToken: token.Value, PageTokenSet: token.Set}
}

// CreateChangeRequest implements POST /v1/change-requests.
func (e *Endpoints) CreateChangeRequest(ctx context.Context, req *gen.ChangeRequestCreate, _ gen.CreateChangeRequestParams) (gen.CreateChangeRequestRes, error) {
	c, err := CreateChangeRequest(ctx, NewChangeRequest{Title: req.Title, Rationale: req.Rationale.Value, BaseVersion: req.BaseVersion,
		Proposed: rawObject(req.Proposed)})
	if err != nil {
		return nil, err
	}
	return &gen.ChangeRequestHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/change-requests/" + c.ID.String()),
		Response: crToAPI(c)}, nil
}

// GetChangeRequest implements GET /v1/change-requests/{id}.
func (e *Endpoints) GetChangeRequest(ctx context.Context, params gen.GetChangeRequestParams) (gen.GetChangeRequestRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getCR(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetChangeRequestOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: crToAPI(c)}, nil
}

var crSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "c.created_at", Type: "timestamptz"},
}}

// ListChangeRequests implements GET /v1/change-requests.
func (e *Endpoints) ListChangeRequests(ctx context.Context, params gen.ListChangeRequestsParams) (gen.ListChangeRequestsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(crSort, "listChangeRequests|"+params.Status.Value, pageRequest(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(crSelect+` WHERE ($1 = '' OR c.status = $1)`, []any{params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	crs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChangeRequest, error) { return scanCR(r) })
	if err != nil {
		return nil, err
	}
	crs, next := pagination.Page(ks, crs, func(c ChangeRequest) (string, uuid.UUID) { return c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ID })
	page := gen.ChangeRequestPage{Items: make([]gen.ChangeRequest, 0, len(crs))}
	for _, c := range crs {
		page.Items = append(page.Items, crToAPI(c))
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.ChangeRequestPageHeaders{Response: page}, nil
}

// UpdateChangeRequest implements PATCH /v1/change-requests/{id}.
func (e *Endpoints) UpdateChangeRequest(ctx context.Context, req *gen.ChangeRequestUpdate, params gen.UpdateChangeRequestParams) (gen.UpdateChangeRequestRes, error) {
	var ch ChangeRequestChange
	if req.Title.Set {
		ch.Title = &req.Title.Value
	}
	if req.Rationale.Set {
		ch.Rationale = &req.Rationale.Value
	}
	if req.BaseVersion.Set {
		ch.BaseVersion = &req.BaseVersion.Value
	}
	if req.Proposed.Set {
		ch.Proposed = rawObject(req.Proposed.Value)
	}
	c, err := UpdateChangeRequest(ctx, params.ID, ifMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return crOK(c), nil
}

// SubmitChangeRequest implements POST /v1/change-requests/{id}:submit.
func (e *Endpoints) SubmitChangeRequest(ctx context.Context, params gen.SubmitChangeRequestParams) (gen.SubmitChangeRequestRes, error) {
	c, err := SubmitChangeRequest(ctx, e.Dependencies, params.ID, ifMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return crOK(c), nil
}

// WithdrawChangeRequest implements POST /v1/change-requests/{id}:withdraw.
func (e *Endpoints) WithdrawChangeRequest(ctx context.Context, req *gen.ReasonInput, params gen.WithdrawChangeRequestParams) (gen.WithdrawChangeRequestRes, error) {
	c, err := WithdrawChangeRequest(ctx, e.Dependencies, params.ID, ifMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return crOK(c), nil
}

// ActivateChangeRequest implements POST /v1/change-requests/{id}:activate.
func (e *Endpoints) ActivateChangeRequest(ctx context.Context, params gen.ActivateChangeRequestParams) (gen.ActivateChangeRequestRes, error) {
	c, err := ActivateChangeRequest(ctx, e.Dependencies, params.ID, ifMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return crOK(c), nil
}

// SimulateChangeRequest implements POST /v1/change-requests/{id}:simulate:
// it starts a config.simulate operation that replays recent transactions
// against the proposal and records simulation evidence.
func (e *Endpoints) SimulateChangeRequest(ctx context.Context, params gen.SimulateChangeRequestParams) (gen.SimulateChangeRequestRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getCR(ctx, tx, params.ID, true)
	if err != nil {
		return nil, err
	}
	if err := etag.Check(params.IfMatch, c.Version); err != nil {
		return nil, err
	}
	if err := requireStatus(c, "simulate", AwaitingEvidence); err != nil {
		return nil, err
	}
	op, err := operations.Start(ctx, "config.simulate", map[string]any{"change_request_id": c.ID})
	if err != nil {
		return nil, err
	}
	if err := audit.Add(ctx, audit.Entry{Action: "config.change_request.simulate", ObjectType: "config.change_request", ObjectID: c.ID,
		Outcome: "succeeded", Detail: map[string]any{"operation_id": op.ID}}); err != nil {
		return nil, err
	}
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}, nil
}

func versionToAPI(v version) gen.ConfigVersion {
	out := gen.ConfigVersion{ID: v.ID, Number: v.Number, Source: v.Source, IsActive: v.IsActive,
		Document: object[gen.ConfigVersionDocument](v.Document), ActivatedAt: v.ActivatedAt, ActivatedBy: v.ActivatedBy}
	if v.ChangeRequestID != nil {
		out.ChangeRequestID.SetTo(*v.ChangeRequestID)
	}
	if v.RestoredFrom != nil {
		out.RestoredFrom.SetTo(*v.RestoredFrom)
	}
	return out
}

// GetConfigVersion implements GET /v1/config-versions/{id}. Versions are
// insert-only; the entity tag follows the representation, which changes
// when the version stops being the active one.
func (e *Endpoints) GetConfigVersion(ctx context.Context, params gen.GetConfigVersionParams) (gen.GetConfigVersionRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	v, err := scanVersion(tx.QueryRow(ctx, versionSelect+` WHERE v.id = $1`, params.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem.NotFound("No configuration version with this id.")
	}
	if err != nil {
		return nil, err
	}
	out := versionToAPI(v)
	return &gen.ConfigVersionHeaders{ETag: gen.NewOptString(etag.Of(&out)), Response: out}, nil
}

var versionSort = pagination.Sort{Default: "number", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "number", Column: "v.number", Type: "integer"},
}}

// ListConfigVersions implements GET /v1/config-versions.
func (e *Endpoints) ListConfigVersions(ctx context.Context, params gen.ListConfigVersionsParams) (gen.ListConfigVersionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var number *int32
	if params.Number.Set {
		number = &params.Number.Value
	}
	var active *bool
	if params.IsActive.Set {
		active = &params.IsActive.Value
	}
	scope := "listConfigVersions|" + strconv.Itoa(int(params.Number.Value)) + "|" + strconv.FormatBool(params.Number.Set) + "|" +
		strconv.FormatBool(params.IsActive.Value) + "|" + strconv.FormatBool(params.IsActive.Set)
	ks, err := e.Pages.Keyset(versionSort, scope, pageRequest(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(versionSelect+` WHERE ($1::integer IS NULL OR v.number = $1) AND ($2::boolean IS NULL OR coalesce(a.number = v.number, false) = $2)`,
		[]any{number, active})
	rows, _ := tx.Query(ctx, q, args...)
	vs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (version, error) { return scanVersion(r) })
	if err != nil {
		return nil, err
	}
	vs, next := pagination.Page(ks, vs, func(v version) (string, uuid.UUID) { return strconv.Itoa(int(v.Number)), v.ID })
	page := gen.ConfigVersionPage{Items: make([]gen.ConfigVersion, 0, len(vs))}
	for _, v := range vs {
		page.Items = append(page.Items, versionToAPI(v))
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.ConfigVersionPageHeaders{Response: page}, nil
}

// NumberRange is a row of config.number_range.
type NumberRange struct {
	ID                               uuid.UUID
	LegalEntityID                    uuid.UUID
	DocumentType                     string
	FiscalYear                       int16
	Prefix                           string
	FirstValue, LastValue, NextValue int64
	IsGapless                        bool
	Version                          int32
	CreatedAt, UpdatedAt             time.Time
	CreatedBy                        uuid.UUID
}

const rangeColumns = `id, legal_entity_id, document_type, fiscal_year, prefix, first_value, last_value, next_value, is_gapless, version,
	created_at, updated_at, created_by`

func scanRange(row pgx.Row) (NumberRange, error) {
	var n NumberRange
	err := row.Scan(&n.ID, &n.LegalEntityID, &n.DocumentType, &n.FiscalYear, &n.Prefix, &n.FirstValue, &n.LastValue, &n.NextValue, &n.IsGapless,
		&n.Version, &n.CreatedAt, &n.UpdatedAt, &n.CreatedBy)
	return n, err
}

func rangeToAPI(n NumberRange) gen.NumberRange {
	return gen.NumberRange{ID: n.ID, Version: n.Version, CreatedAt: n.CreatedAt, CreatedBy: n.CreatedBy, UpdatedAt: n.UpdatedAt,
		LegalEntityID: n.LegalEntityID, DocumentType: n.DocumentType, FiscalYear: int32(n.FiscalYear), Prefix: n.Prefix,
		FirstValue: n.FirstValue, LastValue: n.LastValue, NextValue: n.NextValue, IsGapless: n.IsGapless}
}

// GetNumberRange implements GET /v1/number-ranges/{id}; ranges of legal
// entities outside the caller's company access are not found.
func (e *Endpoints) GetNumberRange(ctx context.Context, params gen.GetNumberRangeParams) (gen.GetNumberRangeRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	n, err := scanRange(tx.QueryRow(ctx, `SELECT `+rangeColumns+` FROM config.number_range WHERE id = $1`, params.ID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(n.LegalEntityID)) {
		return nil, problem.NotFound("No number range with this id.")
	}
	if err != nil {
		return nil, err
	}
	return &gen.NumberRangeHeaders{ETag: gen.NewOptString(etag.Format(n.Version)), Response: rangeToAPI(n)}, nil
}

var rangeSort = pagination.Sort{Default: "document_type", Fields: []pagination.Field{
	{Name: "document_type", Column: "document_type", Type: "text"}, {Name: "fiscal_year", Column: "fiscal_year", Type: "smallint"},
}}

// ListNumberRanges implements GET /v1/number-ranges.
func (e *Endpoints) ListNumberRanges(ctx context.Context, params gen.ListNumberRangesParams) (gen.ListNumberRangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var le *uuid.UUID
	if params.LegalEntityID.Set {
		le = &params.LegalEntityID.Value
	}
	var year *int32
	if params.FiscalYear.Set {
		year = &params.FiscalYear.Value
	}
	scope := "listNumberRanges|" + params.LegalEntityID.Value.String() + "|" + params.DocumentType.Value + "|" + strconv.Itoa(int(params.FiscalYear.Value))
	ks, err := e.Pages.Keyset(rangeSort, scope, pageRequest(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+rangeColumns+` FROM config.number_range WHERE ($1 OR legal_entity_id = ANY($2))
		AND ($3::uuid IS NULL OR legal_entity_id = $3) AND ($4 = '' OR document_type = $4) AND ($5::integer IS NULL OR fiscal_year = $5)`,
		[]any{access.All, access.IDs, le, params.DocumentType.Value, year})
	rows, _ := tx.Query(ctx, q, args...)
	ranges, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (NumberRange, error) { return scanRange(r) })
	if err != nil {
		return nil, err
	}
	byYear := ks.Field() == "fiscal_year"
	ranges, next := pagination.Page(ks, ranges, func(n NumberRange) (string, uuid.UUID) {
		if byYear {
			return strconv.Itoa(int(n.FiscalYear)), n.ID
		}
		return n.DocumentType, n.ID
	})
	page := gen.NumberRangePage{Items: make([]gen.NumberRange, 0, len(ranges))}
	for _, n := range ranges {
		page.Items = append(page.Items, rangeToAPI(n))
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.NumberRangePageHeaders{Response: page}, nil
}
