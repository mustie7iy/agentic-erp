package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/modelalias"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// provider is a fake model provider: it answers with status and body, or
// fails, and keeps what it was last sent (guarded, for concurrent calls).
type provider struct {
	status int
	body   string
	err    error
	mu     sync.Mutex
	sent   []byte
	betas  []string
}

func (p *provider) Messages(_ context.Context, body []byte, betas []string) (int, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent, p.betas = body, betas
	return p.status, []byte(p.body), p.err
}

// providerFunc is a provider that answers as the function does.
type providerFunc func() (int, []byte, error)

func (f providerFunc) Messages(context.Context, []byte, []string) (int, []byte, error) { return f() }

// message is a provider's answer with usage.
func message(model, stop string, in, out, write, read int64) string {
	b, _ := json.Marshal(map[string]any{"type": "message", "model": model, "stop_reason": stop, "content": []any{map[string]any{"type": "text", "text": "ok"}},
		"usage": map[string]int64{"input_tokens": in, "output_tokens": out, "cache_creation_input_tokens": write, "cache_read_input_tokens": read}})
	return string(b)
}

// priced records fixture prices (per million tokens, in millionths of a
// cent): $4 in, $20 out, $5 cache write, $0.20 cache read.
func priced(t *testing.T, model string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read, recorded_by)
		VALUES ($1, '2026-01-01', 'USD', 400000000, 2000000000, 500000000, 20000000, 'test') ON CONFLICT DO NOTHING`, model)
}

// gateway is a gateway on the package database with p as provider.
func gateway(t *testing.T, p agents.Provider) *agents.Gateway {
	t.Helper()
	return &agents.Gateway{Provider: p, Routes: map[string]string{"erp-default": "claude-opus-5-5", "erp-unpriced": "claude-unpriced-1"},
		Pool: pgtest.Pool(t), AppRole: "erp_app", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AgentBudget: budgetOf}
}

// post sends body to the gateway as the principal.
func post(t *testing.T, g *agents.Gateway, as reqctx.Principal, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/model/v1/messages", strings.NewReader(body))
	for k, v := range header {
		r.Header.Set(k, v)
	}
	r = r.WithContext(reqctx.WithPrincipal(r.Context(), as))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, r)
	return rec
}

// startSession opens a running session for the agent.
func startSession(t *testing.T, ten pgtest.Tenant) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO agents.session (tenant_id, id, agent_id, purpose, model, status, cost_currency) VALUES ($1, $2, $3, 'test', 'erp-default', 'running', 'USD')`,
		ten.ID, id, ten.Agent.ID)
	return id
}

func principal(ten pgtest.Tenant) reqctx.Principal {
	return reqctx.Principal{ID: ten.Agent.ID, Kind: reqctx.KindAgent, TenantID: ten.ID}
}

// TestGatewayCall: a call is routed by its alias, forwarded with the model
// in place of the alias, answered as the provider answered, and recorded
// with its tokens and cost, never its content; within a session it adds
// to the session (ADR-0036).
func TestGatewayCall(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	session := startSession(t, ten)
	p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 1000, 100, 200, 300)}
	g := gateway(t, p)
	rec := post(t, g, principal(ten), `{"model": "erp-default", "max_tokens": 64, "messages": [{"role": "user", "content": "secret invoice text"}]}`,
		map[string]string{agents.SessionHeader: session.String()})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("answer: got %d %s, want the provider's message", rec.Code, rec.Body)
	}
	// 1000×4 + 100×20 + 200×5 + 300×0.2 cents per million = 0.706 cent.
	const want = 1000*400 + 100*2000 + 200*500 + 300*20
	check := func(what string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	check("Erp-Model-Cost", rec.Header().Get("Erp-Model-Cost"), "706000")
	check("Erp-Model", rec.Header().Get("Erp-Model"), "claude-opus-5-5")
	check("Erp-Model-Cost-Currency", rec.Header().Get("Erp-Model-Cost-Currency"), "USD")
	var sent map[string]any
	if err := json.Unmarshal(p.sent, &sent); err != nil {
		t.Fatalf("forwarded: %s is not JSON: %v", p.sent, err)
	}
	check("forwarded model", sent["model"], "claude-opus-5-5")
	var status, alias string
	var in, out, costMicro int64
	pgtest.Scan(t, `SELECT status, alias, input_tokens, output_tokens, cost_micro FROM agents.model_call WHERE tenant_id = $1 AND session_id = $2`,
		[]any{ten.ID, session}, &status, &alias, &in, &out, &costMicro)
	check("ledger status", status, "succeeded")
	check("ledger alias", alias, "erp-default")
	check("ledger input tokens", in, int64(1000))
	check("ledger output tokens", out, int64(100))
	check("ledger cost", costMicro, int64(want))
	var sIn, sOut, sMicro, sCost int64
	pgtest.Scan(t, `SELECT input_tokens, output_tokens, cost_micro, cost FROM agents.session WHERE tenant_id = $1 AND id = $2`, []any{ten.ID, session}, &sIn, &sOut, &sMicro, &sCost)
	check("session input tokens (input and cache)", sIn, int64(1500))
	check("session output tokens", sOut, int64(100))
	check("session cost in millionths", sMicro, int64(want))
	check("session cost in cents", sCost, int64(1))
}

