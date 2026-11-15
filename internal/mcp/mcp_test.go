package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/mcp"
)

const meta = `"_meta": {"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": {}}`

// api is an API stand-in: a session, its recorded tool calls (refused
// when unrecorded is set) and legal entities answered by status.
type api struct {
	calls      []string
	unrecorded bool
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/agent-sessions":
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id": "s1", "autonomy_level": 0}`))
	case strings.HasSuffix(r.URL.Path, "/tool-calls"):
		var call struct{ Tool, ResultSummary string }
		_ = json.NewDecoder(r.Body).Decode(&call)
		if a.unrecorded {
			w.WriteHeader(500)
			return
		}
		a.calls = append(a.calls, call.Tool)
		w.WriteHeader(201)
	case r.URL.Path == "/v1/legal-entities":
		a.entities(w, r.URL.Query().Get("status"))
	case r.URL.Path == "/v1/legal-entities/le1":
		w.Header().Set("ETag", `"3"`)
		_, _ = w.Write([]byte(`{"code": "3000", "id": "le1"}`))
	default:
		w.WriteHeader(404)
	}
}

// entities answers by the status asked: a list, no body, text, bytes or a
// problem.
func (a *api) entities(w http.ResponseWriter, status string) {
	switch status {
	case "empty":
		w.WriteHeader(204)
	case "text":
		_, _ = w.Write([]byte("<xml/>"))
	case "bytes":
		_, _ = w.Write([]byte{0xff, 0x00})
	case "refused":
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code": "identity.permission_missing", "status": 403}`))
	default:
		_, _ = w.Write([]byte(`{"items": [{"code": "3000"}]}`))
	}
}

// server is a server whose tools call the stand-in in a session.
func server(t *testing.T, a *api) *mcp.Server {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	c := &apitools.Client{BaseURL: srv.URL, Token: "t"}
	s, err := apitools.StartSession(context.Background(), c, "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := apitools.Tools(c, s, "getLegalEntity", "listLegalEntities")
	if err != nil {
		t.Fatal(err)
	}
	return &mcp.Server{Tools: tools, Name: "erp", Version: "test"}
}

// exchange sends the lines and returns each answer, decoded.
func exchange(t *testing.T, s *mcp.Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")), &out); err != nil {
		t.Fatal(err)
	}
	var answers []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("answer %q: %v", l, err)
		}
		answers = append(answers, m)
	}
	return answers
}

// one is the answer to one line.
func one(t *testing.T, s *mcp.Server, line string) map[string]any {
	t.Helper()
	got := exchange(t, s, line)
	if len(got) != 1 {
		t.Fatalf("%s: got %d answers %v, want one", line, len(got), got)
	}
	return got[0]
}

func errorCode(m map[string]any) any {
	e, _ := m["error"].(map[string]any)
	return e["code"]
}

// TestProtocolErrors (T102a): a message that is not a JSON-RPC request is
// answered with the protocol's error, a notification and a response from
// the host with nothing; nothing reaches the API.
func TestProtocolErrors(t *testing.T) {
	a := &api{}
	s := server(t, a)
	for name, c := range map[string]struct {
		line string
		code float64
	}{
		"not JSON":                {`{"jsonrpc": "2.0", "id": 1,`, -32700},
		"a batch":                 {`[{"jsonrpc": "2.0", "id": 1, "method": "ping"}]`, -32600},
		"a null id":               {`{"jsonrpc": "2.0", "id": null, "method": "tools/list"}`, -32600},
		"a fractional id":         {`{"jsonrpc": "2.0", "id": 1.5, "method": "tools/list"}`, -32600},
		"no jsonrpc":              {`{"id": 1, "method": "tools/list"}`, -32600},
		"neither method nor id":   {`{"jsonrpc": "2.0"}`, -32600},
		"params not an object":    {`{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": 5}`, -32602},
		"no _meta":                {`{"jsonrpc": "2.0", "id": "a", "method": "tools/list", "params": {}}`, -32602},
		"no params":               {`{"jsonrpc": "2.0", "id": "a", "method": "server/discover"}`, -32602},
		"no client capabilities":  {`{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {"_meta": {"io.modelcontextprotocol/protocolVersion": "2026-07-28"}}}`, -32602},
		"an unknown method":       {`{"jsonrpc": "2.0", "id": 1, "method": "resources/list", "params": {` + meta + `}}`, -32601},
		"ping, modern":            {`{"jsonrpc": "2.0", "id": 1, "method": "ping", "params": {` + meta + `}}`, -32601},
		"a cursor":                {`{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {"cursor": "x", ` + meta + `}}`, -32602},
		"initialize, no version":  {`{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}`, -32602},
		"a call without a name":   {`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {` + meta + `}}`, -32602},
		"an unknown tool":         {`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "createPrincipal", ` + meta + `}}`, -32602},
		"arguments not an object": {`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "listLegalEntities", "arguments": [1], ` + meta + `}}`, -32602},
		"a required argument":     {`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "getLegalEntity", "arguments": {}, ` + meta + `}}`, -32602},
	} {
		t.Run(name, func(t *testing.T) {
			if got := errorCode(one(t, s, c.line)); got != c.code {
				t.Errorf("error code: got %v, want %v", got, c.code)
			}
		})
	}
	if got := exchange(t, s, `{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": 1}}`,
		`{"jsonrpc": "2.0", "id": 7, "result": {}}`, "", "  "); len(got) != 0 {
		t.Errorf("notification, response, blank lines: got %v, want no answer", got)
	}
	if len(a.calls) != 0 {
		t.Errorf("calls recorded: got %v, want none", a.calls)
	}
}

