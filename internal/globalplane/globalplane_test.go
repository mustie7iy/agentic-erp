package globalplane_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/globalplane"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// plane is the global control plane on a listener, over a fresh directory.
type plane struct {
	t   *testing.T
	dir *directory.Postgres
	is  *authntest.Issuer
	srv *httptest.Server
	s   *globalplane.Service
}

func newPlane(t *testing.T) *plane {
	t.Helper()
	ctx := context.Background()
	dir, err := directory.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dir.Close)
	is := authntest.New(t)
	v, err := authn.New(ctx, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	s := &globalplane.Service{Directory: dir, Pages: pages}
	srv := httptest.NewServer(globalplane.Handler(s, v, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return &plane{t: t, dir: dir, is: is, srv: srv, s: s}
}

func (p *plane) operator(subject string) string {
	return p.is.Token(p.t, uuid.Nil, subject, map[string]any{"tenant_id": nil})
}

func (p *plane) do(method, path, body string, headers map[string]string) (int, map[string]any, http.Header) {
	p.t.Helper()
	req, _ := http.NewRequest(method, p.srv.URL+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			p.t.Fatalf("%s %s: %d, not JSON: %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, m, resp.Header
}

func (p *plane) as(subject, method, path, body string, extra ...string) (int, map[string]any, http.Header) {
	p.t.Helper()
	h := map[string]string{"Authorization": "Bearer " + p.operator(subject), "Content-Type": "application/json"}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return p.do(method, path, body, h)
}

func (p *plane) must(code int, subject, method, path, body string, extra ...string) (map[string]any, http.Header) {
	p.t.Helper()
	got, m, h := p.as(subject, method, path, body, extra...)
	if got != code {
		p.t.Fatalf("%s %s: %d %v, want %d", method, path, got, m, code)
	}
	return m, h
}

func (p *plane) problem(code int, problemCode, subject, method, path, body string, extra ...string) {
	p.t.Helper()
	got, m, _ := p.as(subject, method, path, body, extra...)
	if got != code || m["code"] != problemCode {
		p.t.Fatalf("%s %s: %d %v, want %d %s", method, path, got, m, code, problemCode)
	}
}

// fakeCell is a cell's control plane: it provisions whatever it is asked and
// remembers the requests.
type fakeCell struct {
	mu   sync.Mutex
	seen []string
	fail int
}

func (c *fakeCell) handler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.seen = append(c.seen, r.Method+" "+r.URL.Path+" key="+r.Header.Get("Idempotency-Key")+" auth="+strconv.FormatBool(strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))+" body="+string(body))
		fail := c.fail
		c.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(fail)
			w.Write([]byte(`{"code":"tenancy.invalid_tenant","detail":"The tenant request is invalid."}`))
			return
		}
		var in map[string]any
		json.Unmarshal(body, &in)
		w.Header().Set("ETag", `"1"`)
		w.Header().Set("Location", "/control/v1/tenants/"+uuid.NewString())
		w.WriteHeader(http.StatusCreated)
		id := uuid.Must(uuid.NewV7())
		json.NewEncoder(w).Encode(map[string]any{"id": id, "name": in["name"], "region": in["region"], "status": "active", "version": 1, "provisioned": map[string]any{"configuration_version": 1}})
	})
}