// TestGatewayRefusals: what the gateway does not forward is refused before
// the provider is called, and nothing is recorded.
func TestGatewayRefusals(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	other := pgtest.NewTenant(t)
	foreign := startSession(t, other)
	p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 1, 1, 0, 0)}
	g := gateway(t, p)
	const ok = `{"model": "erp-default", "max_tokens": 8, "messages": []}`
	for _, c := range []struct {
		name, body string
		header     map[string]string
		status     int
		code       string
	}{
		{"not JSON", `[`, nil, 400, "request.invalid_body"},
		{"a field not forwarded", `{"model": "erp-default", "max_tokens": 8, "messages": [], "stream": true}`, nil, 400, "request.invalid_body"},
		{"no alias", `{"max_tokens": 8, "messages": []}`, nil, 400, "request.invalid_body"},
		{"a concrete model", `{"model": "claude-opus-5-5", "max_tokens": 8, "messages": []}`, nil, 422, "agents.unknown_model"},
		{"a server tool", `{"model": "erp-default", "max_tokens": 8, "messages": [], "tools": [{"type": "web_fetch_20260209", "name": "web_fetch"}]}`, nil, 400, "request.invalid_body"},
		{"tools not a list", `{"model": "erp-default", "max_tokens": 8, "messages": [], "tools": {}}`, nil, 400, "request.invalid_body"},
		{"a provider beta", ok, map[string]string{"anthropic-beta": "server-side-fallback-2026-07-01"}, 400, "request.invalid_body"},
		{"a fallback model", `{"model": "erp-default", "max_tokens": 8, "messages": [], "fallbacks": [{"model": "claude-unpriced-1", "speed": "fast"}]}`, nil, 400, "request.invalid_body"},
		{"a session header that is no id", ok, map[string]string{agents.SessionHeader: "x"}, 400, "request.invalid_header"},
		{"another tenant's session", ok, map[string]string{agents.SessionHeader: foreign.String()}, 404, "request.not_found"},
		{"a model without a price", `{"model": "erp-unpriced", "max_tokens": 8, "messages": []}`, nil, 422, "agents.model_unpriced"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.sent = nil
			rec := post(t, g, principal(ten), c.body, c.header)
			if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.code) {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, c.status, c.code)
			}
			if p.sent != nil {
				t.Error("the provider was called, want refused before")
			}
		})
	}
	t.Run("a custom tool is forwarded", func(t *testing.T) {
		rec := post(t, g, principal(ten), `{"model": "erp-default", "max_tokens": 8, "messages": [], "tools": [{"type": "custom", "name": "t", "input_schema": {"type": "object"}}, {"name": "u", "input_schema": {"type": "object"}}]}`, nil)
		if rec.Code != 200 {
			t.Errorf("got %d %s, want forwarded", rec.Code, rec.Body)
		}
	})
	t.Run("too large", func(t *testing.T) {
		rec := post(t, g, principal(ten), `{"model": "`+strings.Repeat("x", agents.MaxModelBody)+`"}`, nil)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("got %d, want 413", rec.Code)
		}
	})
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM agents.model_call WHERE tenant_id = $1 AND alias <> 'erp-default'`, []any{ten.ID}, &n)
	if n != 0 {
		t.Errorf("refused calls recorded: got %d, want none", n)
	}
}

// TestGatewayFailures: a provider's error answer is passed on as it came,
// and a provider that does not answer is 502; both are recorded as failed
// calls with no tokens; a refusal is recorded as refused.
func TestGatewayFailures(t *testing.T) {
	priced(t, "claude-opus-5-5")
	for _, c := range []struct {
		name     string
		provider *provider
		status   int
		ledger   string
	}{
		{"overloaded", &provider{status: 529, body: `{"type":"error","error":{"type":"overloaded_error"}}`}, 529, "failed"},
		{"the cell's key refused", &provider{status: 401, body: `{"type":"error","error":{"type":"authentication_error"}}`}, 502, "failed"},
		{"the cell's key forbidden", &provider{status: 403, body: `{"type":"error"}`}, 502, "failed"},
		{"unreachable", &provider{err: errors.New("connection refused")}, 502, "failed"},
		{"not a message", &provider{status: 200, body: `{`}, 200, "failed"},
		{"refused", &provider{status: 200, body: message("", "refusal", 5, 0, 0, 0)}, 200, "refused"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ten := pgtest.NewTenant(t)
			rec := post(t, gateway(t, c.provider), principal(ten), `{"model": "erp-default", "max_tokens": 8, "messages": []}`, nil)
			if rec.Code != c.status {
				t.Fatalf("status: got %d %s, want %d", rec.Code, rec.Body, c.status)
			}
			var status, model string
			pgtest.Scan(t, `SELECT status, model FROM agents.model_call WHERE tenant_id = $1`, []any{ten.ID}, &status, &model)
			if status != c.ledger || model != "claude-opus-5-5" {
				t.Errorf("ledger: got %s for %s, want %s for claude-opus-5-5", status, model, c.ledger)
			}
		})
	}
	t.Run("tokens no int64 can price", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		rec := post(t, gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", math.MaxInt64/2, 0, 0, 0)}), principal(ten),
			`{"model": "erp-default", "max_tokens": 8, "messages": []}`, nil)
		if got := rec.Header().Get("Erp-Model-Cost"); got != "9223372036854775807" {
			t.Errorf("cost: got %s, want the most an int64 holds", got)
		}
	})
}

// TestGatewayTransport: the server's own model calls go through the
// gateway in process, as the request's principal, with their checks and
// ledger in transactions of their own; an SDK client uses it as its
// transport.
func TestGatewayTransport(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 2, 0, 0)})
	client := anthropic.NewClient(option.WithBaseURL("http://model-gateway.internal"), option.WithAPIKey("in-process"),
		option.WithHTTPClient(&http.Client{Transport: g.Transport()}), option.WithMaxRetries(0))
	params := anthropic.MessageNewParams{Model: "erp-default", MaxTokens: 8, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal(ten))
		msg, err := client.Messages.New(ctx, params)
		if err != nil {
			return err
		}
		if msg.Usage.InputTokens != 10 {
			t.Errorf("usage: got %d, want 10", msg.Usage.InputTokens)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM agents.model_call WHERE tenant_id = $1 AND status = 'succeeded'`, []any{ten.ID}, &n)
	if n != 1 {
		t.Errorf("recorded: got %d calls, want 1", n)
	}
	t.Run("outside a unit of work", func(t *testing.T) {
		if _, err := client.Messages.New(context.Background(), params); err == nil {
			t.Error("got answered, want refused outside a unit of work")
		}
	})
	t.Run("a call in flight holds its reservation where other calls see it", func(t *testing.T) {
		held := -1
		seeing := providerFunc(func() (int, []byte, error) {
			// Another connection than the request's, as another call's.
			pgtest.Scan(t, `SELECT count(*) FROM agents.model_reservation WHERE tenant_id = $1 AND settled_at IS NULL`, []any{ten.ID}, &held)
			return http.StatusOK, []byte(message("claude-opus-5-5", "end_turn", 10, 2, 0, 0)), nil
		})
		g := gateway(t, seeing)
		client := anthropic.NewClient(option.WithBaseURL("http://model-gateway.internal"), option.WithAPIKey("in-process"),
			option.WithHTTPClient(&http.Client{Transport: g.Transport()}), option.WithMaxRetries(0))
		err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := client.Messages.New(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal(ten)), params)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if held != 2 {
			t.Errorf("reservations seen while the call was in flight: got %d, want 2 (the tenant's line and the agent's)", held)
		}
		if n := count(t, ten, `SELECT count(*) FROM agents.model_reservation WHERE tenant_id = $1 AND settled_at IS NULL`); n != 0 {
			t.Errorf("reservations held after the call: got %d, want 0", n)
		}
	})
	t.Run("a refused call", func(t *testing.T) {
		err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
			bad := params
			bad.Model = "claude-opus-5-5"
			_, err := client.Messages.New(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal(ten)), bad)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "agents.unknown_model") {
			t.Errorf("got %v, want agents.unknown_model", err)
		}
	})
	t.Run("a capture that fails afterwards keeps its call on record", func(t *testing.T) {
		before := calls(t, ten)
		err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := client.Messages.New(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal(ten)), params); err != nil {
				return err
			}
			return errors.New("the capture fails after the call")
		})
		if err == nil {
			t.Fatal("got committed, want the capture's failure")
		}
		if got := calls(t, ten); got != before+1 {
			t.Errorf("calls on record: got %d, want %d: the call stands though the request rolled back", got, before+1)
		}
	})
}

