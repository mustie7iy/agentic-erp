package analytics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// centerPages are validPages with the journal's expense line on cost
// center c1 (node ADMIN), its profit center p1 (node EAST) and segment s1,
// and the centers' and the standard hierarchies' reads. The fake answers
// a path whatever its query, so both kinds' standard hierarchy is h1.
func centerPages() map[string][]string {
	pages := validPages()
	pages["/v1/journal-entries"] = []string{items(map[string]any{"id": "j1", "document_type": "SA", "posting_date": "2026-03-02", "lines": []any{
		map[string]any{"line_no": 1, "account_id": "a2", "amount_lc": map[string]any{"amount": "10.00", "currency": "USD"},
			"cost_center_id": "c1", "cost_center_code": "1100", "profit_center_id": "p1", "profit_center_code": "PC-MTL", "segment_id": "s1", "segment_code": "CA"},
		map[string]any{"line_no": 2, "account_id": "a2", "amount_lc": map[string]any{"amount": "-4.00", "currency": "USD"}}}})}
	pages["/v1/cost-centers"] = []string{items(map[string]any{"id": "c1", "code": "1100", "name": "Finance office"})}
	pages["/v1/profit-centers"] = []string{items(map[string]any{"id": "p1", "code": "PC-MTL", "name": "Montreal office"})}
	pages["/v1/segments"] = []string{items(map[string]any{"id": "s1", "code": "CA", "name": "Canada"})}
	pages["/v1/hierarchies"] = []string{items(map[string]any{"id": "h1"})}
	pages["/v1/hierarchies/h1"] = []string{`{"id": "h1", "nodes": [{"code": "STANDARD", "center_ids": []}, {"code": "ADMIN", "center_ids": ["c1"]},
		{"code": "EAST", "center_ids": ["p1"]}]}`}
	return pages
}