func TestGlobalControlPlane(t *testing.T) {
	p := newPlane(t)
	ctx := context.Background()
	eu1, eu2 := &fakeCell{}, &fakeCell{}
	eu1Srv, eu2Srv := httptest.NewServer(eu1.handler("eu-1")), httptest.NewServer(eu2.handler("eu-2"))
	defer eu1Srv.Close()
	defer eu2Srv.Close()
	region := "gp" + strings.ToLower(uuid.NewString()[:6]) // a region of this test's own
	for _, c := range []directory.Cell{
		{Name: region + "-1", Region: region, APIURL: "https://1.api.example.com", ControlURL: eu1Srv.URL, Accepting: true},
		{Name: region + "-2", Region: region, APIURL: "https://2.api.example.com", ControlURL: eu2Srv.URL, Accepting: true},
		{Name: region + "-full", Region: region, APIURL: "https://3.api.example.com", ControlURL: "http://127.0.0.1:1", Accepting: false},
	} {
		if err := p.dir.UpsertCell(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// Cells, with and without a region.
	cells, _ := p.must(200, "ops|alice", "GET", "/directory/v1/cells?region="+region, "")
	if items := cells["items"].([]any); len(items) != 3 || items[0].(map[string]any)["tenants"] != float64(0) {
		t.Fatalf("cells: %v", cells)
	}
	if all, _ := p.must(200, "ops|alice", "GET", "/directory/v1/cells", ""); len(all["items"].([]any)) < 3 {
		t.Fatalf("all cells: %v", all)
	}
	// Provisioning: placed in the emptiest cell, forwarded with the key and the token, entered in the directory at once.
	body := `{"name":"Acme","region":"` + region + `","group_currency":"EUR","administrator":{"subject":"idp|ada","display_name":"Ada"},"configuration":{"group_currency":"EUR","companies":[]}}`
	created, h := p.must(201, "ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", "key-"+uuid.NewString())
	if created["cell"] != region+"-1" || h.Get("X-Erp-Cell") != region+"-1" || h.Get("ETag") != `"1"` || h.Get("Location") == "" || created["name"] != "Acme" {
		t.Fatalf("created: %v %v", created, h)
	}
	id := uuid.MustParse(created["id"].(string))
	entry, h := p.must(200, "ops|alice", "GET", "/directory/v1/tenants/"+id.String(), "")
	if entry["cell"] != region+"-1" || entry["status"] != "active" || entry["api_url"] != "https://1.api.example.com" || h.Get("ETag") != `"1"` {
		t.Fatalf("entry: %v", entry)
	}
	eu1.mu.Lock()
	if len(eu1.seen) != 1 || !strings.Contains(eu1.seen[0], "POST /control/v1/tenants key=key-") || !strings.Contains(eu1.seen[0], "auth=true") || strings.Contains(eu1.seen[0], `"cell"`) {
		t.Fatalf("cell saw: %v", eu1.seen)
	}
	eu1.mu.Unlock()
	// The second tenant goes to the other cell; a named cell wins; the full cell is refused; a retry keeps its cell.
	second, _ := p.must(201, "ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", "key-"+uuid.NewString())
	if second["cell"] != region+"-2" {
		t.Fatalf("second: %v", second)
	}
	named, _ := p.must(201, "ops|alice", "POST", "/directory/v1/tenants", strings.Replace(body, `"name":"Acme"`, `"name":"Named","cell":"`+region+`-1"`, 1), "Idempotency-Key", "key-"+uuid.NewString())
	if named["cell"] != region+"-1" {
		t.Fatalf("named: %v", named)
	}
	p.problem(422, "tenancy.unknown_cell", "ops|alice", "POST", "/directory/v1/tenants", strings.Replace(body, `"name":"Acme"`, `"name":"Full","cell":"`+region+`-full"`, 1), "Idempotency-Key", "key-"+uuid.NewString())
	p.problem(422, "tenancy.no_cell", "ops|alice", "POST", "/directory/v1/tenants", strings.Replace(body, region, "xx-nowhere", 1), "Idempotency-Key", "key-"+uuid.NewString())
	retryKey := "key-" + uuid.NewString()
	eu2.mu.Lock()
	eu2.fail = http.StatusInternalServerError
	eu2.mu.Unlock()
	// The emptiest cell is now eu-2 (one tenant each; name order breaks the tie) ... eu-1 has two, so eu-2 answers: failing.
	status, m, _ := p.as("ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", retryKey)
	if status != 500 || m["code"] != "tenancy.invalid_tenant" {
		t.Fatalf("the cell's refusal passes through: %d %v", status, m)
	}
	eu2.mu.Lock()
	eu2.fail = 0
	eu2.mu.Unlock()
	retried, _ := p.must(201, "ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", retryKey)
	if retried["cell"] != region+"-2" {
		t.Fatalf("retry: %v", retried)
	}
	// A cell that does not answer is a 502; the placement is kept for the retry.
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-down", Region: region, APIURL: "https://d.api.example.com", ControlURL: "http://127.0.0.1:1", Accepting: true})
	p.problem(502, "platform.cell_unreachable", "ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", "key-"+uuid.NewString())
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-bad", Region: region, APIURL: "https://b.api.example.com", ControlURL: "::", Accepting: true})
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-down", Region: region, APIURL: "https://d.api.example.com", ControlURL: "http://127.0.0.1:1", Accepting: false})
	if status, _, _ := p.as("ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", "key-"+uuid.NewString()); status != 503 {
		t.Fatalf("cell with a bad control URL: %d", status)
	}
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-bad", Region: region, APIURL: "https://b.api.example.com", ControlURL: "::", Accepting: false})
	// A cell whose answer breaks off is unreachable too.
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":`))
	}))
	defer truncated.Close()
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-cut", Region: region, APIURL: "https://c.api.example.com", ControlURL: truncated.URL, Accepting: true})
	p.problem(502, "platform.cell_unreachable", "ops|alice", "POST", "/directory/v1/tenants", strings.Replace(body, `"name":"Acme"`, `"name":"Cut","cell":"`+region+`-cut"`, 1), "Idempotency-Key", "key-"+uuid.NewString())
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-cut", Region: region, APIURL: "https://c.api.example.com", ControlURL: truncated.URL, Accepting: false})
	// Request rules: key, body, region, size.
	if status, _, _ := p.as("ops|alice", "POST", "/directory/v1/tenants", `{"region":"`+region+`","name":"`+strings.Repeat("x", 5<<20)+`"}`, "Idempotency-Key", "key-"+uuid.NewString()); status != 413 {
		t.Fatalf("oversized: %d", status)
	}
	p.problem(428, "request.precondition_required", "ops|alice", "POST", "/directory/v1/tenants", body)
	p.problem(400, "request.invalid_idempotency_key", "ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", "short")
	p.problem(400, "request.invalid_body", "ops|alice", "POST", "/directory/v1/tenants", `{"name":"x"}`, "Idempotency-Key", "key-"+uuid.NewString())
	p.problem(400, "request.invalid_body", "ops|alice", "POST", "/directory/v1/tenants", `not json`, "Idempotency-Key", "key-"+uuid.NewString())
	// A move: freeze, move, unfreeze, each under If-Match; the wrong state and region refused.
	p.problem(428, "request.precondition_required", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":freeze", "")
	p.problem(412, "request.version_mismatch", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":freeze", "", "If-Match", `"9"`)
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":unfreeze", "", "If-Match", `"1"`)
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":move", `{"cell":"`+region+`-2"}`, "If-Match", `"1"`)
	frozen, h := p.must(200, "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":freeze", "", "If-Match", `"1"`)
	if frozen["status"] != "read_only" || h.Get("ETag") != `"2"` {
		t.Fatalf("frozen: %v", frozen)
	}
	p.problem(409, "tenancy.invalid_transition", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":freeze", "", "If-Match", `"2"`)
	p.problem(400, "request.invalid_body", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":move", `{}`, "If-Match", `"2"`)
	p.problem(422, "tenancy.unknown_cell", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":move", `{"cell":"nowhere"}`, "If-Match", `"2"`)
	p.dir.UpsertCell(ctx, directory.Cell{Name: region + "-us", Region: "us-east", APIURL: "https://us.api.example.com", ControlURL: "http://us:9090", Accepting: true})
	p.problem(422, "tenancy.region_mismatch", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":move", `{"cell":"`+region+`-us"}`, "If-Match", `"2"`)
	moved, h := p.must(200, "ops|bob", "POST", "/directory/v1/tenants/"+id.String()+":move", `{"cell":"`+region+`-2"}`, "If-Match", `"2"`)
	if moved["cell"] != region+"-2" || moved["api_url"] != "https://2.api.example.com" || moved["status"] != "read_only" || h.Get("ETag") != `"3"` {
		t.Fatalf("moved: %v", moved)
	}
	active, _ := p.must(200, "ops|bob", "POST", "/directory/v1/tenants/"+id.String()+":unfreeze", "", "If-Match", `"3"`)
	if active["status"] != "active" || active["cell"] != region+"-2" {
		t.Fatalf("unfrozen: %v", active)
	}
	// Listing: filters, pages, order; unknown tenants and methods; tokens.
	page, _ := p.must(200, "ops|alice", "GET", "/directory/v1/tenants?region="+region+"&page_size=2", "")
	if len(page["items"].([]any)) != 2 || page["next_page_token"] == nil {
		t.Fatalf("page: %v", page)
	}
	rest, _ := p.must(200, "ops|alice", "GET", "/directory/v1/tenants?region="+region+"&page_size=2&page_token="+page["next_page_token"].(string), "")
	if len(rest["items"].([]any)) < 1 {
		t.Fatalf("rest: %v", rest)
	}
	if byCell, _ := p.must(200, "ops|alice", "GET", "/directory/v1/tenants?cell="+region+"-2&order_by=cell%20asc", ""); len(byCell["items"].([]any)) != 3 {
		t.Fatalf("by cell: %v", byCell)
	}
	if none, _ := p.must(200, "ops|alice", "GET", "/directory/v1/tenants?status=offboarded&region="+region, ""); len(none["items"].([]any)) != 0 {
		t.Fatalf("none: %v", none)
	}
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", "/directory/v1/tenants?status=bogus", "")
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", "/directory/v1/tenants?page_size=many", "")
	p.problem(400, "request.invalid_order_by", "ops|alice", "GET", "/directory/v1/tenants?order_by=name", "")
	p.problem(404, "request.not_found", "ops|alice", "GET", "/directory/v1/tenants/"+uuid.NewString(), "")
	p.problem(404, "request.not_found", "ops|alice", "POST", "/directory/v1/tenants/"+uuid.NewString()+":freeze", "", "If-Match", "*")
	p.problem(404, "request.not_found", "ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":explode", "", "If-Match", "*")
	p.problem(404, "request.not_found", "ops|alice", "GET", "/directory/v1/nothing", "")
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", "/directory/v1/tenants/not-a-uuid", "")
	for path, allow := range map[string]string{"/directory/v1/tenants": "GET, POST", "/directory/v1/cells": "GET", "/directory/v1/tenants/" + id.String(): "GET", "/directory/v1/tenants/" + id.String() + ":freeze": "POST"} {
		method := "DELETE"
		if allow == "POST" {
			method = "GET"
		}
		status, _, h := p.as("ops|alice", method, path, "")
		if status != 405 || h.Get("Allow") != allow {
			t.Errorf("%s %s: %d allow %q", method, path, status, h.Get("Allow"))
		}
	}
	if status, _, _ := p.do("GET", "/directory/v1/cells", "", nil); status != 401 {
		t.Fatalf("no token: %d", status)
	}
	if status, _, _ := p.do("GET", "/directory/v1/cells", "", map[string]string{"Authorization": "Bearer " + p.is.Token(t, uuid.New(), "ada", nil)}); status != 401 {
		t.Fatalf("tenant token: %d", status)
	}
	// Operator roles: readers read, provisioners provision, operators move.
	withRoles := func(roles any) map[string]string {
		return map[string]string{"Authorization": "Bearer " + p.is.Token(t, uuid.Nil, "ops|roled", map[string]any{"tenant_id": nil, "roles": roles}), "Content-Type": "application/json", "If-Match": "*", "Idempotency-Key": "key-" + uuid.NewString()}
	}
	for _, c := range []struct {
		name   string
		roles  any
		method string
		path   string
		code   int
	}{
		{"reader lists cells", []any{"erp:read"}, "GET", "/directory/v1/cells", 200},
		{"reader lists tenants", []any{"erp:read"}, "GET", "/directory/v1/tenants?region=" + region, 200},
		{"reader freezes", []any{"erp:read"}, "POST", "/directory/v1/tenants/" + id.String() + ":freeze", 403},
		{"reader provisions", []any{"erp:read"}, "POST", "/directory/v1/tenants", 403},
		{"provisioner provisions", []any{"erp:provision"}, "POST", "/directory/v1/tenants", 400},
		{"operator freezes", "erp:operate", "POST", "/directory/v1/tenants/" + id.String() + ":freeze", 200},
		{"admin unfreezes", []any{"erp:admin"}, "POST", "/directory/v1/tenants/" + id.String() + ":unfreeze", 200},
	} {
		status, m, _ := p.do(c.method, c.path, "", withRoles(c.roles))
		if status != c.code || (status == 403 && m["code"] != "authz.operator_role") {
			t.Errorf("%s: %d %v", c.name, status, m)
		}
	}
	// Every operation of the contract is served.
	ops, err := api.DirectoryOperations()
	if err != nil {
		t.Fatal(err)
	}
	for id, op := range ops {
		path := strings.NewReplacer("{id}", uuid.NewString()).Replace(op.Path)
		status, _, _ := p.as("ops|alice", op.Method, path, `{"cell":"x","name":"x","region":"eu-west"}`, "If-Match", "*", "Idempotency-Key", "key-"+uuid.NewString())
		if status == 405 || status == 404 && !strings.Contains(op.Path, "{id}") {
			t.Errorf("%s: %d", id, status)
		}
	}
	// The directory failing.
	p.dir.Close()
	for _, path := range []string{"/directory/v1/tenants", "/directory/v1/cells", "/directory/v1/tenants/" + id.String()} {
		if status, _, _ := p.as("ops|alice", "GET", path, ""); status != 500 {
			t.Errorf("%s with the directory down: %d", path, status)
		}
	}
	if status, _, _ := p.as("ops|alice", "POST", "/directory/v1/tenants/"+id.String()+":move", `{"cell":"x"}`, "If-Match", "*"); status != 500 {
		t.Errorf("move with the directory down: %d", status)
	}
	if status, _, _ := p.as("ops|alice", "POST", "/directory/v1/tenants", body, "Idempotency-Key", "key-"+uuid.NewString()); status != 500 {
		t.Errorf("create with the directory down: %d", status)
	}
}
