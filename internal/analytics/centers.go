package analytics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// The centers' names and nodes (T56i2; ADR-0050 §7, ADR-0044 As built
// T56i2): a journal line names its cost center, profit center and segment
// by id and code; these dimensions name them as people know them, by the
// center's name and the node of today's standard hierarchy it is placed at
// (decision P7). The store joins its snapshots of the centers to the lines
// by the company and code (a cost center's code is unique within its
// company alone); the database path finds the same centers by the line's
// ids.

// CenterDimensions are the dimensions read from the centers rather than
// from the journal's lines.
var CenterDimensions = []string{"cost_center_name", "cost_center_node", "profit_center_name", "profit_center_node", "segment_name"}

// NamesCenters reports whether q breaks down or filters by one of
// CenterDimensions: the database path must then read the centers
// (Source.Centers), which needs ledger.cost_center.read beside the
// journal's permissions. A query of the journal by the centers' codes
// alone needs neither.
func NamesCenters(q Query) bool {
	for _, d := range q.By {
		if slices.Contains(CenterDimensions, d) {
			return true
		}
	}
	for _, f := range q.Where {
		if slices.Contains(CenterDimensions, f.Dim) {
			return true
		}
	}
	return false
}

// errNoCenters refuses a query naming a center's name or node over facts
// read without the centers: answered, every line would be under none.
var errNoCenters = errors.New("the facts were read without the centers' names and nodes this query names")

// withCenters is nil when f answers q's center dimensions, or q names none.
func (f *Facts) withCenters(q Query) error {
	if NamesCenters(q) && !f.Centers {
		return errNoCenters
	}
	return nil
}

// centers is what names a line's centers: each active cost or profit
// center's and each segment's name, and each center's node code in its
// kind's standard hierarchy, by the center's or segment's id (ids are
// UUIDs, so one map holds the three kinds).
type centers struct {
	names, nodes map[string]string
}

// named is a cost center, profit center or segment as the API lists it.
type named struct{ ID, Name string }

// standardTree is a standard hierarchy as GET /v1/hierarchies/{id} answers
// it: each node's code and the centers placed at it.
type standardTree struct {
	Nodes []struct {
		Code      string
		CenterIDs []string `json:"center_ids"`
	}
}

// readCenters reads the active cost centers of the companies the caller may
// act in, the tenant's active profit centers and its segments, and the
// standard hierarchy of each kind, as the caller.
func readCenters(ctx context.Context, api *apitools.Client) (*centers, error) {
	c := &centers{names: map[string]string{}, nodes: map[string]string{}}
	active := url.Values{"status": {"active"}}
	for _, l := range []struct {
		path  string
		query url.Values
	}{{"/v1/cost-centers", active}, {"/v1/profit-centers", active}, {"/v1/segments", nil}} {
		list, err := apitools.List[named](ctx, api, l.path, l.query)
		if err != nil {
			return nil, err
		}
		for _, n := range list {
			c.names[n.ID] = n.Name
		}
	}
	for _, kind := range []string{"cost_center", "profit_center"} {
		if err := c.readNodes(ctx, api, kind); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// readNodes records the node each center of the kind's standard hierarchy
// is placed at; a tenant without one leaves its centers without a node.
func (c *centers) readNodes(ctx context.Context, api *apitools.Client, kind string) error {
	standard, err := apitools.List[struct{ ID string }](ctx, api, "/v1/hierarchies", url.Values{"kind": {kind}, "is_standard": {"true"}})
	if err != nil {
		return err
	}
	for _, h := range standard {
		resp, err := api.Do(ctx, http.MethodGet, "/v1/hierarchies/"+h.ID, nil, nil, "")
		var tree standardTree
		if err == nil {
			err = resp.Decode(&tree)
		}
		if err != nil {
			return fmt.Errorf("analytics: the standard %s hierarchy %s: %w", kind, h.ID, err)
		}
		for _, n := range tree.Nodes {
			for _, id := range n.CenterIDs {
				c.nodes[id] = n.Code
			}
		}
	}
	return nil
}

// dims adds a journal line's center dimensions by its centers' ids, ""
// where it has none.
func (c *centers) dims(dims map[string]string, costCenter, profitCenter, segment string) {
	dims["cost_center_name"], dims["cost_center_node"] = c.names[costCenter], c.nodes[costCenter]
	dims["profit_center_name"], dims["profit_center_node"] = c.names[profitCenter], c.nodes[profitCenter]
	dims["segment_name"] = c.names[segment]
}
