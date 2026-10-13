package apitools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/api"
)

// maxResult bounds a tool result: lists are paged, so a model that needs
// more asks for the next page.
const maxResult = 40000

// param is a parameter of an operation the tool fills.
type param struct {
	name, in string
	array    bool // sent comma-separated (style form, explode false: the contract's only array style)
}

// operation is what a tool needs to call its operation.
type operation struct {
	id, method, path string
	params           []param
	ifMatch, body    bool
}

// Tool is one API operation as an agent tool. It implements
// anthropic.BetaTool: the input is an object of the operation's path and
// query parameters, if_match where the operation needs it, and body.
type Tool struct {
	name, description string
	schema            anthropic.BetaToolInputSchemaParam
	op                operation
	client            *Client
	session           *Session
	guard             func(map[string]any) error
	observe           func(Response)
}

func (t *Tool) Name() string                                    { return t.name }
func (t *Tool) Description() string                             { return t.description }
func (t *Tool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.schema }

// Guard refuses inputs before they reach the API, for limits an agent keeps
// beyond its permissions (a migration agent only rehearses).
func (t *Tool) Guard(fn func(map[string]any) error) { t.guard = fn }

// Observe sees every answer the operation gives, for agents that report
// what they created.
func (t *Tool) Observe(fn func(Response)) { t.observe = fn }

// Execute calls the operation and records the call in the session. API
// answers, problems included, go back to the model as the result.
func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("tool input is not a JSON object: %w", err)
	}
	if input == nil {
		input = map[string]any{}
	}
	resp, err := t.call(ctx, input)
	summary, isError := "", err != nil
	if err == nil {
		summary, isError = fmt.Sprint(resp.Status), !resp.OK()
	} else {
		summary = err.Error()
	}
	if t.session != nil {
		if rerr := t.session.record(ctx, t.name, input, summary, isError); rerr != nil {
			return nil, rerr
		}
	}
	if err != nil {
		return nil, err
	}
	if t.observe != nil {
		t.observe(resp)
	}
	out, _ := json.Marshal(map[string]any{"status": resp.Status, "etag": resp.ETag, "body": json.RawMessage(nonEmpty(resp.Body))})
	text := string(out)
	if len(text) > maxResult {
		text = text[:maxResult] + "... (truncated; ask for a smaller page)"
	}
	return []anthropic.BetaToolResultBlockParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: text}}}, nil
}

func nonEmpty(b []byte) []byte {
	if len(b) == 0 || !json.Valid(b) {
		return []byte("null")
	}
	return b
}

func (t *Tool) call(ctx context.Context, input map[string]any) (Response, error) {
	if t.guard != nil {
		if err := t.guard(input); err != nil {
			return Response{}, err
		}
	}
	path := t.op.path
	query := url.Values{}
	for _, p := range t.op.params {
		v, ok := input[p.name]
		if !ok {
			continue
		}
		if p.in == "path" {
			path = strings.ReplaceAll(path, "{"+p.name+"}", url.PathEscape(fmt.Sprint(v)))
			continue
		}
		if list, isList := v.([]any); isList && p.array {
			values := make([]string, len(list))
			for i, x := range list {
				values[i] = fmt.Sprint(x)
			}
			query.Set(p.name, strings.Join(values, ","))
			continue
		}
		query.Set(p.name, fmt.Sprint(v))
	}
	if strings.Contains(path, "{") {
		return Response{}, fmt.Errorf("missing path parameter in %s", path)
	}
	ifMatch, _ := input["if_match"].(string)
	var body any
	if b, ok := input["body"]; ok && t.op.body {
		body = b
	}
	return t.client.Do(ctx, t.op.method, path, query, body, ifMatch)
}

// Tools builds the tools for the named operations, calling the API with
// client and recording into session (nil: not recorded).
func Tools(client *Client, session *Session, operationIDs ...string) ([]*Tool, error) {
	c := embedded()
	out := make([]*Tool, 0, len(operationIDs))
	for _, id := range operationIDs {
		t, err := c.tool(id)
		if err != nil {
			return nil, err
		}
		t.client, t.session = client, session
		out = append(out, t)
	}
	return out, nil
}

// Operation is an operation called for a person (the CLI): unlike Tools,
// it includes operations closed to agents, which the API still refuses to
// any principal that is not a person.
func Operation(client *Client, id string) (*Tool, error) {
	t, err := embedded().operation(id, true)
	if err != nil {
		return nil, err
	}
	t.client = client
	return t, nil
}

// AsBeta is the tools as the SDK's tool runner takes them.
func AsBeta(tools []*Tool) []anthropic.BetaTool {
	out := make([]anthropic.BetaTool, len(tools))
	for i, t := range tools {
		out[i] = t
	}
	return out
}

// contract is the parsed OpenAPI document.
type contract struct {
	spec map[string]any
}

// embedded is the contract compiled into the binary, parsed once; api's
// TestSpec keeps it valid YAML.
var embedded = sync.OnceValue(func() contract {
	c, _ := load(api.Spec())
	return c
})

func load(src []byte) (contract, error) {
	var spec map[string]any
	if err := yaml.Unmarshal(src, &spec); err != nil {
		return contract{}, fmt.Errorf("apitools: contract: %w", err)
	}
	return contract{spec: spec}, nil
}

// resolve follows local references.
func (c contract) resolve(n map[string]any) map[string]any {
	for {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		var cur any = c.spec
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := cur.(map[string]any)
			cur = m[part]
		}
		target, ok := cur.(map[string]any)
		if !ok {
			return map[string]any{}
		}
		n = target
	}
}

