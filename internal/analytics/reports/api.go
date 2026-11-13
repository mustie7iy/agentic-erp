package reports

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
)

// The contract's specification and queries carry the field names the
// analytics package writes them with (ReportSpec is Spec, ReportQuery a
// Query without scope), so they convert through JSON. The generated
// types and analytics' plain data always encode, and what one encodes
// the other reads.

func specFromAPI(s gen.ReportSpec) analytics.Spec {
	b, _ := s.MarshalJSON()
	var out analytics.Spec
	_ = json.Unmarshal(b, &out)
	return out
}

func queriesFromAPI(in map[string]gen.ReportQuery) map[string]analytics.Query {
	out := make(map[string]analytics.Query, len(in))
	for id, q := range in {
		b, _ := q.MarshalJSON()
		var aq analytics.Query
		_ = json.Unmarshal(b, &aq)
		out[id] = aq
	}
	return out
}

func specToAPI(s analytics.Spec) gen.ReportSpec {
	b, _ := json.Marshal(s)
	var out gen.ReportSpec
	_ = out.UnmarshalJSON(b)
	return out
}

func queriesToAPI(in map[string]analytics.Query) gen.ReportQueries {
	out := make(gen.ReportQueries, len(in))
	for id, q := range in {
		b, _ := json.Marshal(q)
		var gq gen.ReportQuery
		_ = gq.UnmarshalJSON(b)
		out[id] = gq
	}
	return out
}

// names reads the names of the reports' owners in one call.
func (e *Endpoints) names(ctx context.Context, reports []Report) (map[uuid.UUID]string, error) {
	ids := make([]uuid.UUID, len(reports))
	for i, r := range reports {
		ids[i] = r.OwnerID
	}
	names, err := e.People.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("analytics: names of %d report owners: %w", len(ids), err)
	}
	return names, nil
}

// toAPI is a report as the API gives it, with its owner's name.
func toAPI(r Report, names map[uuid.UUID]string) gen.Report {
	out := gen.Report{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.OwnerID, UpdatedAt: r.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(r.OwnerID), Title: r.Title, Spec: specToAPI(r.Spec), Queries: queriesToAPI(r.Queries),
		OwnerID: r.OwnerID, OwnerName: names[r.OwnerID], Status: r.Status}
	if r.Question != "" {
		out.Question = gen.NewOptString(r.Question)
	}
	return out
}

// one is a single report as the API gives it, with its ETag.
func (e *Endpoints) one(ctx context.Context, r Report, err error) (gen.Report, gen.OptString, error) {
	if err != nil {
		return gen.Report{}, gen.OptString{}, err
	}
	names, err := e.names(ctx, []Report{r})
	if err != nil {
		return gen.Report{}, gen.OptString{}, err
	}
	return toAPI(r, names), gen.NewOptString(etag.Format(r.Version)), nil
}

// CreateReport implements POST /v1/reports.
func (e *Endpoints) CreateReport(ctx context.Context, req *gen.ReportCreate, _ gen.CreateReportParams) (gen.CreateReportRes, error) {
	r, err := e.Save(ctx, specFromAPI(req.Spec), queriesFromAPI(req.Queries))
	out, tag, err := e.one(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.ReportHeaders{ETag: tag, Location: gen.NewOptString("/v1/reports/" + r.ID.String()), Response: out}, nil
}

// GetReport implements GET /v1/reports/{id}.
func (e *Endpoints) GetReport(ctx context.Context, params gen.GetReportParams) (gen.GetReportRes, error) {
	r, err := Get(ctx, params.ID)
	out, tag, err := e.one(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetReportOKHeaders{ETag: tag, Response: out}, nil
}

// RemoveReport implements POST /v1/reports/{id}:remove.
func (e *Endpoints) RemoveReport(ctx context.Context, params gen.RemoveReportParams) (gen.RemoveReportRes, error) {
	r, err := Remove(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.one(ctx, r, err)
	if err != nil {
		return nil, err
	}
	return &gen.RemoveReportOKHeaders{ETag: tag, Response: out}, nil
}

// ListReports implements GET /v1/reports.
func (e *Endpoints) ListReports(ctx context.Context, params gen.ListReportsParams) (gen.ListReportsRes, error) {
	reports, ks, err := e.List(ctx, params.Status.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	items, next, err := apitypes.PageWith(ks, reports, func(r Report) (string, uuid.UUID) { return r.Title, r.ID },
		func(page []Report) (map[uuid.UUID]string, error) { return e.names(ctx, page) }, toAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ReportPage{Items: items, NextPageToken: next}, nil
}

// RunReport implements POST /v1/reports/{id}:run.
func (e *Endpoints) RunReport(ctx context.Context, params gen.RunReportParams) (gen.RunReportRes, error) {
	res, err := e.Run(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out := gen.ReportResults{ReportID: res.Report.ID, Title: res.Report.Title, Spec: specToAPI(res.Spec), DataThrough: res.DataThrough,
		Results: make([]gen.AnalyticsResult, len(res.Results))}
	for i, r := range res.Results {
		out.Results[i] = endpoint.ResultToAPI(r)
	}
	return &gen.ReportResultsHeaders{ETag: gen.NewOptString(etag.Format(res.Report.Version)), Response: out}, nil
}
