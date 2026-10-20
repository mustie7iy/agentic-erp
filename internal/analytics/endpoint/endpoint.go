// Package endpoint serves the semantic layer in erp-server
// (runAnalyticsQuery, ADR-0044): a query names governed metrics,
// dimensions and filters, never SQL, and is answered from the analytical
// store when the tenant's store is current (store.Source), else from the
// database, with the answer's lineage. The database path reads the API
// itself as the caller (analytics.Source over an in-process transport), so
// a person or agent sees in analytics exactly what the API lets them read.
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Companies names legal entities by id: their company codes, which the
// semantic layer's company dimension holds (tenancy.Directory).
type Companies interface {
	CompanyCode(ctx context.Context, id uuid.UUID) (code string, ok bool, err error)
}

// Endpoints is the analytics context's part of the generated Handler.
type Endpoints struct {
	// Configs reads the configuration versions whose analytics sections
	// are the governed semantic model (ADR-0044, T55g).
	Configs   Configs
	Companies Companies
	// Reader asks erp-analytics for a plan's groups; nil: no reader is
	// configured and every query is answered from the database.
	Reader store.Querier
	// Loopback reads the API in-process as the caller
	// (httpapi.Loopback): the database path.
	Loopback http.RoundTripper
	// Fallbacks bounds the database answers running at once: each holds
	// its request's transaction while it reads the API in transactions of
	// its own. Beyond it a query is 503 analytics.busy. Default 2.
	Fallbacks int
	Log       *slog.Logger
	Now       func() time.Time

	slots chan struct{}
}

// RunAnalyticsQuery implements gen.Handler.
func (e *Endpoints) RunAnalyticsQuery(ctx context.Context, req *gen.AnalyticsQuery, _ gen.RunAnalyticsQueryParams) (gen.RunAnalyticsQueryRes, error) {
	m, version, err := e.model(ctx, req.ConfigVersion.Or(0))
	if err != nil {
		return nil, err
	}
	q := fromAPI(req)
	if err := analytics.Validate(m, q); err != nil {
		return nil, invalid(err)
	}
	p, err := e.principal(ctx, req.Companies)
	if err != nil {
		return nil, err
	}
	datasets, err := e.datasets(ctx, m, q)
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
	facts, err := src.Facts(ctx, m, p, q, req.AsOf.Or(time.Time{}))
	if err != nil {
		return nil, apiError(err)
	}
	res, err := analytics.Execute(m, facts, p, q)
	if err != nil {
		return nil, invalid(err)
	}
	res.ConfigVersion = version
	return &gen.AnalyticsResultHeaders{Response: toAPI(res)}, nil
}

// invalid is a query the semantic layer refuses: 422.
func invalid(err error) error {
	return problem.Unprocessable("analytics.invalid_query", err.Error())
}

// apiError passes on the status of an API read the database path was
// refused (a permission the query's data needs, for example).
func apiError(err error) error {
	var api *apitools.Error
	if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
		return problem.New(api.Status, api.Code, http.StatusText(api.Status), api.Detail)
	}
	return err
}

// slot takes one of the database answers' slots for the query, or answers
// 503 when they are all taken. A store answer takes one too: whether the
// store answers is known only once the query runs.
func (e *Endpoints) slot() (func(), error) {
	select {
	case e.slots <- struct{}{}:
		return func() { <-e.slots }, nil
	default:
		busy := problem.Unavailable("Analytics queries are at their limit on this server; retry shortly.", 1)
		busy.Code = "analytics.busy"
		return nil, busy
	}
}

// Init sizes the fallback slots; erp-server calls it once when wiring.
func (e *Endpoints) Init() *Endpoints {
	n := e.Fallbacks
	if n <= 0 {
		n = 2
	}
	e.slots = make(chan struct{}, n)
	return e
}

// database is the database path: the semantic layer's facts read through
// the API in-process, as the caller.
func (e *Endpoints) database(datasets []string) analytics.Source {
	return analytics.Source{API: &apitools.Client{BaseURL: "http://loopback", HTTP: &http.Client{Transport: e.Loopback}}, Datasets: datasets}
}

// principal is the caller's company scope: the codes of the companies it
// may read, narrowed to those asked for; nil: every company.
func (e *Endpoints) principal(ctx context.Context, asked []string) (analytics.Principal, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return analytics.Principal{}, err
	}
	p := analytics.Principal{ID: "request"}
	if !access.All {
		p.Companies = []string{}
		for _, id := range access.IDs {
			code, ok, err := e.Companies.CompanyCode(ctx, id)
			if err != nil {
				return p, fmt.Errorf("analytics: company of legal entity %s: %w", id, err)
			}
			if ok {
				p.Companies = append(p.Companies, code)
			}
		}
	}
	if asked != nil {
		narrowed := []string{}
		for _, c := range asked {
			if p.Companies == nil || slices.Contains(p.Companies, c) {
				narrowed = append(narrowed, c)
			}
		}
		p.Companies = narrowed
	}
	return p, nil
}

// datasets are the fact datasets the query's metrics read. A dataset the
// caller's permissions do not read refuses the query as the API would
// refuse that read, whichever path answers; one the query does not need is
// read by neither.
func (e *Endpoints) datasets(ctx context.Context, m *analytics.Model, q analytics.Query) ([]string, error) {
	perms, err := authz.Permissions(ctx)
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for _, name := range q.Measures {
		for _, base := range m.BaseMetrics(name) {
			needed[m.Metric(base).Source] = true
		}
	}
	out := []string{}
	for _, f := range analytics.FactDatasets {
		if !needed[f.Dataset] {
			continue
		}
		if !perms[f.Permission] {
			return nil, problem.Forbidden("identity.permission_missing", "This query reads "+f.Dataset+", which requires the permission "+f.Permission+".")
		}
		out = append(out, f.Dataset)
	}
	return out, nil
}
