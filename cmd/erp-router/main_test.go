package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func environment(t *testing.T, is *authntest.Issuer) map[string]string {
	t.Helper()
	issuers, _ := json.Marshal([]any{is.Config()})
	return map[string]string{"ERP_DIRECTORY_URL": pgtest.URL(t), "ERP_OIDC_ISSUERS": string(issuers), "ERP_OPERATOR_ISSUERS": string(issuers),
		"ERP_PAGE_TOKEN_KEY": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		"ERP_ROUTER_ADDR":    "127.0.0.1:0", "ERP_ROUTER_ADMIN_ADDR": "127.0.0.1:0", "ERP_SHUTDOWN_TIMEOUT": "5s"}
}

// TestRoutesToTheCell: the router serves, forwards a tenant's request to the
// cell the directory names, and stops cleanly.
func TestRoutesToTheCell(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is)
	getenv := func(k string) string { return env[k] }
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cell":"eu-1","path":"` + r.URL.Path + `"}`))
	}))
	defer cell.Close()
	tenant := uuid.New()
	dir, err := directory.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.Upsert(context.Background(), directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: cell.URL, Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	addr, adminAddr := make(chan string, 1), make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, getenv, &logs, func(ln, admin net.Listener) { addr <- ln.Addr().String(); adminAddr <- admin.Addr().String() })
	}()
	var base string
	select {
	case a := <-addr:
		base = "http://" + a
	case code := <-done:
		t.Fatalf("stopped before serving: %d %s", code, logs.String())
	}
	req, _ := http.NewRequest("GET", base+"/v1/balances", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"cell":"eu-1"`) || resp.Header.Get("X-Erp-Cell") != "eu-1" {
		t.Fatalf("%d %s %v", resp.StatusCode, body, resp.Header)
	}
	if code := getStatus(t, base+"/livez"); code != 200 {
		t.Fatalf("livez: got %d, want 200", code)
	}
	// The admin listener: liveness and metrics, and the global control plane for operators.
	var adminBase string
	select {
	case a := <-adminAddr:
		adminBase = "http://" + a
	default:
		t.Fatal("no admin listener")
	}
	for _, path := range []string{"/livez", "/metrics"} {
		if code := getStatus(t, adminBase+path); code != 200 {
			t.Fatalf("admin %s: got %d, want 200", path, code)
		}
	}
	req, _ = http.NewRequest("GET", adminBase+"/directory/v1/tenants/"+tenant.String(), nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, "ops|alice", map[string]any{"tenant_id": nil}))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("directory API: %v %v", resp, err)
	}
	resp.Body.Close()
	cancel()
	if code := <-done; code != 0 || !strings.Contains(logs.String(), "erp-router stopped") {
		t.Fatalf("stop: %d %s", code, logs.String())
	}
}

func TestExitCodes(t *testing.T) {
	is := authntest.New(t)
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	for name, c := range map[string]struct {
		mutate func(env map[string]string)
		code   int
		want   string
	}{
		"configuration": {func(e map[string]string) { delete(e, "ERP_DIRECTORY_URL") }, 2, "ERP_DIRECTORY_URL"},
		"directory": {func(e map[string]string) {
			e["ERP_DIRECTORY_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
		}, 1, "erp-router stopped"},
		"issuer": {func(e map[string]string) {
			e["ERP_OIDC_ISSUERS"] = `[{"issuer":"http://127.0.0.1:1","audience":"erp-api"}]`
		}, 1, "erp-router stopped"},
		"address in use": {func(e map[string]string) { e["ERP_ROUTER_ADDR"] = busy.Addr().String() }, 1, "address already in use"},
		"admin in use":   {func(e map[string]string) { e["ERP_ROUTER_ADMIN_ADDR"] = busy.Addr().String() }, 1, "address already in use"},
		"operator issuer": {func(e map[string]string) {
			e["ERP_OPERATOR_ISSUERS"] = `[{"issuer":"http://127.0.0.1:1","audience":"erp-control"}]`
		}, 1, "erp-router stopped"},
		"page key": {func(e map[string]string) {
			e["ERP_PAGE_TOKEN_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) + "x"
		}, 2, "ERP_PAGE_TOKEN_KEY"},
		"listener closed": {func(map[string]string) {}, 1, "closed"},
	} {
		env := environment(t, is)
		c.mutate(env)
		var logs bytes.Buffer
		started := func(_, _ net.Listener) {}
		if name == "listener closed" {
			started = func(ln, _ net.Listener) { ln.Close() }
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		code := cli(ctx, func(k string) string { return env[k] }, &logs, started)
		cancel()
		if code != c.code || !strings.Contains(logs.String(), c.want) {
			t.Errorf("%s: %d %s", name, code, logs.String())
		}
	}
	// main wires the signal context and exits with the code.
	exit = func(int) {}
	defer func() { exit = nil }()
	t.Setenv("ERP_DIRECTORY_URL", "")
	main()
}

// getStatus gets the URL and returns the response's status, its body closed.
func getStatus(t *testing.T, u string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
