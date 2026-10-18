package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
)

// cell is a fake cell: a control plane that creates and offboards tenants,
// a development issuer and a gateway answering with its headers.
type cell struct {
	mu         sync.Mutex
	created    []map[string]any
	offboarded []string
	fail       map[string]int // path → the status to answer
	tokens     []string
}

func newCell(t *testing.T) (*cell, *httptest.Server) {
	t.Helper()
	c := &cell{fail: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *cell) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if status, ok := c.fail[r.URL.Path]; ok {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code": "x"}`))
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/dev/token":
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		c.tokens = append(c.tokens, in["tenant_id"]+"|"+in["subject"])
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-" + in["subject"]})
	case r.URL.Path == "/control/v1/tenants":
		if r.Header.Get("Idempotency-Key") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			w.WriteHeader(400)
			return
		}
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		c.created = append(c.created, in)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "0192a000-0000-7000-8000-000000000001", "version": 1})
	case strings.HasSuffix(r.URL.Path, ":offboard"):
		c.offboarded = append(c.offboarded, r.URL.Path+" "+r.Header.Get("If-Match"))
		_, _ = w.Write([]byte(`{}`))
	case r.URL.Path == "/v1/model/v1/messages":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Erp-Model", "claude-test-1")
		w.Header().Set("Erp-Model-Cost", "1500")
		_, _ = w.Write([]byte(`{"id": "m", "type": "message", "role": "assistant", "model": "claude-test-1", "content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn", "usage": {"input_tokens": 1, "output_tokens": 1}}`))
	default:
		w.WriteHeader(404)
	}
}

func controlPlane(srv *httptest.Server) *eval.ControlPlane {
	return &eval.ControlPlane{ControlURL: srv.URL, APIURL: srv.URL + "/api", Mint: eval.DevIssuer(srv.URL, srv.Client()), HTTP: srv.Client()}
}

var tenantBody = map[string]any{"administrator": map[string]any{"subject": "eval|admin"}, "region": "eu-west"}

// TestControlPlane: a throwaway tenant is created under a name of its own
// from the suite's body, as an operator; its administrator and agent get
// tokens from the development issuer; closing offboards it.
func TestControlPlane(t *testing.T) {
	c, srv := newCell(t)
	cp := controlPlane(srv)
	ten, err := cp.New(context.Background(), tenantBody)
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := c.created[0]["name"].(string); !strings.HasPrefix(name, "eval-") {
		t.Errorf("name: got %q, want eval-<uuid>", name)
	}
	if _, ok := tenantBody["name"]; ok {
		t.Error("the suite's body was changed")
	}
	if ten.Admin().Token != "tok-eval|admin" || ten.Admin().BaseURL != srv.URL+"/api" {
		t.Errorf("administrator: got %+v", ten.Admin())
	}
	agent, err := ten.As(context.Background(), "eval|agent")
	if err != nil || agent.Token != "tok-eval|agent" {
		t.Fatalf("agent: got %v %v", agent, err)
	}
	if want := "0192a000-0000-7000-8000-000000000001|eval|agent"; c.tokens[len(c.tokens)-1] != want {
		t.Errorf("agent token: got %s, want %s", c.tokens[len(c.tokens)-1], want)
	}
	if err := ten.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.offboarded) != 1 || !strings.HasSuffix(c.offboarded[0], ":offboard *") {
		t.Errorf("offboarded: got %v", c.offboarded)
	}
}

