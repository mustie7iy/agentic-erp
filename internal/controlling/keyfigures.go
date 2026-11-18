package controlling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
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

// Statistical key figures (ADR-0064 As built T60j): a company's quantity
// such as employees or floor space, in a unit of measure of the reference
// data, whose values are posted per cost center and fiscal period. A
// posting adds to what was posted before (SAP's totals values, from
// memory): a center's value of a period is the sum of its postings'
// values, exact in numeric, and a correction is a further posting with a
// negative value. Postings are insert-only. A cycle whose share basis is a
// key figure splits a run's costs by its receivers' values of the period
// run (keyFigureWeights in allocationrun.go). Values are read as numeric
// text and handled as big.Rat, never as a float.

func init() {
	// A code is checked by its constraint alone.
	problem.RegisterConstraint("statistical_key_figure_code_once", func() *problem.Problem {
		return problem.Conflict("controlling.statistical_key_figure_exists", "The legal entity already has a statistical key figure with this code.")
	})
}

// keyFigure is a row of controlling.statistical_key_figure.
type keyFigure struct {
	ID, LegalEntityID uuid.UUID
	Code, Name, Unit  string
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const keyFigureColumns = `k.id, k.legal_entity_id, k.code, k.name, k.unit, k.version, k.created_at, k.created_by, k.updated_at, k.updated_by`

func scanKeyFigure(r pgx.Row) (keyFigure, error) {
	var k keyFigure
	err := r.Scan(&k.ID, &k.LegalEntityID, &k.Code, &k.Name, &k.Unit, &k.Version, &k.CreatedAt, &k.CreatedBy, &k.UpdatedAt, &k.UpdatedBy)
	return k, errs.Wrapf(err, "controlling: scan statistical key figure")
}

func keyFigureToAPI(k keyFigure) gen.StatisticalKeyFigure {
	return gen.StatisticalKeyFigure{ID: k.ID, Version: k.Version, CreatedAt: k.CreatedAt, CreatedBy: k.CreatedBy, UpdatedAt: k.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(k.UpdatedBy), LegalEntityID: k.LegalEntityID, Code: k.Code, Name: k.Name, Unit: k.Unit}
}

func keyFigureNotFound() error { return problem.NotFoundOf("statistical key figure") }

// keyFigureInCompany reads a key figure and its company. One that does not
// exist, or of a company the caller may not act in, is answered as missing
// (04 §4.9).
func (e *Endpoints) keyFigureInCompany(ctx context.Context, tx pgx.Tx, id uuid.UUID) (keyFigure, tenancy.Entity, error) {
	k, err := scanKeyFigure(tx.QueryRow(ctx, `SELECT `+keyFigureColumns+` FROM controlling.statistical_key_figure k WHERE k.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return keyFigure{}, tenancy.Entity{}, keyFigureNotFound()
	}
	if err != nil {
		return keyFigure{}, tenancy.Entity{}, fmt.Errorf("controlling: read statistical key figure %s: %w", id, err)
	}
	le, _, ok, err := e.company(ctx, tx, k.LegalEntityID)
	if err != nil {
		return keyFigure{}, tenancy.Entity{}, err
	}
	if !ok {
		return keyFigure{}, tenancy.Entity{}, keyFigureNotFound()
	}
	return k, le, nil
}

// CreateStatisticalKeyFigure implements POST /v1/statistical-key-figures.
func (e *Endpoints) CreateStatisticalKeyFigure(ctx context.Context, req *gen.StatisticalKeyFigureCreate,
	_ gen.CreateStatisticalKeyFigureParams) (gen.CreateStatisticalKeyFigureRes, error) {
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
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ref.unit_of_measure WHERE code = $1)`, req.Unit).Scan(&known); err != nil {
		return nil, fmt.Errorf("controlling: unit of measure %s: %w", req.Unit, err)
	}
	if !known {
		return nil, problem.Unprocessable("controlling.statistical_key_figure_invalid", "The statistical key figure cannot be defined as given.",
			problem.FieldError{Field: "unit", Code: "unknown", Message: "a unit of measure of the reference data"})
	}
	k, err := scanKeyFigure(tx.QueryRow(ctx, `INSERT INTO controlling.statistical_key_figure AS k (tenant_id, id, legal_entity_id, code, name, unit, created_by,
			updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING `+keyFigureColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), le.ID, req.Code, req.Name, req.Unit, p.ID))
	if err != nil {
		return nil, fmt.Errorf("controlling: insert statistical key figure %s of %s: %w", req.Code, le.ID, err)
	}
	detail := map[string]any{"legal_entity_id": k.LegalEntityID, "code": k.Code, "name": k.Name, "unit": k.Unit}
	payload := map[string]any{"statistical_key_figure_id": k.ID, "legal_entity_id": k.LegalEntityID, "code": k.Code, "name": k.Name, "unit": k.Unit,
		"change": "created"}
	if err := audit.Record(ctx, "controlling.statistical_key_figure.create", "controlling.statistical_key_figure", k.ID, detail,
		outbox.Event{Context: "controlling", Name: "statistical_key_figure_changed", AggregateType: "controlling.statistical_key_figure", AggregateID: k.ID,
			Payload: payload}); err != nil {
		return nil, err
	}
	return &gen.StatisticalKeyFigureHeaders{ETag: gen.NewOptString(etag.Format(k.Version)),
		Location: gen.NewOptString("/v1/statistical-key-figures/" + k.ID.String()), Response: keyFigureToAPI(k)}, nil
}

// GetStatisticalKeyFigure implements GET /v1/statistical-key-figures/{id}.
func (e *Endpoints) GetStatisticalKeyFigure(ctx context.Context, params gen.GetStatisticalKeyFigureParams) (gen.GetStatisticalKeyFigureRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	k, _, err := e.keyFigureInCompany(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetStatisticalKeyFigureOKHeaders{ETag: gen.NewOptString(etag.Format(k.Version)), Response: keyFigureToAPI(k)}, nil
}

var keyFigureSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "k.code", Type: "text"}}}

