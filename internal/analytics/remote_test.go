package analytics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// remote is a Remote on a server answering body with status, which
// records the request it was sent.
func remote(t *testing.T, status int, body string, sent *map[string]any) Remote {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) // a test server's request body
		if sent != nil {
			_ = json.Unmarshal(b, sent) // the assertions say what was missing
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // the client reports what it could not read
	}))
	t.Cleanup(srv.Close)
	return Remote{API: &apitools.Client{BaseURL: srv.URL}}
}

const remoteAnswer = `{"query_id": "q_1", "columns": [{"name": "company", "label": "Company", "kind": "dimension"},
	{"name": "revenue", "label": "Revenue", "kind": "measure", "type": "money"}, {"name": "margin", "label": "Margin", "kind": "measure", "type": "ratio"}],
	"rows": [[{"value": "1000"}, {"value": "-1234.567", "currency": "BHD"}, {"value": null}], [{"value": null}, {"value": "5000", "currency": "JPY"}, {"value": "0.2500"}]],
	"source": "store", "as_of": "2026-10-20T12:00:00Z", "data_through": "2026-09-30", "definitions": [], "truncated": false}`

func TestRemote(t *testing.T) {
	var sent map[string]any
	r := remote(t, 200, remoteAnswer, &sent)
	r.Companies, r.AsOf = []string{"1000"}, time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	res, err := r.Query(context.Background(), Query{Measures: []string{"revenue"}, Ledger: "l1"})
	if err != nil {
		t.Fatal(err)
	}
	if sent["as_of"] != "2026-10-20T11:00:00Z" || sent["ledger"] != "l1" {
		t.Errorf("request: got %v, want as_of and the ledger sent", sent)
	}
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"dimension", res.Rows[0][0], "1000"},
		{"amount", res.Rows[0][1], Value{Units: -1234567, Scale: 3, Currency: "BHD"}},
		{"not computable", res.Rows[0][2], nil},
		{"dimension without a value", res.Rows[1][0], nil},
		{"whole amount", res.Rows[1][1], Value{Units: 5000, Scale: 0, Currency: "JPY"}},
		{"ratio", res.Rows[1][2], Value{Units: 2500, Scale: 4}},
		{"source", res.Source, SourceStore},
		{"query", res.Query.Ledger, "l1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %#v, want %#v", c.got, c.want)
			}
		})
	}
	if _, err := remote(t, 200, remoteAnswer, nil).Query(context.Background(), Query{}); err != nil {
		t.Errorf("without as_of: %v", err)
	}
}

func TestRemoteFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"refused":       {422, `{"status": 422, "code": "analytics.invalid_query", "detail": "unknown metric nope"}`, "unknown metric nope"},
		"not JSON":      {200, `{`, "decode the result"},
		"short row":     {200, `{"columns": [{"name": "a", "kind": "dimension"}], "rows": [[]]}`, "a row of 0 cells for 1 columns"},
		"not a decimal": {200, `{"columns": [{"name": "m", "kind": "measure"}], "rows": [[{"value": "1e3"}]]}`, `m "1e3"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := remote(t, c.status, c.body, nil).Query(context.Background(), Query{})
			pgtest.CheckErr(t, err, c.want)
		})
	}
	_, err := Remote{API: &apitools.Client{BaseURL: "http://127.0.0.1:1"}}.Query(context.Background(), Query{})
	pgtest.CheckErr(t, err, "apitools")
}

// TestRemoteModel: the CLI and the agent describe the model the endpoint
// returns (a metric a change request added among it), not the seed; one
// that is refused, does not read or does not validate fails.
func TestRemoteModel(t *testing.T) {
	const answer = `{"config_version": 2, "model": {"calendar": {"fiscal_year_start_month": 4},
		"dimensions": [{"name": "company", "label": "Company"}],
		"metrics": [{"name": "aa_total", "label": "Total", "description": "Everything posted", "type": "money", "source": "gl_lines", "aggregate": "sum"}]}}`
	m, err := remote(t, 200, answer, nil).Model(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Metric("aa_total"); got == nil || got.Description != "Everything posted" {
		t.Errorf("aa_total: got %+v, want the endpoint's metric", got)
	}
	if m.Metric("revenue") != nil {
		t.Error("revenue: got the seed's metric, want the endpoint's model alone")
	}
	if got := m.Calendar.FiscalYearStartMonth; got != 4 {
		t.Errorf("fiscal year start: got %d, want 4", got)
	}
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"refused":      {403, `{"status": 403, "code": "identity.permission_missing", "detail": "analytics.query.run"}`, "analytics: the semantic model"},
		"not JSON":     {200, `{`, "decode the semantic model"},
		"not governed": {200, `{"model": {"metrics": [{"name": "x", "type": "money", "source": "nowhere", "aggregate": "sum"}]}}`, "unknown dataset"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := remote(t, c.status, c.body, nil).Model(context.Background())
			pgtest.CheckErr(t, err, c.want)
		})
	}
	_, err = Remote{API: &apitools.Client{BaseURL: "http://127.0.0.1:1"}}.Model(context.Background())
	pgtest.CheckErr(t, err, "apitools")
}

// TestSessionRunner: a session with a Runner keeps its answers, and its
// data reaches the latest of theirs.
func TestSessionRunner(t *testing.T) {
	m, _ := DefaultModel()
	s := NewSession(m, nil, Principal{ID: "p"})
	s.Runner = remote(t, 200, remoteAnswer, nil)
	if _, err := s.Run(context.Background(), Query{Measures: []string{"revenue"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.DataThrough(); got != "2026-09-30" {
		t.Errorf("data through: got %q, want 2026-09-30", got)
	}
	s.Runner = remote(t, 422, `{"detail": "no"}`, nil)
	if _, err := s.Run(context.Background(), Query{}); err == nil {
		t.Error("refused query: got nil, want the error")
	}
}
