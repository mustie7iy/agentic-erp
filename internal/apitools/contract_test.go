package apitools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContractEdges(t *testing.T) {
	if _, err := load([]byte("paths: [")); err == nil {
		t.Fatal("broken contract parsed")
	}
	c, _ := load([]byte(`paths:
  /v1/x:
    post:
      operationId: makeX
      requestBody:
        $ref: "#/components/requestBodies/NoContent"
      parameters:
        - name: q
          in: query
          schema:
            $ref: "#/components/schemas/Deep"
components:
  requestBodies:
    NoContent:
      description: A body without content gives the tool no body.
  schemas:
    Deep:
      type: object
      properties:
        next:
          $ref: "#/components/schemas/Deep"
`))
	tool, err := c.tool("makeX")
	if err != nil {
		t.Fatalf("makeX: %v", err)
	}
	if tool.op.body {
		t.Errorf("body: got true, want false for a request body without content")
	}
	if got := len(tool.op.params); got != 1 {
		t.Errorf("params: got %d, want 1", got)
	}
	if got, err := c.schema(map[string]any{"$ref": "#/nowhere/at/all"}, "x", 0); err == nil {
		t.Fatalf("got %v for an unresolvable reference, want an error", got)
	}
}

func TestReadOnlyAndExtensions(t *testing.T) {
	c, _ := load([]byte(`paths: {}
components:
  schemas:
    Stamp:
      type: string
      readOnly: true
`))
	v, err := c.schema(map[string]any{"type": "object", "x-internal": true, "required": []any{"id", "name", "at"}, "properties": map[string]any{
		"id":   map[string]any{"type": "string", "readOnly": true},
		"at":   map[string]any{"$ref": "#/components/schemas/Stamp"},
		"name": map[string]any{"type": "string"}}}, "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := v.(map[string]any)
	if x, has := got["x-internal"]; has {
		t.Errorf("x-internal: got %v, want the extension dropped", x)
	}
	if n := len(got["properties"].(map[string]any)); n != 1 {
		t.Errorf("properties: got %d, want 1 (read-only ones dropped) in %v", n, got)
	}
	if n := len(got["required"].([]any)); n != 1 {
		t.Errorf("required: got %d, want 1 (read-only ones dropped) in %v", n, got)
	}
}

func TestAnswers(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == "cut" {
			w.Header().Set("Content-Length", "10")
			w.Write([]byte("{}"))
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := load([]byte(`paths:
  /v1/x:
    get:
      operationId: getX
`))
	tool, _ := c.tool("getX")
	tool.client = &Client{BaseURL: srv.URL}
	result := func(t *testing.T) string {
		t.Helper()
		res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return res[0].OfText.Text
	}
	for _, b := range []string{"", "not json"} {
		t.Run(b, func(t *testing.T) {
			body = b
			if got := result(t); !strings.Contains(got, `"body":null`) {
				t.Errorf("answer %q: got %s, want a null body", b, got)
			}
		})
	}
	body = `["` + strings.Repeat("x", maxResult) + `"]`
	got := result(t)
	if len(got) > maxResult+100 {
		t.Errorf("truncated length: got %d bytes, want at most %d", len(got), maxResult+100)
	}
	if !strings.HasSuffix(got, "ask for a smaller page)") {
		t.Errorf("truncated answer: got ...%s, want it to end asking for a smaller page", got[max(0, len(got)-40):])
	}
	body = "cut"
	if _, err := tool.client.Do(context.Background(), http.MethodGet, "/v1/x", nil, nil, ""); err == nil {
		t.Error("cut answer read")
	}
}

// A malformed contract is refused with an error naming where it is
// malformed, never a panic; a parameter without a schema gets an empty one.
func TestMalformedContract(t *testing.T) {
	const op = "paths:\n  /v1/x:\n    post:\n      operationId: makeX\n"
	cases := map[string]struct{ fragment, want string }{
		"parameter that is not an object": {"      parameters: [q]\n", "parameters[0]"},
		"parameter schema that is not an object": {
			"      parameters:\n        - {name: q, in: query, schema: 5}\n", "parameters[0].schema"},
		"open enumeration that is not a list": {
			"      parameters:\n        - {name: q, in: query, schema: {type: string, x-extensible-enum: a}}\n",
			"parameters[0].schema.x-extensible-enum"},
		"properties that are not an object": {
			"      requestBody:\n        content:\n          application/json:\n            schema: {type: object, properties: [a]}\n",
			"requestBody.content.application/json.schema.properties"},
		"malformed schema in a list": {
			"      parameters:\n        - {name: q, in: query, schema: {allOf: [{x-extensible-enum: a}]}}\n",
			"parameters[0].schema.allOf[0].x-extensible-enum"},
		"malformed property": {
			"      requestBody:\n        content:\n          application/json:\n            schema: {properties: {a: {x-extensible-enum: a}}}\n",
			"requestBody.content.application/json.schema.properties.a.x-extensible-enum"},
		"media type that is not an object": {
			"      requestBody:\n        content:\n          application/json: 5\n", "requestBody.content.application/json"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := load([]byte(op + tc.fragment))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.tool("makeX")
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if want := "paths./v1/x.post." + tc.want; !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want it to name %s", err, want)
			}
		})
	}
	t.Run("parameter with a description and no schema", func(t *testing.T) {
		c, err := load([]byte(op + "      parameters:\n        - {name: q, in: query, description: Search text.}\n"))
		if err != nil {
			t.Fatal(err)
		}
		tool, err := c.tool("makeX")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(tool.schema)
		if want := `"q":{"description":"Search text."}`; !strings.Contains(string(b), want) {
			t.Errorf("got %s, want it to contain %s", b, want)
		}
	})
}

