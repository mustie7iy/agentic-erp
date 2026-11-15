package apitools_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// TestLocal: a local tool is answered by its own code, guarded and
// observed as an API call; its error, and an answer that does not encode,
// refuse the call.
func TestLocal(t *testing.T) {
	errNo := errors.New("no")
	echo := apitools.Local(nil, "echo", "Echoes.", map[string]any{"x": map[string]any{"type": "string"}}, []string{"x"},
		func(_ context.Context, in map[string]any) (any, error) {
			if in["x"] == "fail" {
				return nil, errNo
			}
			if in["x"] == "func" {
				return func() {}, nil
			}
			return in, nil
		})
	echo.Guard(func(in map[string]any) error {
		if in["x"] == "guarded" {
			return errNo
		}
		return nil
	})
	var seen int
	echo.Observe(func(r apitools.Response) { seen = r.Status })
	if got := call(t, echo, `{"x": "hello"}`); got["status"] != float64(200) || got["body"].(map[string]any)["x"] != "hello" || seen != 200 {
		t.Errorf("answer: got %v (observed %d), want hello with 200", got, seen)
	}
	if echo.Name() != "echo" || echo.Description() != "Echoes." {
		t.Errorf("tool: got %s %q, want echo %q", echo.Name(), echo.Description(), "Echoes.")
	}
	for in, want := range map[string]string{"guarded": "no", "fail": "no", "func": "encode the answer"} {
		t.Run(in, func(t *testing.T) {
			if _, err := echo.Execute(context.Background(), json.RawMessage(`{"x": "`+in+`"}`)); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want an error saying %q", err, want)
			}
		})
	}
}

// TestContract: findOperations answers the operations holding every word
// asked, or those named, in the order of their ids, with their method,
// path, permission and problem codes, operations closed to agents
// included and marked so; at most 20; nothing asked finds nothing.
func TestContract(t *testing.T) {
	c := apitools.Contract(nil)
	items := func(input string) []apitools.Described {
		t.Helper()
		var page struct{ Items []apitools.Described }
		b, _ := json.Marshal(call(t, c, input)["body"]) // decoded from JSON
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	found := items(`{"query": "Release Supplier Invoice hold"}`)
	if len(found) != 1 || found[0].ID != "releaseSupplierInvoice" || found[0].Method != http.MethodPost ||
		found[0].Path != "/v1/supplier-invoices/{id}:release" || found[0].Permission != "payables.invoice.review" ||
		!strings.Contains(strings.Join(found[0].ProblemCodes, " "), "payables.invalid_status") || found[0].PeopleOnly {
		t.Errorf("by words: got %+v, want releaseSupplierInvoice described", found)
	}
	named := items(`{"operation_ids": ["activateChangeRequest", "getRecommendation"]}`)
	if len(named) != 2 || named[0].ID != "activateChangeRequest" || !named[0].PeopleOnly || named[1].ID != "getRecommendation" {
		t.Errorf("by ids: got %+v, want activateChangeRequest (people only) then getRecommendation", named)
	}
	if n := len(items(`{"query": "list"}`)); n != 20 {
		t.Errorf("a common word: got %d operations, want 20", n)
	}
	if n := len(items(`{}`)); n != 0 {
		t.Errorf("nothing asked: got %d operations, want none", n)
	}
}

// TestActiveConfig: the active version is read as the caller may read
// it; none active, or an answer that is no list, is an error.
func TestActiveConfig(t *testing.T) {
	answer := `{"items": [{"id": "v1", "number": 3, "document": {"companies": []}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("is_active") != "true" {
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = w.Write([]byte(answer)) // a test's answer
	}))
	t.Cleanup(srv.Close)
	c := &apitools.Client{BaseURL: srv.URL, Token: "t"}
	if v, err := apitools.ActiveConfig(context.Background(), c); err != nil || v.ID != "v1" || v.Number != 3 || string(v.Document) != `{"companies": []}` {
		t.Errorf("active: got %+v, %v, want version 3", v, err)
	}
	answer = `{"items": []}`
	if _, err := apitools.ActiveConfig(context.Background(), c); err == nil || !strings.Contains(err.Error(), "no active version") {
		t.Errorf("none active: got %v, want no active version", err)
	}
}
