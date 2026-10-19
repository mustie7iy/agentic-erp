package eval

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// check runs one check as the tenant's administrator: failed is why the
// agent failed it, nil when it holds; err is the harness failing (a value
// not saved, the API unreachable), which stops the run rather than grade
// the agent for it.
func check(ctx context.Context, admin *apitools.Client, ch Check, saved map[string]string) (failed, err error) {
	if len(ch.ToolsWithin) > 0 {
		return toolsWithin(ctx, admin, ch.ToolsWithin)
	}
	path, err := fillString(ch.Get, saved)
	if err != nil {
		return nil, err
	}
	want, err := fill(any(ch.Expect), saved)
	if err != nil {
		return nil, err
	}
	res, err := admin.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	switch {
	case res.Status >= 500:
		return nil, fmt.Errorf("GET %s answered %d: %s", path, res.Status, res.Body)
	case !res.OK():
		return fmt.Errorf("GET %s answered %d: %s", path, res.Status, res.Body), nil
	}
	doc, err := decode(res.Body)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(ch.Expect))
	for k := range ch.Expect {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := want.(map[string]any)[k] // fill keeps a map a map
		got, ok := at(doc, k)
		m, _ := v.(map[string]any) // a value that is no map is no $not
		if not, isNot := m["$not"]; isNot {
			if ok && same(got, not) {
				return fmt.Errorf("%s %s: got %s, want anything else", path, k, text(got)), nil
			}
			continue
		}
		if !ok || !same(got, v) {
			return fmt.Errorf("%s %s: got %s, want %s", path, k, text(got), text(v)), nil
		}
	}
	return nil, nil
}

// get reads a path and decodes its answer, which must be 2xx.
func get(ctx context.Context, c *apitools.Client, path string) (any, error) {
	res, err := c.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	if !res.OK() {
		return nil, fmt.Errorf("GET %s answered %d: %s", path, res.Status, res.Body)
	}
	return decode(res.Body)
}

// toolsWithin holds when every tool call the tenant's agents recorded is
// one of allowed: the agents' own record, through the API (ADR-0036).
func toolsWithin(ctx context.Context, admin *apitools.Client, allowed []string) (failed, err error) {
	type item struct {
		ID   string `json:"id"`
		Tool string `json:"tool"`
	}
	sessions, err := apitools.List[item](ctx, admin, "/v1/agent-sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("eval: list the agent sessions: %w", err)
	}
	for _, s := range sessions {
		calls, err := apitools.List[item](ctx, admin, "/v1/agent-sessions/"+s.ID+"/tool-calls", nil)
		if err != nil {
			return nil, fmt.Errorf("eval: list a session's tool calls: %w", err)
		}
		for _, c := range calls {
			if !slices.Contains(allowed, c.Tool) {
				return fmt.Errorf("the agent called %s, outside %v", c.Tool, allowed), nil
			}
		}
	}
	return nil, nil
}
