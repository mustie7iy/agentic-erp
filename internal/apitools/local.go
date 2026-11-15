package apitools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Local is a tool the agent's own code answers instead of the API (the
// support agent's search of the contract, its reply, T108a): offered at
// every autonomy level, since it calls nothing, and recorded in the
// session, guarded, observed and run under a lock like an API call. An
// answer is the result's body, with status 200; an error refuses the call
// as a guard does.
func Local(session *Session, name, description string, properties map[string]any, required []string,
	answer func(ctx context.Context, input map[string]any) (any, error)) *Tool {
	t := &Tool{name: name, description: description, session: session, local: answer}
	schema := map[string]any{"type": "object", "properties": properties, "required": required}
	b, _ := json.Marshal(schema)  // built from JSON values
	_ = t.schema.UnmarshalJSON(b) // an object always unmarshals into the schema param
	return t
}

// answerLocally runs a local tool's answer as its call's response.
func (t *Tool) answerLocally(ctx context.Context, input map[string]any) (Response, error) {
	v, err := t.local(ctx, input)
	if err != nil {
		return Response{}, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return Response{}, fmt.Errorf("apitools: %s: encode the answer: %w", t.name, err)
	}
	return Response{Status: 200, Body: b}, nil
}

// FindOperations is the name of the contract's search (Contract).
const FindOperations = "findOperations"

// maxFound bounds the operations one search answers.
const maxFound = 20

// Described is an operation as the contract describes it to an agent.
type Described struct {
	ID           string   `json:"operation_id"`
	Method       string   `json:"method"`
	Path         string   `json:"path"`
	Summary      string   `json:"summary"`
	Description  string   `json:"description,omitempty"`
	Permission   string   `json:"permission,omitempty"`
	ProblemCodes []string `json:"problem_codes,omitempty"`
	// PeopleOnly is x-agent: false: only a person may call it.
	PeopleOnly bool `json:"people_only,omitempty"`
}

// Contract is the tool that reads the contract to an agent (T108a): given
// words, the operations whose id or summary hold every one of them, and
// given operation ids, those operations; at most 20, in the order of
// their ids, each with its method, path, summary, description,
// permission and problem codes. It reads the contract compiled into the
// binary, closed operations included: a person may call those.
func Contract(session *Session) *Tool {
	return Local(session, FindOperations,
		"Find operations of the API contract: by words that their id or summary all hold (query), or by their ids "+
			"(operation_ids). Answers at most 20, with their method, path, summary, description, permission and problem codes.",
		map[string]any{
			"query":         map[string]any{"type": "string", "description": "Words every operation found holds, case ignored."},
			"operation_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Operations to describe."},
		}, nil, func(_ context.Context, input map[string]any) (any, error) {
			query, _ := input["query"].(string) // none: no words
			var ids []string
			list, _ := input["operation_ids"].([]any) // none: no ids
			for _, id := range list {
				ids = append(ids, fmt.Sprint(id))
			}
			return map[string]any{"items": embedded().find(strings.Fields(strings.ToLower(query)), ids)}, nil
		})
}

// find is the operations the words or the ids select, at most maxFound.
func (c contract) find(words, ids []string) []Described {
	out := []Described{}
	paths, _ := c.spec["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, v := range methods {
			op, _ := v.(map[string]any) // a path's parameters are no operation: no id, so never selected
			if d := describe(path, method, op); d.ID != "" && selects(d, words, ids) {
				out = append(out, d)
			}
		}
	}
	slices.SortFunc(out, func(a, b Described) int { return strings.Compare(a.ID, b.ID) })
	return out[:min(len(out), maxFound)]
}

// describe is an operation of the contract as Described; a node that is
// none has no id.
func describe(path, method string, op map[string]any) Described {
	id, _ := op["operationId"].(string)
	d := Described{ID: id, Method: strings.ToUpper(method), Path: path, PeopleOnly: op["x-agent"] == false}
	d.Summary, _ = op["summary"].(string)
	d.Description, _ = op["description"].(string)
	d.Permission, _ = op["x-permission"].(string)
	codes, _ := op["x-problem-codes"].([]any)
	for _, code := range codes {
		d.ProblemCodes = append(d.ProblemCodes, fmt.Sprint(code))
	}
	return d
}

// selects reports whether an operation is one of the ids, or its id and
// summary hold every word (a description mentions much it does not do):
// none selects nothing.
func selects(d Described, words, ids []string) bool {
	if slices.Contains(ids, d.ID) {
		return true
	}
	text := strings.ToLower(d.ID + " " + d.Summary)
	return len(words) > 0 && !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(text, w) })
}