// ListStatisticalKeyFigures implements GET /v1/statistical-key-figures.
func (e *Endpoints) ListStatisticalKeyFigures(ctx context.Context, params gen.ListStatisticalKeyFiguresParams) (gen.ListStatisticalKeyFiguresRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	scope := fmt.Sprintf("listStatisticalKeyFigures|%v|%s", le, params.Code.Value)
	ks, err := e.Pages.Keyset(keyFigureSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+keyFigureColumns+` FROM controlling.statistical_key_figure k WHERE ($1 OR k.legal_entity_id = ANY($2))
		AND ($3::uuid IS NULL OR k.legal_entity_id = $3) AND ($4 = '' OR k.code = $4)`, []any{access.All, access.IDs, le, params.Code.Value})
	rows, _ := tx.Query(ctx, q, args...)
	figures, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (keyFigure, error) { return scanKeyFigure(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list statistical key figures: %w", err)
	}
	items, next := apitypes.PageOf(ks, figures, func(k keyFigure) (string, uuid.UUID) { return k.Code, k.ID }, keyFigureToAPI)
	return &gen.StatisticalKeyFigurePage{Items: items, NextPageToken: next}, nil
}

// keyFigureValue is a cost center's value of a key figure, as numeric
// text without trailing zeros; Code is named by the ledger's port.
type keyFigureValue struct {
	CostCenterID uuid.UUID
	Code         string
	Value        string
}

func valueToAPI(v keyFigureValue) gen.StatisticalKeyFigureValue {
	return gen.StatisticalKeyFigureValue{CostCenterID: v.CostCenterID, CostCenterCode: v.Code, Value: v.Value}
}

// postingLines checks a posting's lines: each an active cost center of
// the key figure's company, once, with a value that is not zero (the
// contract's pattern holds its form). It returns the lines with their
// centers' codes, or 422 naming every line that cannot stand.
func (e *Endpoints) postingLines(ctx context.Context, le uuid.UUID, lines []gen.StatisticalKeyFigurePostingCreateLinesItem) ([]keyFigureValue, error) {
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.CostCenterID
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("controlling: cost centers of a key figure posting: %w", err)
	}
	var bad []problem.FieldError
	out := make([]keyFigureValue, len(lines))
	seen := map[uuid.UUID]bool{}
	for i, l := range lines {
		reason, message := centerState(refs, l.CostCenterID, le), "an active cost center of the legal entity"
		if seen[l.CostCenterID] {
			reason, message = "duplicate", "a cost center named once"
		}
		seen[l.CostCenterID] = true
		if reason != "" {
			bad = append(bad, problem.FieldError{Field: fmt.Sprintf("lines[%d].cost_center_id", i), Code: reason, Message: message})
		}
		if v, err := money.ParseDecimalRat(l.Value); err != nil || v.Sign() == 0 {
			bad = append(bad, problem.FieldError{Field: fmt.Sprintf("lines[%d].value", i), Code: "zero", Message: "a value other than zero"})
		}
		out[i] = keyFigureValue{CostCenterID: l.CostCenterID, Code: refs[l.CostCenterID].Code, Value: l.Value}
	}
	if len(bad) > 0 {
		return nil, problem.Unprocessable("controlling.statistical_key_figure_posting_invalid", "The values cannot be posted as given.", bad...)
	}
	return out, nil
}

