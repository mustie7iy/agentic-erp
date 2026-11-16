package apitools

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Explained is what the contract says of an operation and of one of its
// fields or problem codes (the enablement agent's in-context help,
// T109b): the operation as findOperations describes it, and the field's
// description or the problem code asked about.
type Explained struct {
	Operation Described `json:"operation"`
	// Field is the field asked about, as asked (a parameter, or a property
	// of the request or a response body, dotted through nested objects),
	// and FieldDescription its description in the contract.
	Field            string `json:"field,omitempty"`
	FieldDescription string `json:"field_description,omitempty"`
	// Problem is the problem code asked about: one the operation lists, or
	// one the contract lists for every operation.
	Problem string `json:"problem,omitempty"`
}

// ErrNotInContract refuses help on an operation, field or problem code the
// contract does not have, before anything else is asked.
var ErrNotInContract = errors.New("apitools: not in the contract")

// Explain is what the contract compiled into the binary says of the
// operation and, when asked, of its field or problem code; one not in the
// contract is ErrNotInContract. Closed operations are explained too: a
// person may call them.
func Explain(operationID, field, problem string) (Explained, error) {
	return embedded().explain(operationID, field, problem)
}

// explain is Explain on the contract.
func (c contract) explain(operationID, field, problem string) (Explained, error) {
	path, method, op, found := c.find1(operationID)
	if !found {
		return Explained{}, fmt.Errorf("%w: no operation %q", ErrNotInContract, operationID)
	}
	e := Explained{Operation: describe(path, method, op), Field: field, Problem: problem}
	if field != "" {
		d, ok := c.fieldDescription(op, field)
		if !ok {
			return Explained{}, fmt.Errorf("%w: no field %q of %s", ErrNotInContract, field, operationID)
		}
		e.FieldDescription = d
	}
	if problem != "" && !slices.Contains(e.Operation.ProblemCodes, problem) && !slices.Contains(c.everyOperations(), problem) {
		return Explained{}, fmt.Errorf("%w: no problem code %q of %s", ErrNotInContract, problem, operationID)
	}
	return e, nil
}

// find1 is the operation of the id, with its path and method.
func (c contract) find1(id string) (path, method string, op map[string]any, found bool) {
	paths, _ := c.spec["paths"].(map[string]any)
	for p, item := range paths {
		methods, _ := item.(map[string]any)
		for m, v := range methods {
			if o, ok := v.(map[string]any); ok && id != "" && o["operationId"] == id {
				return p, m, o, true
			}
		}
	}
	return "", "", nil, false
}

// everyOperations are the problem codes the contract lists for every
// operation (the request pipeline's, ADR-0052 G7).
func (c contract) everyOperations() []string {
	var out []string
	codes, _ := c.spec["x-problem-codes"].([]any)
	for _, code := range codes {
		out = append(out, fmt.Sprint(code))
	}
	return out
}

// fieldDescription is the description of the operation's field: a
// parameter of that name, else a property at the dotted path of its
// request body, else of a successful response's body, in the order of
// their media types and statuses.
func (c contract) fieldDescription(op map[string]any, field string) (string, bool) {
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		p, _ := raw.(map[string]any)
		if p, err := c.resolve(p, "parameter"); err == nil && p["name"] == field {
			d, _ := p["description"].(string)
			return d, true
		}
	}
	var bodies []any
	if rb, ok := op["requestBody"].(map[string]any); ok {
		bodies = append(bodies, rb)
	}
	responses, _ := op["responses"].(map[string]any)
	for _, status := range sortedKeys(responses) {
		if strings.HasPrefix(status, "2") {
			bodies = append(bodies, responses[status])
		}
	}
	for _, b := range bodies {
		body, _ := b.(map[string]any)
		if body, err := c.resolve(body, "body"); err == nil {
			content, _ := body["content"].(map[string]any)
			for _, mediaType := range sortedKeys(content) {
				media, _ := content[mediaType].(map[string]any)
				if d, ok := c.property(media["schema"], strings.Split(field, "."), 0); ok {
					return d, true
				}
			}
		}
	}
	return "", false
}

// property is the description of the property at the path under a
// schema: through its properties, its items and the schemas it combines.
func (c contract) property(v any, path []string, depth int) (string, bool) {
	n, _ := v.(map[string]any)
	n, err := c.resolve(n, "schema")
	if err != nil || n == nil || depth > 12 {
		return "", false
	}
	if items, ok := n["items"]; ok {
		return c.property(items, path, depth+1)
	}
	for _, combined := range []string{"allOf", "oneOf", "anyOf"} {
		list, _ := n[combined].([]any)
		for _, s := range list {
			if d, ok := c.property(s, path, depth+1); ok {
				return d, true
			}
		}
	}
	props, _ := n["properties"].(map[string]any)
	p, ok := props[path[0]]
	if !ok {
		return "", false
	}
	if len(path) > 1 {
		return c.property(p, path[1:], depth+1)
	}
	return c.description(p), true
}

// description is a property's own description, else that of the schema
// it refers to.
func (c contract) description(v any) string {
	n, _ := v.(map[string]any)
	if d, ok := n["description"].(string); ok {
		return d
	}
	resolved, _ := c.resolve(n, "schema") // a property that resolves to nothing has no description
	d, _ := resolved["description"].(string)
	return d
}

// sortedKeys are a node's keys in order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
