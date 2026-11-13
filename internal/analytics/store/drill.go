package store

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Drilled is a page of the documents behind a row and the facts of the
// row's figure, both from one source (T94b, ADR-0044 As built T94b).
type Drilled struct {
	// Facts answer the row query's figure (analytics.Execute); their
	// Exponents also hold the documents' currencies.
	Facts     *analytics.Facts
	Documents []analytics.Document
	More      bool // documents follow the page
}

// Documents is the page of the documents behind the row query q's one
// base metric (analytics.RowQuery) after the cursor, at most limit of
// them, with the facts of its figure: from the store when it may answer,
// as Facts decides, else both from the database.
func (s *Source) Documents(ctx context.Context, m *analytics.Model, p analytics.Principal, q analytics.Query, after *analytics.DocumentCursor,
	limit int, notBefore time.Time,
) (*Drilled, error) {
	snap, why, err := s.fresh(ctx, notBefore)
	if err != nil {
		return nil, err
	}
	if why == "" {
		d, failed := s.documentsFromStore(ctx, m, p, q, after, limit, snap)
		if failed == nil {
			return d, nil
		}
		why = failed.Error()
	}
	s.log().InfoContext(ctx, "analytics documents answered from the database", "reason", why)
	f, err := s.Database.Facts(ctx, m, s.now())
	if err != nil {
		return nil, fmt.Errorf("analytics: database facts: %w", err)
	}
	docs, err := analytics.DocumentsOf(m, f, p, q)
	if err != nil {
		return nil, err
	}
	page, more := analytics.PageOf(docs, after, limit)
	return &Drilled{Facts: f, Documents: page, More: more}, nil
}

// documentsFromStore asks the reader for the figure's groups and for one
// more document than the page holds, to know whether more follow.
func (s *Source) documentsFromStore(ctx context.Context, m *analytics.Model, p analytics.Principal, q analytics.Query, after *analytics.DocumentCursor,
	limit int, snap Snapshot,
) (*Drilled, error) {
	f, err := s.fromStore(ctx, m, p, q, snap)
	if err != nil {
		return nil, err
	}
	plan := DocumentPlan(m, q, cmp.Or(q.Ledger, snap.Leading), after, limit+1)
	scope := wire.Scope{AllCompanies: p.Companies == nil, Companies: p.Companies, Datasets: s.Datasets}
	res, err := s.Reader.Query(ctx, wire.Request{TenantID: snap.TenantID, ManifestKey: snap.ManifestKey, Scope: scope, Plan: plan})
	if err != nil {
		return nil, err
	}
	docs, err := documents(m.Metric(q.Measures[0]), plan, res, f.Exponents)
	if err != nil {
		return nil, err
	}
	if len(docs) > limit {
		return &Drilled{Facts: f, Documents: docs[:limit], More: true}, nil
	}
	return &Drilled{Facts: f, Documents: docs}, nil
}

// DocumentPlan is the reader plan of the documents behind the row query
// q's one base metric: its dataset, summed per document and currency and
// broken down by what tells a document's company and kind, with the
// metric's filter and the query's, after the cursor, at most limit rows.
func DocumentPlan(m *analytics.Model, q analytics.Query, ledger string, after *analytics.DocumentCursor, limit int) wire.Plan {
	met := m.Metric(q.Measures[0])
	p := wire.Plan{Dataset: met.Source, Aggregate: "documents", By: analytics.DocumentDimensions(met.Source), Ledger: ledger,
		FiscalYearStartMonth: m.Calendar.FiscalYearStartMonth, Limit: limit}
	for _, d := range slices.Sorted(maps.Keys(met.Filter)) {
		p.Where = append(p.Where, wire.Filter{Column: d, Op: "in", Values: met.Filter[d]})
	}
	for _, f := range q.Where {
		p.Where = append(p.Where, wire.Filter{Column: f.Dim, Op: f.Op, Values: f.Values})
	}
	if after != nil {
		p.After = []string{after.ID, after.Currency}
	}
	return p
}

// documents turns a documents plan's rows into documents, each signed as
// the metric adds it. A row that does not fit the plan, or a currency with
// two exponents, is refused, as groups refuses it.
func documents(met *analytics.Metric, p wire.Plan, res *wire.Result, exponents map[string]int) ([]analytics.Document, error) {
	out := make([]analytics.Document, 0, len(res.Rows))
	for _, r := range res.Rows {
		id, err := uuid.Parse(r.Document)
		if len(r.By) != len(p.By) || err != nil || r.Currency == "" {
			return nil, fmt.Errorf("analytics: erp-analytics answered a document row %+v for %d dimensions", r, len(p.By))
		}
		if e, seen := exponents[r.Currency]; seen && e != r.Exponent {
			return nil, fmt.Errorf("analytics: erp-analytics answered %s with exponents %d and %d", r.Currency, e, r.Exponent)
		}
		exponents[r.Currency] = r.Exponent
		kind := ""
		if len(r.By) > 1 {
			kind = r.By[1]
		}
		out = append(out, analytics.Document{Kind: analytics.DocumentKind(p.Dataset, kind), ID: id, Company: r.By[0], Currency: r.Currency,
			Exponent: r.Exponent, Amount: r.Amount * money.Minor(met.Sign)})
	}
	return out, nil
}
