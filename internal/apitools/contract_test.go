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
	if got := c.schema(map[string]any{"$ref": "#/nowhere/at/all"}, 0); len(got.(map[string]any)) != 0 {
		t.Fatalf("unresolvable reference: %v", got)
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
	got := c.schema(map[string]any{"type": "object", "x-internal": true, "required": []any{"id", "name", "at"}, "properties": map[string]any{
		"id":   map[string]any{"type": "string", "readOnly": true},
		"at":   map[string]any{"$ref": "#/components/schemas/Stamp"},
		"name": map[string]any{"type": "string"}}}, 0).(map[string]any)
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