// TestVersions (T102a): a request for a version not served is refused
// naming those served; 2026-07-28's discovery names them; 2025-11-25 is
// served after initialize, with ping, and its results carry no
// 2026-07-28 fields.
func TestVersions(t *testing.T) {
	s := server(t, &api{})
	refused := one(t, s, `{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {"_meta": {"io.modelcontextprotocol/protocolVersion": "2099-01-01"}}}`)
	e := refused["error"].(map[string]any)
	if b, _ := json.Marshal(e); e["code"] != float64(-32022) || !strings.Contains(string(b), `"data":{"requested":"2099-01-01","supported":["2026-07-28","2025-11-25"]}`) {
		t.Errorf("unsupported version: got %s", b)
	}
	d := one(t, s, `{"jsonrpc": "2.0", "id": 2, "method": "server/discover", "params": {`+meta+`}}`)["result"].(map[string]any)
	if b, _ := json.Marshal(d); !strings.Contains(string(b), `"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"erp","version":"test"}}`) ||
		!strings.Contains(string(b), `"cacheScope":"public","capabilities":{"tools":{}}`) || d["resultType"] != "complete" {
		t.Errorf("discover: got %s", b)
	}
	// Legacy: before initialize, ping is a request without _meta.
	if got := errorCode(one(t, s, `{"jsonrpc": "2.0", "id": 3, "method": "ping"}`)); got != float64(-32602) {
		t.Errorf("ping before initialize: got %v, want -32602", got)
	}
	got := exchange(t, s, `{"jsonrpc": "2.0", "id": 4, "method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "h", "version": "1"}}}`,
		`{"jsonrpc": "2.0", "method": "notifications/initialized"}`,
		`{"jsonrpc": "2.0", "id": 5, "method": "ping"}`,
		`{"jsonrpc": "2.0", "id": 6, "method": "tools/list"}`,
		`{"jsonrpc": "2.0", "id": 7, "method": "server/discover"}`,
		`{"jsonrpc": "2.0", "id": 8, "method": "tools/call", "params": {"name": "listLegalEntities"}}`)
	if len(got) != 5 {
		t.Fatalf("legacy exchange: got %v, want five answers", got)
	}
	init := got[0]["result"].(map[string]any)
	if init["protocolVersion"] != mcp.Legacy || init["instructions"] != mcp.Instructions {
		t.Errorf("initialize: got %v, want %s", init, mcp.Legacy)
	}
	if b, _ := json.Marshal(got[1]); string(b) != `{"id":5,"jsonrpc":"2.0","result":{}}` {
		t.Errorf("ping: got %s", b)
	}
	list := got[2]["result"].(map[string]any)
	if _, has := list["resultType"]; has || len(list["tools"].([]any)) != 2 {
		t.Errorf("legacy list: got %v, want two tools and no resultType", list)
	}
	if errorCode(got[3]) != float64(-32601) {
		t.Errorf("legacy discover: got %v, want -32601", got[3])
	}
	if call := got[4]["result"].(map[string]any); call["isError"] != false || call["resultType"] != nil {
		t.Errorf("legacy call: got %v", call)
	}
	// A 2026-07-28 request is still served as such.
	if r := one(t, s, `{"jsonrpc": "2.0", "id": 9, "method": "tools/list", "params": {`+meta+`}}`)["result"].(map[string]any); r["cacheScope"] != "private" {
		t.Errorf("modern list after initialize: got %v", r)
	}
}

