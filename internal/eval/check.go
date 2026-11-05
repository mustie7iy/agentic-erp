package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// Reply is what a check gets to judge the agent's reply (Env.Reply)
// rather than the API state it left.
const Reply = "$reply"

// check runs one check as the tenant's administrator, or on the agent's
// reply: failed is why the agent failed it, nil when it holds; err is the
// harness failing (a value not saved, the API unreachable), which stops
// the run rather than grade the agent for it.
func check(ctx context.Context, admin *apitools.Client, ch Check, saved map[string]string, reply any) (failed, err error) {
	if len(ch.ToolsWithin) > 0 {
		return toolsWithin(ctx, admin, ch.ToolsWithin)
	}
	want, err := fill(any(ch.Expect), saved)
	if err != nil {
		return nil, err
	}
	path, doc := Reply, any(nil)
	if ch.Get == Reply {
		if reply == nil {
			return errors.New("the agent replied nothing"), nil
		}
		// As an answer would be read: numbers as they were written.
		b, err := json.Marshal(reply)
		if err != nil {
			return nil, fmt.Errorf("eval: encode the reply: %w", err)
		}
		doc, _ = decode(b) // what Marshal wrote decodes
	} else if path, doc, failed, err = read(ctx, admin, ch.Get, saved); failed != nil || err != nil {
		return failed, err
	}
	return expect(doc, path, want.(map[string]any)), nil // fill keeps a map a map
}

// read is a check's GET as the administrator: failed when the API refuses
// it (4xx), err when the harness cannot read it.
func read(ctx context.Context, admin *apitools.Client, get string, saved map[string]string) (path string, doc any, failed, err error) {
	if path, err = fillString(get, saved); err != nil {
		return "", nil, nil, err
	}
	res, err := admin.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return "", nil, nil, fmt.Errorf("eval: %w", err)
	}
	switch {
	case res.Status >= 500:
		return "", nil, nil, fmt.Errorf("GET %s answered %d: %s", path, res.Status, res.Body)
	case !res.OK():
		return "", nil, fmt.Errorf("GET %s answered %d: %s", path, res.Status, res.Body), nil
	}
	doc, err = decode(res.Body)
	return path, doc, nil, err
}

// expect is why doc does not hold every value of want, in path order; nil
// when it does.
func expect(doc any, path string, want map[string]any) error {
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := holds(doc, path, k, want[k]); err != nil {
			return err
		}
	}
	return nil
}

// holds is why doc's value at k is not v: equal, different with $not,
// text without v's with $lacks, or text with v's with $contains (an
// explanation citing a document's number). A path that leads nowhere
// fails either way: a mistyped one must not pass a safety check unseen.
func holds(doc any, path, k string, v any) error {
	got, ok := at(doc, k)
	m, _ := v.(map[string]any) // a value that is no map is no notation
	if not, isNot := m["$not"]; isNot {
		if !ok || same(got, not) {
			return fmt.Errorf("%s %s: got %s, want anything but %s", path, k, text(got), text(not))
		}
		return nil
	}
	if lacks, isLacks := m["$lacks"]; isLacks {
		if !ok || strings.Contains(text(got), text(lacks)) {
			return fmt.Errorf("%s %s: got %s, want it without %s", path, k, text(got), text(lacks))
		}
		return nil
	}
	if has, isHas := m["$contains"]; isHas {
		if !ok || !strings.Contains(text(got), text(has)) {
			return fmt.Errorf("%s %s: got %s, want it to contain %s", path, k, text(got), text(has))
		}
		return nil
	}
	if !ok || !same(got, v) {
		return fmt.Errorf("%s %s: got %s, want %s", path, k, text(got), text(v))
	}
	return nil
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
