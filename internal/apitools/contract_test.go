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
        $ref: "#/components/requestBodies/missing"
      parameters:
        - name: q
          in: query
          schema:
            $ref: "#/components/schemas/Deep"
components:
  schemas:
    Deep:
      type: object
      properties:
        next:
          $ref: "#/components/schemas/Deep"
`))
	tool, err := c.tool("makeX")
	if err != nil || tool.op.body || len(tool.op.params) != 1 {
		t.Fatalf("%+v %v", tool, err)
	}
	if got, err := c.schema(map[string]any{"$ref": "#/nowhere/at/all"}, "x", 0); err != nil || len(got.(map[string]any)) != 0 {
		t.Fatalf("unresolvable reference: %v %v", got, err)
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
	if _, has := got["x-internal"]; has || len(got["properties"].(map[string]any)) != 1 || len(got["required"].([]any)) != 1 {
		t.Fatalf("read-only properties kept: %v", got)
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
	result := func() string {
		res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return res[0].OfText.Text
	}
	for _, b := range []string{"", "not json"} {
		if body = b; !strings.Contains(result(), `"body":null`) {
			t.Errorf("%q: %s", b, result())
		}
	}
	body = `["` + strings.Repeat("x", maxResult) + `"]`
	if got := result(); len(got) > maxResult+100 || !strings.HasSuffix(got, "ask for a smaller page)") {
		t.Errorf("not truncated: %d bytes", len(got))
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
		c, _ := load([]byte(op + "      parameters:\n        - {name: q, in: query, description: Search text.}\n"))
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

// Every operation of the embedded contract builds a tool.
func TestEveryOperationBuilds(t *testing.T) {
	c := embedded()
	paths, _ := c.spec["paths"].(map[string]any)
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
		}
	}
}
