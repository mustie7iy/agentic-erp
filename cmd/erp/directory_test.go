package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

func TestDirectoryCommand(t *testing.T) {
	global, seen := directoryPlane(t)
	defer global.Close()
	env := map[string]string{"ERP_DIRECTORY_API_URL": global.URL, "ERP_OPERATOR_TOKEN": "op"}
	getenv = func(k string) string { return env[k] }
	defer func() { getenv = os.Getenv }()
	doc, _ := fixture.Document(t).Marshal()
	file := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, file, doc)
	s := &directoryCase{env: env, file: file, seen: seen}
	// Each step builds on the ones before it, so the first failure stops the rest.
	if !t.Run("reads and changes", s.readsAndChanges) {
		t.FailNow()
	}
	if !t.Run("defaults", s.defaults) {
		t.FailNow()
	}
	if !t.Run("lost output", s.lostOutput) {
		t.FailNow()
	}
	if !t.Run("refusals", s.refusals) {
		t.FailNow()
	}
	if !t.Run("token and connection", s.tokenAndConnection) {
		t.FailNow()
	}
}

// directoryPlane is a global control plane answering the directory
// subcommands, and the requests it saw so far.
func directoryPlane(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" if-match="+r.Header.Get("If-Match"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer op" {
			w.WriteHeader(401)
			w.Write([]byte(`{"code": "authn.invalid_token", "detail": "A bearer token is required."}`))
			return
		}
		w.Header().Set("ETag", `"2"`)
		switch {
		case r.URL.Path == "/directory/v1/cells":
			w.Write([]byte(`{"items": [{"name": "eu-1", "region": "eu-west", "tenants": 3}]}`))
		case r.URL.Path == "/directory/v1/tenants" && r.Method == "GET":
			w.Write([]byte(`{"items": [{"tenant_id": "t1", "cell": "eu-1"}], "next_page_token": "n"}`))
		case r.URL.Path == "/directory/v1/tenants" && r.Method == "POST":
			var in map[string]any
			if !decodeRequest(t, w, r, &in) {
				return
			}
			if r.Header.Get("Idempotency-Key") == "replayed-key" {
				w.Header().Set("Idempotent-Replayed", "true")
			}
			cell, _ := in["cell"].(string)
			if cell == "" {
				cell = "eu-2"
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"id": "t2", "name": "` + in["name"].(string) + `", "status": "active", "cell": "` + cell + `"}`))
		case r.URL.Path == "/directory/v1/tenants/t1":
			w.Write([]byte(`{"tenant_id": "t1", "cell": "eu-1", "status": "active", "version": 2}`))
		case r.URL.Path == "/directory/v1/tenants/t1:freeze" && r.Header.Get("If-Match") == `"2"`:
			w.Write([]byte(`{"tenant_id": "t1", "status": "read_only", "version": 3}`))
		case r.URL.Path == "/directory/v1/tenants/t1:move" && r.Header.Get("If-Match") == `"2"`:
			var in map[string]string
			if !decodeRequest(t, w, r, &in) {
				return
			}
			w.Write([]byte(`{"tenant_id": "t1", "status": "read_only", "cell": "` + in["cell"] + `", "version": 4}`))
		case r.URL.Path == "/directory/v1/tenants/t1:verify":
			var in map[string]string
			if !decodeRequest(t, w, r, &in) {
				return
			}
			equal := in["cell"] == "eu-2"
			w.Write([]byte(`{"tenant_id": "t1", "source": {"cell": "eu-1"}, "destination": {"cell": "` + in["cell"] + `"}, "equal": ` + strconv.FormatBool(equal) + `, "differences": ["ledger.journal_line: 120 rows in eu-1, 118 in ` + in["cell"] + `"]}`))
		case r.URL.Path == "/directory/v1/tenants/t1:unfreeze":
			w.WriteHeader(409)
			w.Write([]byte(`{"code": "tenancy.invalid_transition", "detail": "The tenant is active; unfreeze applies to read-only tenants."}`))
		case r.URL.Path == "/directory/v1/tenants/plain":
			w.Write([]byte(`not json`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"code": "request.not_found", "detail": "No cell serves this tenant."}`))
		}
	}))
	return global, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(seen, "\n")
	}
}

// directoryCase is the state the steps of TestDirectoryCommand share.
type directoryCase struct {
	env  map[string]string // what getenv answers
	file string            // a configuration document
	seen func() string     // the requests the plane saw
}