// calls is how many model calls the tenant has on record.
func calls(t *testing.T, ten pgtest.Tenant) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM agents.model_call WHERE tenant_id = $1`, []any{ten.ID}, &n)
	return n
}

// TestGatewayDatabaseFailures: each statement of the gateway's own
// transactions failing in turn (the session and the price, the budget
// lines and the reservation; the session's lock, the ledger row, the
// session's update and the settlement), a transaction that cannot begin
// or commit, each fails the call.
func TestGatewayDatabaseFailures(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	pool := pgtest.Pool(t)
	try := func(t *testing.T, begin database.BeginFunc) int {
		t.Helper()
		g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 1, 1, 0, 0)})
		g.Begin = begin
		return post(t, g, principal(ten), `{"model": "erp-default", "max_tokens": 8, "messages": []}`, map[string]string{agents.SessionHeader: startSession(t, ten).String()}).Code
	}
	t.Run("no transaction", func(t *testing.T) {
		if code := try(t, func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }); code != http.StatusInternalServerError {
			t.Errorf("got %d, want 500", code)
		}
	})
	t.Run("no commit", func(t *testing.T) {
		if code := try(t, func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
		}); code != http.StatusInternalServerError {
			t.Errorf("got %d, want 500", code)
		}
	})
	t.Run("each statement", func(t *testing.T) {
		for at := 1; ; at++ {
			count := 0
			code := try(t, func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				return &pgtest.FailingTx{Tx: tx, FailAt: at, Count: &count}, err
			})
			if count < at { // every statement ran: none was failed
				if code != http.StatusOK {
					t.Errorf("with no statement failed: got %d, want 200", code)
				}
				return
			}
			if code != http.StatusInternalServerError {
				t.Fatalf("statement %d failed: got %d, want 500", at, code)
			}
		}
	})
}

// deadlineWriter is a response writer that records the write deadline
// asked of it, as http.ResponseController asks.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error { d.deadline = t; return nil }

// TestGatewayWriteDeadline: the answer may be written for the call's time,
// not the listener's write timeout, which would cut a long answer short
// and have the SDK retry, a call billed again.
func TestGatewayWriteDeadline(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{{"the default", 0, agents.DefaultTimeout}, {"configured", time.Minute, time.Minute}} {
		t.Run(c.name, func(t *testing.T) {
			g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 1, 1, 0, 0)})
			g.Now, g.Timeout = func() time.Time { return now }, c.timeout
			w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			r := httptest.NewRequest(http.MethodPost, agents.Route, strings.NewReader(`{"model": "erp-default", "max_tokens": 8, "messages": []}`))
			g.ServeHTTP(w, r.WithContext(reqctx.WithPrincipal(r.Context(), principal(ten))))
			if want := now.Add(c.want + agents.WriteMargin); !w.deadline.Equal(want) {
				t.Errorf("write deadline: got %v, want %v", w.deadline, want)
			}
		})
	}
}

// TestModelContractIsServed: the gateway's contract names the route the
// server serves (api/model.yaml).
func TestModelContractIsServed(t *testing.T) {
	ops, err := api.ModelOperations()
	if err != nil {
		t.Fatal(err)
	}
	if op := ops["createModelMessage"]; op.Method+" "+op.Path != "POST "+agents.Route {
		t.Errorf("contract: got %s %s, want POST %s", op.Method, op.Path, agents.Route)
	}
}

// TestDefaultRoutes: without routes of its own the gateway routes the
// platform's aliases.
func TestDefaultRoutes(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 1, 1, 0, 0)}
	g := gateway(t, p)
	g.Routes = nil
	for alias := range agents.DefaultRoutes {
		t.Run(alias, func(t *testing.T) {
			if rec := post(t, g, principal(ten), `{"model": "`+alias+`", "max_tokens": 8, "messages": []}`, nil); rec.Code != 200 {
				t.Errorf("got %d %s, want routed", rec.Code, rec.Body)
			}
		})
	}
}

// failingBody is a request body that cannot be read.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("broken body") }

// TestTransportRefusals: an in-process request whose body cannot be read,
// or whose session header is no id, is refused.
func TestTransportRefusals(t *testing.T) {
	ten := pgtest.NewTenant(t)
	g := gateway(t, &provider{status: 200})
	err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal(ten))
		broken, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://gw/v1/messages", failingBody{})
		if err := roundTrip(g, broken); err == nil || !strings.Contains(err.Error(), "broken body") {
			t.Errorf("unreadable body: got %v, want its error", err)
		}
		bad, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://gw/v1/messages", strings.NewReader(`{}`))
		bad.Header.Set(agents.SessionHeader, "x")
		if err := roundTrip(g, bad); err == nil {
			t.Error("a session header that is no id: got answered, want refused")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAnthropic: the provider is called with the cell's key, the API
// version and the forwarded betas, and its answer comes back as it was.
func TestAnthropic(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(529)
		w.Write([]byte(`{"type":"error"}`))
	}))
	defer srv.Close()
	a := agents.Anthropic{Endpoint: srv.URL + "/", Key: "sk-cell", Client: srv.Client()}
	status, body, err := a.Messages(context.Background(), []byte(`{}`), []string{"server-side-fallback-2026-07-01"})
	if err != nil || status != 529 || string(body) != `{"type":"error"}` {
		t.Errorf("answer: got %d %s %v, want 529 and the body", status, body, err)
	}
	if got.Get("X-Api-Key") != "sk-cell" || got.Get("Anthropic-Version") != agents.APIVersion || got.Get("Anthropic-Beta") != "server-side-fallback-2026-07-01" {
		t.Errorf("headers: got %v", got)
	}
	t.Run("without betas", func(t *testing.T) {
		if _, _, err := a.Messages(context.Background(), []byte(`{}`), nil); err != nil || got.Get("Anthropic-Beta") != "" {
			t.Errorf("got %v, beta %q, want no beta header", err, got.Get("Anthropic-Beta"))
		}
	})
	for name, endpoint := range map[string]string{"unreachable": "http://127.0.0.1:1", "not a URL": "::"} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := (agents.Anthropic{Endpoint: endpoint, Client: http.DefaultClient}).Messages(context.Background(), nil, nil); err == nil {
				t.Error("got answered, want an error")
			}
		})
	}
	t.Run("an answer cut short", func(t *testing.T) {
		cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.Write([]byte("short"))
		}))
		defer cut.Close()
		if _, _, err := (agents.Anthropic{Endpoint: cut.URL, Client: cut.Client()}).Messages(context.Background(), nil, nil); err == nil {
			t.Error("got answered, want the read's error")
		}
	})
}

// roundTrip sends req through the gateway's in-process transport and
// closes the answer's body, returning the transport's error.
func roundTrip(g *agents.Gateway, req *http.Request) error {
	resp, err := g.Transport().RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close() // only the outcome is asserted
	}
	return err
}

// TestConcurrentCallsOfOneSession: calls of one session that finish
// together each stand, and the session sums them all: its row is locked
// before the calls' rows reference it, so no two of them deadlock.
func TestConcurrentCallsOfOneSession(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	session := startSession(t, ten)
	g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)})
	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, agents.Route, strings.NewReader(`{"model": "erp-default", "max_tokens": 8, "messages": []}`))
			r.Header.Set(agents.SessionHeader, session.String())
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, r.WithContext(reqctx.WithPrincipal(r.Context(), principal(ten))))
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a concurrent call: got %d, want 200", code)
		}
	}
	var in int64
	var spent int64
	pgtest.Scan(t, `SELECT input_tokens, cost_micro FROM agents.session WHERE tenant_id = $1 AND id = $2`, []any{ten.ID, session}, &in, &spent)
	if in != 10*n {
		t.Errorf("session input tokens: got %d, want %d", in, 10*n)
	}
	var rows int
	var each int64
	pgtest.Scan(t, `SELECT count(*), sum(cost_micro) / count(*) FROM agents.model_call WHERE tenant_id = $1 AND session_id = $2`, []any{ten.ID, session}, &rows, &each)
	if rows != n {
		t.Errorf("calls recorded: got %d, want %d", rows, n)
	}
	if each <= 0 || spent != each*int64(n) {
		t.Errorf("session cost: got %d, want %d calls of %d", spent, n, each)
	}
}

// TestRoutesAreTheAliases: the gateway routes exactly the aliases agents'
// profiles may name.
func TestRoutesAreTheAliases(t *testing.T) {
	var got []string
	for alias := range agents.DefaultRoutes {
		got = append(got, alias)
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(modelalias.Aliases))
	if !slices.Equal(got, want) {
		t.Errorf("routes: got %v, want %v", got, want)
	}
}