// TestSourceReadsCenters: with Centers, a journal line is named by its
// centers' names and the nodes of the standard hierarchies they are
// placed at, found by the line's ids; a line without centers is under
// none. The centers are asked for in status active, the hierarchies of
// each kind standard (T56i2).
func TestSourceReadsCenters(t *testing.T) {
	m, _ := DefaultModel()
	var asked []string
	api := fakeAPI(t, centerPages())
	api.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if q := r.URL.Query(); q.Get("status") != "" || q.Get("kind") != "" || strings.HasPrefix(r.URL.Path, "/v1/hierarchies/") {
			asked = append(asked, r.URL.Path+" "+q.Get("status")+q.Get("kind")+" "+q.Get("is_standard"))
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	f, err := Source{API: api, Datasets: []string{"gl_lines"}, Centers: true}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/cost-centers active ", "/v1/profit-centers active ", "/v1/hierarchies cost_center true", "/v1/hierarchies/h1  ",
		"/v1/hierarchies profit_center true", "/v1/hierarchies/h1  "}
	if !slices.Equal(asked, want) {
		t.Errorf("reads: got %q, want %q", asked, want)
	}
	if !f.Centers {
		t.Error("facts: got them without the centers, want the centers read")
	}
	for name, c := range map[string]struct {
		by   []string
		want string
	}{
		"by cost center":   {[]string{"cost_center_name", "cost_center_node"}, "[[  -4.00] [Finance office ADMIN 10.00]]"},
		"by profit center": {[]string{"profit_center_name", "profit_center_node"}, "[[  -4.00] [Montreal office EAST 10.00]]"},
		"by segment":       {[]string{"segment_name"}, "[[ -4.00] [Canada 10.00]]"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Execute(m, f, Principal{}, Query{Measures: []string{"expenses"}, By: c.by})
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(res.Rows); got != c.want {
				t.Errorf("expenses %s: got %s, want %s", name, got, c.want)
			}
		})
	}
	t.Run("filtered by a node", func(t *testing.T) {
		res, err := Execute(m, f, Principal{}, Query{Measures: []string{"expenses"}, Where: []Filter{{Dim: "cost_center_node", Op: "in", Values: []string{"ADMIN"}}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(res.Rows); got != "[[10.00]]" {
			t.Errorf("expenses of ADMIN: got %s, want [[10.00]]", got)
		}
	})
}

// TestSourceWithoutCenters: without Centers the centers are not read, and
// a query naming a center's name or node is refused over its facts, never
// answered as though every line had none; a query by the centers' codes
// is answered.
func TestSourceWithoutCenters(t *testing.T) {
	m, _ := DefaultModel()
	pages := centerPages()
	for _, path := range []string{"/v1/profit-centers", "/v1/segments", "/v1/hierarchies", "/v1/hierarchies/h1"} {
		delete(pages, path) // refused if read
	}
	f, err := Source{API: fakeAPI(t, pages), Datasets: []string{"gl_lines"}}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	byName := Query{Measures: []string{"expenses"}, By: []string{"cost_center_name"}}
	if _, err := Execute(m, f, Principal{}, byName); !errors.Is(err, errNoCenters) {
		t.Errorf("by name: got %v, want %v", err, errNoCenters)
	}
	if _, err := DocumentsOf(m, f, Principal{}, Query{Measures: []string{"expenses"}, Where: []Filter{{Dim: "segment_name", Op: "in", Values: []string{"Canada"}}}}); !errors.Is(err, errNoCenters) {
		t.Errorf("documents of a segment's name: got %v, want %v", err, errNoCenters)
	}
	res, err := Execute(m, f, Principal{}, Query{Measures: []string{"expenses"}, By: []string{"cost_center"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(res.Rows); got != "[[ -4.00] [1100 10.00]]" {
		t.Errorf("by code: got %s, want [[ -4.00] [1100 10.00]]", got)
	}
}

// TestSourceCenterErrors: a read of the centers refused, or a tree that
// does not decode, refuses the facts.
func TestSourceCenterErrors(t *testing.T) {
	m, _ := DefaultModel()
	for _, path := range []string{"/v1/cost-centers", "/v1/profit-centers", "/v1/segments", "/v1/hierarchies", "/v1/hierarchies/h1"} {
		t.Run("without "+path, func(t *testing.T) {
			pages := centerPages()
			delete(pages, path)
			_, err := Source{API: fakeAPI(t, pages), Datasets: []string{"gl_lines"}, Centers: true}.Facts(context.Background(), m, time.Now())
			pgtest.CheckErr(t, err, "403")
		})
	}
	t.Run("a tree that does not decode", func(t *testing.T) {
		pages := centerPages()
		pages["/v1/hierarchies/h1"] = []string{`{"nodes": "x"}`}
		_, err := Source{API: fakeAPI(t, pages), Datasets: []string{"gl_lines"}, Centers: true}.Facts(context.Background(), m, time.Now())
		pgtest.CheckErr(t, err, "the standard cost_center hierarchy h1")
	})
}

// TestNamesCenters: a query names the centers by breaking down or
// filtering by a center's name or node, not by a center's code.
func TestNamesCenters(t *testing.T) {
	for name, c := range map[string]struct {
		q    Query
		want bool
	}{
		"by a name":    {Query{By: []string{"company", "profit_center_name"}}, true},
		"by a node":    {Query{Where: []Filter{{Dim: "cost_center_node", Op: "in", Values: []string{"ADMIN"}}}}, true},
		"by the codes": {Query{By: []string{"cost_center", "segment"}, Where: []Filter{{Dim: "profit_center", Op: "in", Values: []string{"PC10"}}}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := NamesCenters(c.q); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestSourcePermissionsOfCenters: reading the centers needs
// ledger.cost_center.read after the accounts' read, for the journal's
// lines alone.
func TestSourcePermissionsOfCenters(t *testing.T) {
	for name, c := range map[string]struct {
		datasets []string
		want     string
	}{
		"journal": {[]string{"gl_lines"}, "tenancy.org.read partner.supplier.read ledger.account.read ledger.cost_center.read ledger.entry.read"},
		"spend":   {[]string{"spend_lines"}, "tenancy.org.read partner.supplier.read ledger.account.read payables.invoice.read procurement.order.read procurement.contract.read ledger.cost_center.read"},
		"sales":   {[]string{"sales_lines"}, "tenancy.org.read partner.supplier.read sales.order.read sales.billing.read"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(Source{Datasets: c.datasets, Centers: true}.Permissions(), " "); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
