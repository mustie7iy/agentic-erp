package endpoint

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// drill is a checked drill-through request: the row's query, the scope its
// page tokens are bound to and the page asked for.
type drill struct {
	model   *analytics.Model
	version int32
	row     analytics.Query
	scope   string
	after   *analytics.DocumentCursor
	size    int
}

// DrillAnalyticsQuery implements gen.Handler: the documents behind one
// measure of one row of a query's result (T94b, ADR-0044 As built T94b),
// checked and scoped as RunAnalyticsQuery checks the query, each
// document's number read as the caller from its own read.
func (e *Endpoints) DrillAnalyticsQuery(ctx context.Context, req *gen.AnalyticsDrill, params gen.DrillAnalyticsQueryParams) (gen.DrillAnalyticsQueryRes, error) {
	d, err := e.checkDrill(ctx, req, params)
	if err != nil {
		return nil, err
	}
	p, err := e.principal(ctx, req.Query.Companies)
	if err != nil {
		return nil, err
	}
	datasets, err := e.datasets(ctx, d.model, d.row)
	if err != nil {
		return nil, err
	}
	release, err := e.slot()
	if err != nil {
		return nil, err
	}
	defer release()
	db := e.database(datasets)
	src := store.Source{Reader: e.Reader, Database: db, Datasets: datasets, Requires: db.Permissions(), Now: e.Now, Log: e.Log}
	drilled, err := src.Documents(ctx, d.model, p, d.row, d.after, d.size, req.Query.AsOf.Or(time.Time{}))
	if err != nil {
		return nil, apiError(err)
	}
	res, err := analytics.Execute(d.model, drilled.Facts, p, d.row)
	if err != nil {
		return nil, invalid(err)
	}
	if err := db.Numbers(ctx, drilled.Documents); err != nil {
		return nil, apiError(err)
	}
	return e.documentsPage(d, res, drilled), nil
}

// checkDrill reads the model the query names, checks the drill-through
// against it and reads the page asked for.
func (e *Endpoints) checkDrill(ctx context.Context, req *gen.AnalyticsDrill, params gen.DrillAnalyticsQueryParams) (drill, error) {
	m, version, err := e.model(ctx, req.Query.ConfigVersion.Or(0))
	if err != nil {
		return drill{}, err
	}
	row, err := analytics.RowQuery(m, analytics.Drill{Query: fromAPI(&req.Query), Measure: req.Measure.Or(""), Row: req.Row})
	if err != nil {
		return drill{}, invalid(err)
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return drill{}, err
	}
	// A query, company codes, a time and a version are plain data: they
	// always encode.
	b, _ := json.Marshal(struct {
		Row       analytics.Query
		Companies []string
		AsOf      time.Time
		Version   int32
	}{row, req.Query.Companies, req.Query.AsOf.Or(time.Time{}), version})
	sum := sha256.Sum256(b)
	d := drill{model: m, version: version, row: row, scope: fmt.Sprintf("analytics-documents:%x", sum), size: size}
	if token, ok := params.PageToken.Get(); ok {
		d.after = &analytics.DocumentCursor{}
		if err := e.Pages.Decode(d.scope, token, d.after); err != nil {
			return drill{}, err
		}
	}
	return d, nil
}

// documentsPage is the answer: the row's figure, the page's documents and
// the next page's token when more follow.
func (e *Endpoints) documentsPage(d drill, res *analytics.Result, drilled *store.Drilled) gen.DrillAnalyticsQueryRes {
	out := gen.AnalyticsDocuments{Measure: d.row.Measures[0], Total: gen.AnalyticsCell{Value: gen.NilString{Null: true}}, Source: drilled.Facts.Source,
		AsOf: drilled.Facts.AsOf, ConfigVersion: d.version, Documents: make([]gen.AnalyticsDocument, 0, len(drilled.Documents))}
	if len(res.Rows) == 1 {
		out.Total = toCell(res.Rows[0][0])
	}
	for _, doc := range drilled.Documents {
		out.Documents = append(out.Documents, gen.AnalyticsDocument{Kind: doc.Kind, ID: doc.ID, Number: doc.Number, Company: doc.Company, Path: doc.Path(),
			Amount: gen.Money{Amount: analytics.Value{Units: doc.Amount, Scale: doc.Exponent}.String(), Currency: doc.Currency}})
	}
	if drilled.More {
		last := drilled.Documents[len(drilled.Documents)-1]
		// A cursor of two strings always encodes.
		token, _ := e.Pages.Encode(d.scope, analytics.DocumentCursor{ID: last.ID.String(), Currency: last.Currency})
		out.NextPageToken = gen.NewOptString(token)
	}
	return &gen.AnalyticsDocumentsHeaders{Response: out}
}
