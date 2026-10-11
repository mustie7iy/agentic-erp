package configuration

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func suiteToAPI(s RegressionSuite) gen.RegressionSuite {
	out := gen.RegressionSuite{ID: s.ID, Version: s.Version, CreatedAt: s.CreatedAt, CreatedBy: s.CreatedBy, UpdatedAt: s.UpdatedAt, UpdatedBy: gen.NewOptUUID(s.UpdatedBy),
		Number: s.Number, Name: s.Name, ConfigVersion: s.ConfigVersion, WindowDays: s.WindowDays, CaseCount: s.CaseCount, Strata: s.Strata,
		Cases: make([]gen.RegressionCase, 0, len(s.Cases))}
	// Strata and issues are stored as JSON arrays, so they scan as slices, never nil.
	for _, c := range s.Cases {
		out.Cases = append(out.Cases, gen.RegressionCase{CaseNo: c.CaseNo, SupplierInvoiceID: c.InvoiceID, DocumentNumber: c.DocumentNumber, Company: c.Company,
			Why: c.Why, ExpectedDecision: c.ExpectedDecision, ExpectedIssues: c.ExpectedIssues})
	}
	return out
}

// CreateRegressionSuite implements POST /v1/regression-suites.
func (e *Endpoints) CreateRegressionSuite(ctx context.Context, req *gen.RegressionSuiteCreate, _ gen.CreateRegressionSuiteParams) (gen.CreateRegressionSuiteRes, error) {
	s, err := CaptureRegressionSuite(ctx, e.Dependencies, NewRegressionSuite{Name: req.Name, MaxCases: int(req.MaxCases.Value), WindowDays: int(req.WindowDays.Value)})
	if err != nil {
		return nil, err
	}
	return &gen.RegressionSuiteHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Location: gen.NewOptString("/v1/regression-suites/" + s.ID.String()),
		Response: suiteToAPI(s)}, nil
}

// GetRegressionSuite implements GET /v1/regression-suites/{id}.
func (e *Endpoints) GetRegressionSuite(ctx context.Context, params gen.GetRegressionSuiteParams) (gen.GetRegressionSuiteRes, error) {
	s, err := GetRegressionSuite(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetRegressionSuiteOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: suiteToAPI(s)}, nil
}

var suiteSort = pagination.Sort{Default: "captured_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "captured_at", Column: "s.captured_at", Type: "timestamptz"},
}}

// ListRegressionSuites implements GET /v1/regression-suites.
func (e *Endpoints) ListRegressionSuites(ctx context.Context, params gen.ListRegressionSuitesParams) (gen.ListRegressionSuitesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	version := params.ConfigVersion.Value
	ks, err := e.Pages.Keyset(suiteSort, "listRegressionSuites|"+etag.Format(version), pageRequest(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(suiteSelect+` WHERE ($1 = 0 OR s.config_version = $1)`, []any{version})
	rows, _ := tx.Query(ctx, q, args...)
	suites, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RegressionSuite, error) { return scanSuite(r) })
	if err != nil {
		return nil, err
	}
	suites, next := pagination.Page(ks, suites, func(s RegressionSuite) (string, uuid.UUID) { return s.CapturedAt.UTC().Format(time.RFC3339Nano), s.ID })
	page := gen.RegressionSuitePage{Items: make([]gen.RegressionSuite, 0, len(suites))}
	for _, s := range suites {
		page.Items = append(page.Items, suiteToAPI(s))
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.RegressionSuitePageHeaders{Response: page}, nil
}
