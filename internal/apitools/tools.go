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
	multipart        bool // the body is a multipart form (Multipart)
}

// Tool is one API operation as an agent tool. It implements
// anthropic.BetaTool: the input is an object of the operation's path and
// query parameters, if_match where the operation needs it, and body.
type Tool struct {
	name, description string
	schema            anthropic.BetaToolInputSchemaParam
	op                operation
	level             int // the autonomy level that performs the operation (api.Operation.Level)
	client            *Client
	session           *Session
	guard             func(map[string]any) error
	observe           func(Response)
	exclusive         *sync.Mutex
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

// Exclusive runs each call of the tool, its guard, the API's answer and
// its observer, holding mu: the model's runner executes the tool calls
// of one turn concurrently, so tools whose guards and observers share
// state (a check that one object gets one proposal, recorded when the
// API accepts it) share one mutex and see each call whole.
func (t *Tool) Exclusive(mu *sync.Mutex) { t.exclusive = mu }

// Execute calls the operation and records the call in the session. API
// answers, problems included, go back to the model as the result.
func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	if t.exclusive != nil {
		t.exclusive.Lock()
		defer t.exclusive.Unlock()
	}
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
	// A status, a tag and a body nonEmpty has made valid JSON always encode.
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
	body, err := t.body(input)
	if err != nil {
		return Response{}, err
	}
	return t.client.Do(ctx, t.op.method, path, query, body, ifMatch)
}

// body is the input's body as the operation takes it: a multipart
// operation's as its form (formOf), any other as it is; none when the
// operation has none.
func (t *Tool) body(input map[string]any) (any, error) {
	b, ok := input["body"]
	switch {
	case !ok || !t.op.body:
		return nil, nil
	case t.op.multipart:
		return formOf(b)
	}
	return b, nil
}

// formOf is a multipart operation's body as its form: a field of text is
// sent as it is, any other value as its JSON.
func formOf(body any) (Multipart, error) {
	fields, ok := body.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the body of a multipart operation is an object of its fields, not %T", body)
	}
	form := Multipart{}
	for k, v := range fields {
		if s, isText := v.(string); isText {
			form[k] = s
			continue
		}
		b, _ := json.Marshal(v) // decoded from JSON, so it encodes again
		form[k] = string(b)
	}
	return form, nil
}

// Call calls the operation with the input as Execute takes it and returns
// the answer as it came, whatever its media type and size, without
// recording it in a session: the command line's erp call.
func (t *Tool) Call(ctx context.Context, input map[string]any) (Response, error) {
	return t.call(ctx, input)
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
	c, _ := load(api.Spec()) // TestSpec keeps the embedded contract loadable
	return c
})

func load(src []byte) (contract, error) {
	var spec map[string]any
	if err := yaml.Unmarshal(src, &spec); err != nil {
		return contract{}, fmt.Errorf("apitools: contract: %w", err)
	}
	return contract{spec: spec}, nil
}

// maxRefHops bounds a chain of references: real contracts follow one or
// two, so a longer chain is a cycle.
const maxRefHops = 32

// resolve follows local references from the node at at. A reference that
// points to no object, or a chain longer than maxRefHops (a cycle), is a
// malformed contract: guessing an empty schema would hand an agent a tool
// that accepts anything.
func (c contract) resolve(n map[string]any, at string) (map[string]any, error) {
	for hops := 0; ; hops++ {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n, nil
		}
		if hops == maxRefHops {
			return nil, malformed(at, "a resolvable reference (a cycle through "+ref+")")
		}
		var cur any = c.spec
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := cur.(map[string]any)
			cur = m[part]
		}
		target, ok := cur.(map[string]any)
		if !ok {
			return nil, malformed(at, "a resolvable reference ("+ref+" points nowhere)")
		}
		n = target
	}
}

// malformed is the error for a contract node that does not have the shape
// the contract format gives it; at is where the node sits.
func malformed(at, want string) error {
	return fmt.Errorf("contract %s is not %s", at, want)
}