// CreateStatisticalKeyFigurePosting implements POST
// /v1/statistical-key-figures/{id}/postings. A posting only adds rows, so
// it takes no lock: postings of one key figure at once add up whatever
// their order.
func (e *Endpoints) CreateStatisticalKeyFigurePosting(ctx context.Context, req *gen.StatisticalKeyFigurePostingCreate,
	params gen.CreateStatisticalKeyFigurePostingParams) (gen.CreateStatisticalKeyFigurePostingRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	k, le, err := e.keyFigureInCompany(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	if _, err := e.regularPeriod(ctx, le.ID, req.FiscalYear, req.FiscalPeriod); err != nil {
		return nil, err
	}
	lines, err := e.postingLines(ctx, le.ID, req.Lines)
	if err != nil {
		return nil, err
	}
	out := gen.StatisticalKeyFigurePosting{ID: uuid.Must(uuid.NewV7()), StatisticalKeyFigureID: k.ID, StatisticalKeyFigureCode: k.Code, Unit: k.Unit,
		LegalEntityID: le.ID, FiscalYear: req.FiscalYear, FiscalPeriod: req.FiscalPeriod, CreatedBy: p.ID, Lines: make([]gen.StatisticalKeyFigureValue, len(lines))}
	if err := tx.QueryRow(ctx, `INSERT INTO controlling.statistical_key_figure_posting (tenant_id, id, key_figure_id, legal_entity_id, fiscal_year,
			fiscal_period, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, p.TenantID, out.ID, k.ID, le.ID, req.FiscalYear,
		req.FiscalPeriod, p.ID).Scan(&out.CreatedAt); err != nil {
		return nil, fmt.Errorf("controlling: keep a posting of statistical key figure %s: %w", k.ID, err)
	}
	centers, values := make([]uuid.UUID, len(lines)), make([]string, len(lines))
	for i, l := range lines {
		centers[i], values[i] = l.CostCenterID, l.Value
	}
	// Each value comes back as the column keeps it, without trailing zeros.
	rows, _ := tx.Query(ctx, `INSERT INTO controlling.statistical_key_figure_value (tenant_id, posting_id, position, cost_center_id, value)
		SELECT $1, $2, v.n, v.center, v.value::numeric FROM unnest($3::uuid[], $4::text[]) WITH ORDINALITY AS v(center, value, n)
		RETURNING position, trim_scale(value)::text`, p.TenantID, out.ID, centers, values)
	var position int
	var value string
	if _, err := pgx.ForEachRow(rows, []any{&position, &value}, func() error {
		lines[position-1].Value = value
		return nil
	}); err != nil {
		return nil, fmt.Errorf("controlling: keep the values of posting %s: %w", out.ID, err)
	}
	for i, l := range lines {
		out.Lines[i] = valueToAPI(l)
	}
	if err := recordPosting(ctx, k, out, lines); err != nil {
		return nil, err
	}
	return &gen.StatisticalKeyFigurePostingHeaders{Response: out}, nil
}

// recordPosting audits a posting and publishes it, its lines in their
// order.
func recordPosting(ctx context.Context, k keyFigure, out gen.StatisticalKeyFigurePosting, lines []keyFigureValue) error {
	shown := make([]map[string]any, len(lines))
	for i, l := range lines {
		shown[i] = map[string]any{"cost_center_id": l.CostCenterID, "cost_center_code": l.Code, "value": l.Value}
	}
	detail := map[string]any{"posting_id": out.ID, "legal_entity_id": k.LegalEntityID, "code": k.Code, "unit": k.Unit, "fiscal_year": out.FiscalYear,
		"fiscal_period": out.FiscalPeriod, "lines": shown}
	payload := map[string]any{"posting_id": out.ID, "statistical_key_figure_id": k.ID, "legal_entity_id": k.LegalEntityID, "code": k.Code, "unit": k.Unit,
		"fiscal_year": out.FiscalYear, "fiscal_period": out.FiscalPeriod, "lines": shown}
	return audit.Record(ctx, "controlling.statistical_key_figure.post", "controlling.statistical_key_figure", k.ID, detail,
		outbox.Event{Context: "controlling", Name: "statistical_key_figure_posted", AggregateType: "controlling.statistical_key_figure", AggregateID: k.ID,
			Payload: payload})
}

// periodValues is the relation of a key figure's values in a period, one
// row per cost center with a posting in it: id is the center, value the
// sum of its postings' values. $1 is the key figure, $2 and $3 the year
// and period, $4 a center or NULL for every one.
const periodValues = `SELECT v.cost_center_id AS id, sum(v.value) AS value
	FROM controlling.statistical_key_figure_value v
	JOIN controlling.statistical_key_figure_posting p ON p.tenant_id = v.tenant_id AND p.id = v.posting_id
	WHERE p.key_figure_id = $1 AND p.fiscal_year = $2 AND p.fiscal_period = $3 AND ($4::uuid IS NULL OR v.cost_center_id = $4)
	GROUP BY v.cost_center_id`

var valueSort = pagination.Sort{Default: "cost_center_id", Fields: []pagination.Field{{Name: "cost_center_id", Column: "id", Type: "uuid"}}}

// ListStatisticalKeyFigureValues implements GET
// /v1/statistical-key-figures/{id}/values: a page of the centers' values
// of the period, in the order of their ids, and the total of every page.
func (e *Endpoints) ListStatisticalKeyFigureValues(ctx context.Context, params gen.ListStatisticalKeyFigureValuesParams) (gen.ListStatisticalKeyFigureValuesRes,
	error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	center := apitypes.Ptr[uuid.UUID](params.CostCenterID)
	scope := fmt.Sprintf("listStatisticalKeyFigureValues|%s|%d|%d|%v", params.ID, params.FiscalYear, params.FiscalPeriod, center)
	ks, err := e.Pages.Keyset(valueSort, scope, apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	k, le, err := e.keyFigureInCompany(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	args := []any{k.ID, params.FiscalYear, params.FiscalPeriod, center}
	var total string
	if err := tx.QueryRow(ctx, `SELECT coalesce(trim_scale(sum(value)), 0)::text FROM (`+periodValues+`) t`, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("controlling: total of statistical key figure %s in %d/%d: %w", k.ID, params.FiscalYear, params.FiscalPeriod, err)
	}
	q, qargs := ks.SQL(`SELECT id, trim_scale(value)::text FROM (`+periodValues+`) t WHERE true`, args)
	rows, _ := tx.Query(ctx, q, qargs...)
	values, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (keyFigureValue, error) {
		var v keyFigureValue
		return v, r.Scan(&v.CostCenterID, &v.Value)
	})
	if err != nil {
		return nil, fmt.Errorf("controlling: values of statistical key figure %s in %d/%d: %w", k.ID, params.FiscalYear, params.FiscalPeriod, err)
	}
	items, next, err := apitypes.PageWith(ks, values, func(v keyFigureValue) (string, uuid.UUID) { return v.CostCenterID.String(), v.CostCenterID },
		func(page []keyFigureValue) (map[uuid.UUID]ledger.CostCenterRef, error) {
			ids := make([]uuid.UUID, len(page))
			for i, v := range page {
				ids[i] = v.CostCenterID
			}
			return e.Ledger.CostCenterRefs(ctx, ids)
		},
		func(v keyFigureValue, refs map[uuid.UUID]ledger.CostCenterRef) gen.StatisticalKeyFigureValue {
			v.Code = refs[v.CostCenterID].Code
			return valueToAPI(v)
		})
	if err != nil {
		return nil, fmt.Errorf("controlling: cost centers of statistical key figure %s's values: %w", k.ID, err)
	}
	return &gen.StatisticalKeyFigureValuePage{StatisticalKeyFigureID: k.ID, StatisticalKeyFigureCode: k.Code, Unit: k.Unit, LegalEntityID: le.ID,
		FiscalYear: params.FiscalYear, FiscalPeriod: params.FiscalPeriod, Items: items, Total: total, NextPageToken: next}, nil
}

// receiverValues are the key figure's values of the period for the
// centers, as numeric text without trailing zeros; a center without a
// posting in it has none.
func receiverValues(ctx context.Context, tx pgx.Tx, figure uuid.UUID, period ledger.FiscalPeriod, centers []uuid.UUID) (map[uuid.UUID]string, error) {
	rows, _ := tx.Query(ctx, `SELECT id, trim_scale(value)::text FROM (`+periodValues+`) t WHERE id = ANY($5)`, figure, period.FiscalYear, period.Number,
		nil, centers)
	out := map[uuid.UUID]string{}
	var center uuid.UUID
	var value string
	if _, err := pgx.ForEachRow(rows, []any{&center, &value}, func() error {
		out[center] = value
		return nil
	}); err != nil {
		return nil, fmt.Errorf("controlling: values of statistical key figure %s in %d/%d: %w", figure, period.FiscalYear, period.Number, err)
	}
	return out, nil
}