// TestTools (T102a): the tools are listed with their schemas, in order;
// a call's result is the API's answer, problems included, and each call
// is recorded in the session.
func TestTools(t *testing.T) {
	a := &api{}
	s := server(t, a)
	list := one(t, s, `{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {`+meta+`}}`)["result"].(map[string]any)
	tools := list["tools"].([]any)
	first := tools[0].(map[string]any)
	if b, _ := json.Marshal(first); first["name"] != "getLegalEntity" || !strings.Contains(string(b), `"required":["id"]`) ||
		!strings.Contains(string(b), `"annotations":{"readOnlyHint":true}`) || !strings.Contains(string(b), `"type":"object"`) {
		t.Errorf("first tool: got %s", b)
	}
	if list["ttlMs"] != float64(0) || list["resultType"] != "complete" {
		t.Errorf("list: got %v", list)
	}
	callOf := func(name, args string) map[string]any {
		t.Helper()
		return one(t, s, `{"jsonrpc": "2.0", "id": "c", "method": "tools/call", "params": {"name": "`+name+`", "arguments": `+args+`, `+meta+`}}`)["result"].(map[string]any)
	}
	for name, c := range map[string]struct {
		tool, args, structured string
		isError                bool
	}{
		"JSON":        {"getLegalEntity", `{"id": "le1"}`, `{"body":{"code":"3000","id":"le1"},"etag":"\"3\"","status":200}`, false},
		"no body":     {"listLegalEntities", `{"status": "empty"}`, `{"body":null,"etag":"","status":204}`, false},
		"text":        {"listLegalEntities", `{"status": "text"}`, `{"body":"\u003cxml/\u003e","etag":"","status":200}`, false},
		"bytes":       {"listLegalEntities", `{"status": "bytes"}`, `{"body_base64":"/wA=","etag":"","status":200}`, false},
		"a problem":   {"listLegalEntities", `{"status": "refused"}`, `{"body":{"code":"identity.permission_missing","status":403},"etag":"","status":403}`, true},
		"null inputs": {"listLegalEntities", `null`, `{"body":{"items":[{"code":"3000"}]},"etag":"","status":200}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := callOf(c.tool, c.args)
			structured, _ := json.Marshal(r["structuredContent"])
			if string(structured) != c.structured || r["isError"] != c.isError {
				t.Errorf("structured: got %s %v, want %s %v", structured, r["isError"], c.structured, c.isError)
			}
			text := r["content"].([]any)[0].(map[string]any)["text"]
			if text != string(structured) {
				t.Errorf("text: got %v, want the structured content %s", text, structured)
			}
		})
	}
	slices.Sort(a.calls)
	if got := strings.Join(a.calls, " "); got != "getLegalEntity listLegalEntities listLegalEntities listLegalEntities listLegalEntities listLegalEntities" {
		t.Errorf("recorded calls: got %s", got)
	}
	// A call the API cannot be reached for is an error the model sees.
	tools2, _ := apitools.Tools(&apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}, nil, "listLegalEntities")
	down := &mcp.Server{Tools: tools2}
	r := one(t, down, `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "listLegalEntities", `+meta+`}}`)["result"].(map[string]any)
	if text := r["content"].([]any)[0].(map[string]any)["text"].(string); r["isError"] != true || !strings.Contains(text, "connect") {
		t.Errorf("unreachable API: got %v", r)
	}
	// A call the session does not record is the server's error.
	a.unrecorded = true
	if got := errorCode(one(t, s, `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "listLegalEntities", `+meta+`}}`)); got != float64(-32603) {
		t.Errorf("unrecorded call: got %v, want -32603", got)
	}
}

// TestServeFailures: a failed read or write ends serving with its error.
func TestServeFailures(t *testing.T) {
	s := server(t, &api{})
	errRead := errors.New("read failed")
	if err := s.Serve(context.Background(), iotest.ErrReader(errRead), &bytes.Buffer{}); !errors.Is(err, errRead) {
		t.Errorf("read: got %v, want its error", err)
	}
	if err := s.Serve(context.Background(), strings.NewReader("[]\n"), failing{}); err == nil || !strings.Contains(err.Error(), "full") {
		t.Errorf("write: got %v, want its error", err)
	}
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("disk full") }