// TestControlPlaneFailures: a body without an administrator, the control
// plane or the issuer refusing, fail the tenant; a tenant whose
// administrator gets no token is offboarded at once.
func TestControlPlaneFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		body any
		fail map[string]int
		mint eval.Mint
		want string
	}{
		{"not an object", "acme", nil, nil, "not an object"},
		{"no administrator", map[string]any{"region": "eu-west"}, nil, nil, "administrator subject"},
		{"creation refused", tenantBody, map[string]int{"/control/v1/tenants": 409}, nil, "answered 409"},
		{"no operator token", tenantBody, map[string]int{"/dev/token": 500}, nil, "operator's token"},
		{"no administrator token", tenantBody, nil, func(_ context.Context, ten, _ string) (string, error) {
			if ten == "" {
				return "tok-op", nil
			}
			return "", errors.New("issuer down")
		}, "administrator's token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl, srv := newCell(t)
			cl.fail = c.fail
			if cl.fail == nil {
				cl.fail = map[string]int{}
			}
			cp := controlPlane(srv)
			if c.mint != nil {
				cp.Mint = c.mint
			}
			_, err := cp.New(context.Background(), c.body)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
	t.Run("offboarding refused", func(t *testing.T) {
		cl, srv := newCell(t)
		ten, err := controlPlane(srv).New(context.Background(), tenantBody)
		if err != nil {
			t.Fatal(err)
		}
		cl.fail["/control/v1/tenants/0192a000-0000-7000-8000-000000000001:offboard"] = 409
		if err := ten.Close(context.Background()); err == nil {
			t.Error("got no error")
		}
	})
	t.Run("no agent token", func(t *testing.T) {
		cl, srv := newCell(t)
		ten, err := controlPlane(srv).New(context.Background(), tenantBody)
		if err != nil {
			t.Fatal(err)
		}
		cl.fail["/dev/token"] = 500
		if _, err := ten.As(context.Background(), "eval|agent"); err == nil {
			t.Error("got no error")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		cp := &eval.ControlPlane{ControlURL: "http://127.0.0.1:1", Mint: eval.DevIssuer("http://127.0.0.1:1", nil)}
		if _, err := cp.New(context.Background(), tenantBody); err == nil {
			t.Error("got no error")
		}
		cp.Mint = func(context.Context, string, string) (string, error) { return "t", nil }
		if _, err := cp.New(context.Background(), tenantBody); err == nil {
			t.Error("an unreachable control plane: got no error")
		}
		cp.ControlURL = "http://bad host"
		if _, err := cp.New(context.Background(), tenantBody); err == nil {
			t.Error("a bad URL: got no error")
		}
		if _, err := eval.DevIssuer("http://bad host", nil)(context.Background(), "", "s"); err == nil {
			t.Error("a bad issuer URL: got no error")
		}
	})
	t.Run("an answer not JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/dev/token" {
				_, _ = w.Write([]byte(`{"token": "t"}`))
				return
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{`))
		}))
		t.Cleanup(srv.Close)
		if _, err := controlPlane(srv).New(context.Background(), tenantBody); err == nil {
			t.Error("got no error")
		}
	})
	t.Run("a body cut short", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/dev/token" {
				_, _ = w.Write([]byte(`{"token": "t"}`))
				return
			}
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":`))
		}))
		t.Cleanup(srv.Close)
		if _, err := controlPlane(srv).New(context.Background(), tenantBody); err == nil {
			t.Error("got no error")
		}
	})
}

// TestGateway: the live model is the cell's gateway with the agent's own
// token, metered.
func TestGateway(t *testing.T) {
	_, srv := newCell(t)
	for _, agent := range []*apitools.Client{{BaseURL: srv.URL, Token: "a", HTTP: srv.Client()}, {BaseURL: srv.URL, Token: "a"}} {
		runner, meter, err := eval.Gateway{Alias: "erp-analytics"}.For(context.Background(), agent, eval.Case{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := runner.Client.Beta.Messages.New(context.Background(), runner.Params("system"))
		if err != nil {
			t.Fatal(err)
		}
		if runner.Model != "erp-analytics" {
			t.Errorf("alias: got %s, want the suite's", runner.Model)
		}
		if msg.Model != "claude-test-1" {
			t.Errorf("model: got %s", msg.Model)
		}
		if meter.Cost() != 1500 {
			t.Errorf("cost: got %d, want 1500", meter.Cost())
		}
	}
}