// schema turns a contract schema into a self-contained JSON schema for a
// request: references inlined, read-only properties and examples dropped,
// open enumerations described. at is where v sits in the contract, for
// errors.
func (c contract) schema(v any, at string, depth int) (any, error) {
	switch n := v.(type) {
	case map[string]any:
		if depth > 12 {
			return map[string]any{}, nil
		}
		resolved, err := c.resolve(n, at)
		if err != nil {
			return nil, err
		}
		return c.schemaObject(resolved, at, depth)
	case []any:
		out := make([]any, len(n))
		for i, x := range n {
			s, err := c.schema(x, fmt.Sprintf("%s[%d]", at, i), depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	}
	return v, nil
}

// schemaObject converts one resolved schema object.
func (c contract) schemaObject(n map[string]any, at string, depth int) (map[string]any, error) {
	out := map[string]any{}
	var readOnly []string
	for k, x := range n {
		ro, err := c.schemaKeyword(out, n, k, x, at+"."+k, depth)
		if err != nil {
			return nil, err
		}
		readOnly = append(readOnly, ro...)
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
	return out, nil
}

// schemaKeyword converts keyword k of schema object n into out and returns
// the read-only properties it left out.
func (c contract) schemaKeyword(out, n map[string]any, k string, x any, at string, depth int) ([]string, error) {
	switch {
	case k == "examples" || k == "example" || k == "readOnly":
	case k == "x-extensible-enum":
		enum, ok := x.([]any)
		if !ok {
			return nil, malformed(at, "a list")
		}
		out["description"] = enumDescription(n, enum)
	case strings.HasPrefix(k, "x-"):
	case k == "properties":
		properties, ok := x.(map[string]any)
		if !ok {
			return nil, malformed(at, "an object")
		}
		props, readOnly, err := c.schemaProperties(properties, at, depth)
		if err != nil {
			return nil, err
		}
		out[k] = props
		return readOnly, nil
	case k == "description":
		// An open enumeration's description already includes this one.
		if _, has := out["description"]; !has {
			out[k] = x
		}
	default:
		s, err := c.schema(x, at, depth+1)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return nil, nil
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
func (c contract) schemaProperties(properties map[string]any, at string, depth int) (map[string]any, []string, error) {
	props := map[string]any{}
	var readOnly []string
	for name, p := range properties {
		pat := at + "." + name
		if pm, ok := p.(map[string]any); ok {
			resolved, err := c.resolve(pm, pat)
			if err != nil {
				return nil, nil, err
			}
			if pm["readOnly"] == true || resolved["readOnly"] == true {
				readOnly = append(readOnly, name)
				continue
			}
		}
		s, err := c.schema(p, pat, depth+1)
		if err != nil {
			return nil, nil, err
		}
		props[name] = s
	}
	return props, readOnly, nil
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
			t, err := c.buildTool(id, method, path, op)
			if err != nil {
				return nil, fmt.Errorf("apitools: operation %s: %w", id, err)
			}
			return t, nil
		}
	}
	return nil, fmt.Errorf("apitools: no operation %s in the contract", id)
}

// buildTool is the tool of one contract operation: its parameters and body
// become the properties of one input object.
func (c contract) buildTool(id, method, path string, op map[string]any) (*Tool, error) {
	at := "paths." + path + "." + method
	autonomy, _ := op["x-autonomy"].(string)
	level := api.Operation{Method: strings.ToUpper(method), Proposes: autonomy == "propose", Reads: autonomy == "read"}.Level()
	t := &Tool{name: id, level: level, op: operation{id: id, method: strings.ToUpper(method), path: path}}
	summary, _ := op["summary"].(string)
	desc, _ := op["description"].(string)
	t.description = summary + ". " + desc
	props, required := map[string]any{}, []any{}
	params, _ := op["parameters"].([]any)
	for i, raw := range params {
		pat := fmt.Sprintf("%s.parameters[%d]", at, i)
		p, ok := raw.(map[string]any)
		if !ok {
			return nil, malformed(pat, "an object")
		}
		resolved, err := c.resolve(p, pat)
		if err != nil {
			return nil, err
		}
		if required, err = c.parameter(t, resolved, pat, props, required); err != nil {
			return nil, err
		}
	}
	bodyRequired, err := c.requestBody(t, op, at, props)
	if err != nil {
		return nil, err
	}
	if bodyRequired {
		required = append(required, "body")
	}
	schema := map[string]any{"type": "object", "properties": props, "required": required}
	b, _ := json.Marshal(schema)  // built from YAML values
	_ = t.schema.UnmarshalJSON(b) // an object always unmarshals into the schema param
	return t, nil
}

// requestBody adds the operation's request body, if it has one, to the
// tool's input properties and reports whether it is required.
func (c contract) requestBody(t *Tool, op map[string]any, at string, props map[string]any) (bool, error) {
	rb, ok := op["requestBody"].(map[string]any)
	if !ok {
		return false, nil
	}
	rb, err := c.resolve(rb, at+".requestBody")
	if err != nil {
		return false, err
	}
	content, _ := rb["content"].(map[string]any)
	for mediaType, media := range content {
		mat := at + ".requestBody.content." + mediaType
		m, ok := media.(map[string]any)
		if !ok {
			return false, malformed(mat, "an object")
		}
		s, err := c.schema(m["schema"], mat+".schema", 0)
		if err != nil {
			return false, err
		}
		props["body"] = s
		t.op.body = true
		t.op.multipart = mediaType == "multipart/form-data"
	}
	return rb["required"] == true, nil
}

// parameter adds one operation parameter to the tool's input properties
// and returns the required names with it added when it is required.
func (c contract) parameter(t *Tool, p map[string]any, at string, props map[string]any, required []any) ([]any, error) {
	name, _ := p["name"].(string)
	in, _ := p["in"].(string)
	switch {
	case in == "header" && strings.EqualFold(name, "If-Match"):
		t.op.ifMatch = true
		props["if_match"] = map[string]any{"type": "string", "description": "The ETag of the resource as you last read it (the etag of its latest result)."}
		return append(required, "if_match"), nil
	case in != "path" && in != "query":
		return required, nil // Idempotency-Key is set by the client
	}
	s := map[string]any{} // a parameter without a schema takes any value
	if raw, has := p["schema"]; has && raw != nil {
		v, err := c.schema(raw, at+".schema", 0)
		if err != nil {
			return nil, err
		}
		if s, has = v.(map[string]any); !has {
			return nil, malformed(at+".schema", "an object")
		}
	}
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
	return required, nil
}