// schema turns a contract schema into a self-contained JSON schema for a
// request: references inlined, read-only properties and examples dropped,
// open enumerations described.
func (c contract) schema(v any, depth int) any {
	switch n := v.(type) {
	case map[string]any:
		if depth > 12 {
			return map[string]any{}
		}
		return c.schemaObject(c.resolve(n), depth)
	case []any:
		out := make([]any, len(n))
		for i, x := range n {
			out[i] = c.schema(x, depth+1)
		}
		return out
	}
	return v
}

// schemaObject converts one resolved schema object.
func (c contract) schemaObject(n map[string]any, depth int) map[string]any {
	out := map[string]any{}
	var readOnly []string
	for k, x := range n {
		switch {
		case k == "examples" || k == "example" || k == "readOnly":
		case k == "x-extensible-enum":
			out["description"] = enumDescription(n, x.([]any))
		case strings.HasPrefix(k, "x-"):
		case k == "properties":
			var props map[string]any
			props, readOnly = c.schemaProperties(x.(map[string]any), depth)
			out[k] = props
		case k == "description":
			// An open enumeration's description already includes this one.
			if _, has := out["description"]; !has {
				out[k] = x
			}
		default:
			out[k] = c.schema(x, depth+1)
		}
	}
	if req, ok := out["required"].([]any); ok && len(readOnly) > 0 {
		kept := []any{}
		for _, r := range req {
			if !slices.Contains(readOnly, fmt.Sprint(r)) {
				kept = append(kept, r)
			}
		}
		out["required"] = kept
	}
	return out
}

// enumDescription is the description of an open enumeration, with its
// known values appended so an agent can choose among them.
func enumDescription(n map[string]any, enum []any) string {
	var values []string
	for _, e := range enum {
		values = append(values, fmt.Sprint(e))
	}
	desc, _ := n["description"].(string)
	return strings.TrimSpace(desc + " Known values: " + strings.Join(values, ", ") + ".")
}

// schemaProperties converts the properties of an object schema, leaving out
// the read-only ones, which it returns by name.
func (c contract) schemaProperties(properties map[string]any, depth int) (map[string]any, []string) {
	props := map[string]any{}
	var readOnly []string
	for name, p := range properties {
		if pm, ok := p.(map[string]any); ok && (pm["readOnly"] == true || c.resolve(pm)["readOnly"] == true) {
			readOnly = append(readOnly, name)
			continue
		}
		props[name] = c.schema(p, depth+1)
	}
	return props, readOnly
}

// tool builds the agent tool of an operation.
func (c contract) tool(id string) (*Tool, error) { return c.operation(id, false) }

// operation builds the tool of an operation; forPeople admits operations
// closed to agents.
func (c contract) operation(id string, forPeople bool) (*Tool, error) {
	paths, _ := c.spec["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, v := range methods {
			op, ok := v.(map[string]any)
			if !ok || op["operationId"] != id {
				continue
			}
			if op["x-agent"] == false && !forPeople {
				return nil, fmt.Errorf("apitools: %s is not open to agents (x-agent: false)", id)
			}
			return c.buildTool(id, strings.ToUpper(method), path, op), nil
		}
	}
	return nil, fmt.Errorf("apitools: no operation %s in the contract", id)
}

// buildTool is the tool of one contract operation: its parameters and body
// become the properties of one input object.
func (c contract) buildTool(id, method, path string, op map[string]any) *Tool {
	t := &Tool{name: id, op: operation{id: id, method: method, path: path}}
	summary, _ := op["summary"].(string)
	desc, _ := op["description"].(string)
	t.description = summary + ". " + desc
	props, required := map[string]any{}, []any{}
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		required = c.parameter(t, c.resolve(raw.(map[string]any)), props, required)
	}
	if rb, ok := op["requestBody"].(map[string]any); ok {
		rb = c.resolve(rb)
		content, _ := rb["content"].(map[string]any)
		for _, media := range content {
			s := c.schema(media.(map[string]any)["schema"], 0)
			props["body"] = s
			t.op.body = true
		}
		if rb["required"] == true {
			required = append(required, "body")
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "required": required}
	b, _ := json.Marshal(schema)  // built from YAML values
	_ = t.schema.UnmarshalJSON(b) // an object always unmarshals into the schema param
	return t
}

// parameter adds one operation parameter to the tool's input properties
// and returns the required names with it added when it is required.
func (c contract) parameter(t *Tool, p map[string]any, props map[string]any, required []any) []any {
	name, _ := p["name"].(string)
	in, _ := p["in"].(string)
	switch {
	case in == "header" && strings.EqualFold(name, "If-Match"):
		t.op.ifMatch = true
		props["if_match"] = map[string]any{"type": "string", "description": "The ETag of the resource as you last read it (the etag of its latest result)."}
		return append(required, "if_match")
	case in != "path" && in != "query":
		return required // Idempotency-Key is set by the client
	}
	s, _ := c.schema(p["schema"], 0).(map[string]any)
	if d, ok := p["description"].(string); ok {
		if sd, has := s["description"].(string); has {
			d += " " + sd
		}
		s["description"] = d
	}
	props[name] = s
	t.op.params = append(t.op.params, param{name: name, in: in, array: s["type"] == "array"})
	if p["required"] == true {
		required = append(required, name)
	}
	return required
}