// A reference cycle is a malformed contract, refused with the path where it
// was met instead of being followed forever.
func TestReferenceCycle(t *testing.T) {
	const cycle = "components:\n  schemas:\n    A: {$ref: '#/components/schemas/B'}\n    B: {$ref: '#/components/schemas/A'}\n" +
		"  parameters:\n    P: {$ref: '#/components/parameters/P'}\n  requestBodies:\n    R: {$ref: '#/components/requestBodies/R'}\n" +
		"paths:\n  /v1/x:\n    post:\n      operationId: makeX\n"
	cases := map[string]struct{ fragment, want string }{
		"schema": {"      parameters:\n        - {name: q, in: query, schema: {$ref: '#/components/schemas/A'}}\n",
			"paths./v1/x.post.parameters[0].schema"},
		"property": {"      requestBody:\n        content:\n          application/json:\n            schema: {properties: {a: {$ref: '#/components/schemas/A'}}}\n",
			"paths./v1/x.post.requestBody.content.application/json.schema.properties.a"},
		"parameter":    {"      parameters:\n        - {$ref: '#/components/parameters/P'}\n", "paths./v1/x.post.parameters[0]"},
		"request body": {"      requestBody: {$ref: '#/components/requestBodies/R'}\n", "paths./v1/x.post.requestBody"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := load([]byte(cycle + tc.fragment))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.tool("makeX")
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if !strings.Contains(err.Error(), "contract "+tc.want+" ") || !strings.Contains(err.Error(), "resolvable reference") {
				t.Errorf("got %v, want an unresolvable reference at %s", err, tc.want)
			}
		})
	}
}

// A reference to nothing is a malformed contract, refused with the path
// where it was met instead of standing in for an empty schema.
func TestDanglingReference(t *testing.T) {
	const op = "components:\n  schemas:\n    Leaf: {type: string}\npaths:\n  /v1/x:\n    post:\n      operationId: makeX\n"
	cases := map[string]struct{ fragment, want string }{
		"schema": {"      parameters:\n        - {name: q, in: query, schema: {$ref: '#/components/schemas/Missing'}}\n",
			"paths./v1/x.post.parameters[0].schema"},
		"through a node that is not an object": {"      parameters:\n        - {name: q, in: query, schema: {$ref: '#/components/schemas/Leaf/type/x'}}\n",
			"paths./v1/x.post.parameters[0].schema"},
		"property": {"      requestBody:\n        content:\n          application/json:\n            schema: {properties: {a: {$ref: '#/nowhere'}}}\n",
			"paths./v1/x.post.requestBody.content.application/json.schema.properties.a"},
		"parameter":    {"      parameters:\n        - {$ref: '#/components/parameters/Missing'}\n", "paths./v1/x.post.parameters[0]"},
		"request body": {"      requestBody: {$ref: '#/components/requestBodies/Missing'}\n", "paths./v1/x.post.requestBody"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := load([]byte(op + tc.fragment))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.tool("makeX")
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if !strings.Contains(err.Error(), "contract "+tc.want+" ") || !strings.Contains(err.Error(), "points nowhere") {
				t.Errorf("got %v, want a reference that points nowhere at %s", err, tc.want)
			}
		})
	}
}

// Every operation of the embedded contract builds a tool.
func TestEveryOperationBuilds(t *testing.T) {
	c := embedded()
	paths, _ := c.spec["paths"].(map[string]any)
	built := 0
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, v := range methods {
			op, _ := v.(map[string]any)
			id, ok := op["operationId"].(string)
			if !ok {
				continue
			}
			if _, err := c.operation(id, true); err != nil {
				t.Errorf("%s %s: got %v, want no error", method, path, err)
			}
			built++
		}
	}
	// The contract has well over a hundred operations; none would mean the
	// loop above checked nothing.
	if built < 100 {
		t.Errorf("got %d operations, want the contract's (over 100)", built)
	}
}
