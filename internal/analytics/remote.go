package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// QueryPath is erp-server's analytics endpoint (runAnalyticsQuery).
const QueryPath = "/v1/analytics-queries"

// ModelPath is erp-server's governed semantic model (getAnalyticsModel).
const ModelPath = "/v1/analytics-model"

// Remote runs semantic queries on erp-server's analytics endpoint as the
// API client's principal: the server answers from the analytical store or
// the database and says which (ADR-0044), and the client computes nothing.
type Remote struct {
	API *apitools.Client
	// Companies restricts answers to these company codes within the
	// principal's access; nil: all of those.
	Companies []string
	// AsOf asks for data as of this time or later; zero: the freshness
	// target alone.
	AsOf time.Time
}

// remoteQuery is the endpoint's request: the query and its scope.
type remoteQuery struct {
	Query
	Companies []string   `json:"companies,omitempty"`
	AsOf      *time.Time `json:"as_of,omitempty"`
}

// remoteResult is the endpoint's answer: rows of cells, each a value
// (an exact decimal for a measure) and a measure's currency.
type remoteResult struct {
	Result
	Rows [][]struct {
		Value    *string `json:"value"`
		Currency string  `json:"currency"`
	} `json:"rows"`
}

// Query implements Runner.
func (r Remote) Query(ctx context.Context, q Query) (*Result, error) {
	in := remoteQuery{Query: q, Companies: r.Companies}
	if !r.AsOf.IsZero() {
		in.AsOf = &r.AsOf
	}
	resp, err := r.API.Do(ctx, http.MethodPost, QueryPath, nil, in, "")
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, fmt.Errorf("analytics: %w", err)
	}
	var out remoteResult
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("analytics: decode the result: %w", err)
	}
	res := out.Result
	res.Query = q // what a saved report reruns; the server's answer does not repeat it
	res.Rows = make([]Row, 0, len(out.Rows))
	for _, cells := range out.Rows {
		if len(cells) != len(res.Columns) {
			return nil, fmt.Errorf("analytics: a row of %d cells for %d columns", len(cells), len(res.Columns))
		}
		row := make(Row, len(cells))
		for i, c := range cells {
			if row[i], err = cell(res.Columns[i], c.Value, c.Currency); err != nil {
				return nil, err
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return &res, nil
}

// Model is the tenant's governed model as the endpoint answers under it:
// the active configuration version's, so what is described to a person or
// the agent is what their queries are answered with, a metric a change
// request added among it (ADR-0044, T55n).
func (r Remote) Model(ctx context.Context) (*Model, error) {
	resp, err := r.API.Do(ctx, http.MethodGet, ModelPath, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("analytics: the semantic model: %w", err)
	}
	if err := resp.Err(); err != nil {
		return nil, fmt.Errorf("analytics: the semantic model: %w", err)
	}
	var out struct {
		Model config.Analytics `json:"model"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("analytics: decode the semantic model: %w", err)
	}
	return FromConfig(&config.Config{Analytics: &out.Model})
}

// cell is a cell as Execute writes it: a dimension's string, a measure's
// exact Value with exactly the digits the server wrote, or nil.
func cell(col Column, value *string, currency string) (any, error) {
	switch {
	case value == nil:
		return nil, nil
	case col.Kind == "dimension":
		return *value, nil
	}
	_, frac, _ := strings.Cut(*value, ".")
	units, err := money.ParseDecimal(*value, len(frac))
	if err != nil {
		return nil, fmt.Errorf("analytics: %s %q: %w", col.Name, *value, err)
	}
	return Value{Units: units, Scale: len(frac), Currency: currency}, nil
}