// readsAndChanges: each subcommand calls the global plane as the operator, with the request it names.
func (s *directoryCase) readsAndChanges(t *testing.T) {
	file := s.file
	if got := must(t, "directory", "cells", "-region", "eu-west"); !strings.Contains(got, `"tenants": 3`) {
		t.Fatalf("cells: %s", got)
	}
	if got := must(t, "directory", "cells"); !strings.Contains(got, `"eu-1"`) {
		t.Fatalf("all cells: %s", got)
	}
	if got := must(t, "directory", "tenants", "-region", "eu-west", "-cell", "eu-1", "-status", "active", "-order-by", "cell asc", "-page-size", "1", "-page-token", "n"); !strings.Contains(got, `"next_page_token": "n"`) {
		t.Fatalf("tenants: %s", got)
	}
	if got := must(t, "directory", "locate", "-id", "t1"); !strings.Contains(got, `"etag": "\"2\""`) {
		t.Fatalf("locate: %s", got)
	}
	if got := must(t, "directory", "create", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "s", "-admin-name", "Ada", "-admin-email", "ada@example.com", "-config", file); !strings.Contains(got, `"cell": "eu-2"`) {
		t.Fatalf("create: %s", got)
	}
	got := must(t, "directory", "create", "-name", "Beta", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "s", "-admin-name", "Bea", "-cell", "eu-1", "-config", file, "-idempotency-key", "replayed-key")
	for _, want := range []string{`"cell": "eu-1"`, "replayed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("create in a cell, replayed: got %s, want it to contain %s", got, want)
		}
	}
	if got := must(t, "directory", "freeze", "-id", "t1"); !strings.Contains(got, `"read_only"`) {
		t.Fatalf("freeze: %s", got)
	}
	if got := must(t, "directory", "move", "-id", "t1", "-cell", "eu-3", "-if-match", `"2"`); !strings.Contains(got, `"eu-3"`) {
		t.Fatalf("move: %s", got)
	}
	joined := s.seen()
	for _, want := range []string{"GET /directory/v1/cells?region=eu-west", "GET /directory/v1/tenants?cell=eu-1&order_by=cell+asc&page_size=1&page_token=n&region=eu-west&status=active",
		"GET /directory/v1/tenants/t1 if-match=\nPOST /directory/v1/tenants/t1:freeze if-match=\"2\"", "POST /directory/v1/tenants/t1:move if-match=\"2\""} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests: got\n%s\nwant them to contain %q", joined, want)
		}
	}
}

// defaults: without filters, without an If-Match (the ETag is read first), and verify.
func (s *directoryCase) defaults(t *testing.T) {
	if got := must(t, "directory", "tenants"); !strings.Contains(got, `"t1"`) {
		t.Fatalf("all tenants: %s", got)
	}
	if got := must(t, "directory", "move", "-id", "t1", "-cell", "eu-4"); !strings.Contains(got, `"eu-4"`) { // the ETag read first
		t.Fatalf("move reading the etag: %s", got)
	}
	if got := must(t, "directory", "verify", "-id", "t1", "-cell", "eu-2"); !strings.Contains(got, `"equal": true`) {
		t.Fatalf("verify: %s", got)
	}
}

// lostOutput: a failed print fails each subcommand.
func (s *directoryCase) lostOutput(t *testing.T) {
	file := s.file
	for name, args := range map[string][]string{
		"cells":   {"cells"},
		"tenants": {"tenants"},
		"locate":  {"locate", "-id", "t1"},
		"create":  {"create", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "s", "-admin-name", "Ada", "-config", file},
		"freeze":  {"freeze", "-id", "t1"},
		"move":    {"move", "-id", "t1", "-cell", "eu-4"},
		"verify":  {"verify", "-id", "t1", "-cell", "eu-2"},
	} {
		t.Run(name+" with lost output", func(t *testing.T) { printFails(t, append([]string{"directory"}, args...)...) })
	}
}

// refusals: what the plane refuses and what the command refuses itself.
func (s *directoryCase) refusals(t *testing.T) {
	fails(t, "the cells differ", "directory", "verify", "-id", "t1", "-cell", "eu-3")
	fails(t, "404 request.not_found", "directory", "verify", "-id", "t9", "-cell", "eu-2")
	fails(t, "-id and -cell are required", "directory", "verify", "-id", "t1")
	fails(t, "404 request.not_found", "directory", "move", "-id", "t1", "-cell", "eu-4", "-if-match", `"9"`)
	fails(t, "404 request.not_found", "directory", "freeze", "-id", "t9")
	fails(t, "409 tenancy.invalid_transition", "directory", "unfreeze", "-id", "t1")
	fails(t, "404 request.not_found", "directory", "locate", "-id", "t9")
	fails(t, "404 request.not_found", "directory", "move", "-id", "t9", "-cell", "eu-1")
	fails(t, "not JSON", "directory", "locate", "-id", "plain")
	fails(t, "-id is required", "directory", "locate")
	fails(t, "-id is required", "directory", "freeze")
	fails(t, "-id and -cell are required", "directory", "move", "-id", "t1")
	fails(t, "-config is required", "directory", "create", "-name", "x")
	fails(t, "no such file", "directory", "create", "-config", "/no/such.yaml")
	fails(t, "unknown command", "directory", "explode")
	fails(t, "usage: erp directory", "directory")
	for _, sub := range []string{"cells", "tenants", "locate", "create", "freeze", "verify", "move"} {
		t.Run(sub+" unknown flag", func(t *testing.T) {
			fails(t, "flag provided but not defined", "directory", sub, "-x")
		})
	}
}

// tokenAndConnection: a wrong or missing token, and a plane that cannot be reached.
func (s *directoryCase) tokenAndConnection(t *testing.T) {
	env, file := s.env, s.file
	env["ERP_OPERATOR_TOKEN"] = "wrong"
	fails(t, "401 authn.invalid_token", "directory", "cells")
	fails(t, "401 authn.invalid_token", "directory", "tenants")
	fails(t, "401 authn.invalid_token", "directory", "create", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "s", "-admin-name", "Ada", "-config", file)
	env["ERP_OPERATOR_TOKEN"] = ""
	fails(t, "ERP_OPERATOR_TOKEN is not set", "directory", "cells")
	env["ERP_OPERATOR_TOKEN"], env["ERP_DIRECTORY_API_URL"] = "op", "http://127.0.0.1:1"
	fails(t, "connect", "directory", "cells")
	delete(env, "ERP_DIRECTORY_API_URL")
	fails(t, "connect", "directory", "cells") // the default, localhost:9091, with nothing there
}
