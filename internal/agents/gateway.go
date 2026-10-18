package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The model gateway (api/model.yaml, ADR-0036): every model call an agent
// or the server makes goes through it. It routes the alias a request names
// to a model, refuses a model without a price, forwards the request to the
// provider and records the call, its tokens and cost but never its
// content, in agents.model_call.

// DefaultRoutes are the aliases callers name and the models they route to
// when nothing else is configured.
var DefaultRoutes = map[string]string{"erp-default": "claude-opus-5-5", "erp-extraction": "claude-opus-5-5", "erp-analytics": "claude-opus-5-5"}

// SessionHeader ties a call to the caller's agent session.
const SessionHeader = "Erp-Agent-Session"

// Route is the gateway's path on the public listener: an SDK whose base
// URL is /v1/model sends its Messages requests there (api/model.yaml).
const Route = "/v1/model/v1/messages"

// MaxModelBody bounds a request: the provider takes 32 MB, documents
// included.
const MaxModelBody = 32 << 20

// allowedFields are the Messages API request fields the gateway forwards.
// Not fallbacks: a fallback model would answer unrouted and unpriced; the
// gateway routes every call to the one model it prices.
var allowedFields = []string{"model", "max_tokens", "messages", "system", "tools", "tool_choice", "output_config", "thinking",
	"stop_sequences", "metadata"}

// DefaultTimeout bounds one model call; its answer may take WriteMargin
// more to write. A long answer takes minutes, which the listener's write
// timeout would cut short, and a cut answer is retried by the SDK, a call
// billed again.
const DefaultTimeout = api.ModelCallTimeout

// WriteMargin is the time past the call's own that its answer may take to
// write: the deadline is set before the checks, the provider's timeout
// only once it is called.
const WriteMargin = time.Minute

// Provider sends a Messages API request to the model provider.
type Provider interface {
	Messages(ctx context.Context, body []byte, betas []string) (status int, answer []byte, err error)
}

// Gateway is the model gateway of a cell.
type Gateway struct {
	Provider Provider
	Routes   map[string]string  // alias → model; nil: DefaultRoutes
	Pool     *database.Pool     // the gateway's own transactions
	Begin    database.BeginFunc // nil: Pool.Begin; tests inject failures
	AppRole  string
	Log      *slog.Logger
	Now      func() time.Time
	Timeout  time.Duration // a call's answer may be written for this long; 0: DefaultTimeout
	// AgentBudget reads an agent's monthly budget (identity.AgentBudget);
	// nil: agents are held by the tenant's cap only.
	AgentBudget AgentBudget
}

// inTx runs fn in a transaction of the caller's tenant.
type inTx func(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error

// call is one request to the gateway.
type call struct {
	body    []byte
	betas   []string
	session *uuid.UUID
	check   inTx // the checks before the call
	record  inTx // the ledger after it: always a transaction of its own, so the row stands whatever the caller does next
}

// answer is what the provider said and what the call cost.
type answer struct {
	status   int
	body     []byte
	model    string
	cost     money.Micro
	currency string
}

// price is a model's prices per million tokens.
type price struct {
	input, output, cacheWrite, cacheRead money.Micro
	currency                             string
}

// request is a call's request, checked and routed.
type request struct {
	alias, model string
	fields       map[string]json.RawMessage
	body         []byte // with the model in place of the alias, once routed
	maxTokens    int64
}

// route sets the model the request goes to, in place of its alias.
func (r *request) route(model string) {
	r.model = model
	r.fields["model"], _ = json.Marshal(model) // a string always encodes
	r.body, _ = json.Marshal(r.fields)         // raw messages of a decoded object always encode
}

// serve makes one call for p: routed, priced, held against its budgets,
// forwarded, recorded and settled.
func (g *Gateway) serve(ctx context.Context, p reqctx.Principal, c call) (answer, error) {
	req, err := g.prepare(c.body)
	if err != nil {
		return answer{}, err
	}
	if len(c.betas) > 0 {
		return answer{}, problem.BadRequest("request.invalid_body", "The gateway forwards no provider betas.")
	}
	pr, res, err := g.admit(ctx, p, c, &req)
	if err != nil {
		return answer{}, err
	}
	started := clock.Now(g.Now)
	status, raw, sendErr := g.Provider.Messages(ctx, req.body, c.betas)
	rec := g.meter(status, raw, sendErr, req.model, pr)
	rec.id, rec.alias, rec.duration = res.call, req.alias, clock.Now(g.Now).Sub(started)
	if err := c.record(context.WithoutCancel(ctx), func(ctx context.Context, tx pgx.Tx) error {
		if err := recordCall(ctx, tx, p, c.session, rec); err != nil {
			return err
		}
		return settle(ctx, tx, p, res, rec.cost, clock.Now(g.Now))
	}); err != nil {
		return answer{}, err
	}
	switch {
	case sendErr != nil:
		return answer{}, problem.New(http.StatusBadGateway, "agents.model_unreachable", "Model unreachable", "The model provider did not answer; try again.").WithCause(sendErr)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The provider refused the cell's key, not the caller's token.
		g.Log.ErrorContext(ctx, "the model provider refused the cell's key", "status", status)
		return answer{}, problem.New(http.StatusBadGateway, "agents.model_unreachable", "Model unreachable", "The model provider refused this cell's credentials.")
	}
	return answer{status: status, body: raw, model: rec.model, cost: rec.cost, currency: pr.currency}, nil
}

// admit runs the checks before the call, in their own transaction, which
// commits before the call is forwarded so that its reservation holds
// against the calls after it: the tenant's AI is on, the session is p's
// and running, the alias routes under the tenant's policy to a model it
// allows and that is priced, and what the call may cost fits its budgets.
// A refusal for the budget commits its audit entry.
func (g *Gateway) admit(ctx context.Context, p reqctx.Principal, c call, req *request) (price, reservation, error) {
	var pr price
	var res reservation
	var refused *problem.Problem
	err := c.check(ctx, func(ctx context.Context, tx pgx.Tx) error {
		pol, err := ReadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		if err := enabled(pol); err != nil {
			return err
		}
		if err := ownSession(ctx, tx, p, c.session); err != nil {
			return err
		}
		if err := routeUnder(pol, req); err != nil {
			return err
		}
		now := clock.Now(g.Now)
		if pr, err = priceOf(ctx, tx, req.model, now); err != nil {
			return err
		}
		res, refused, err = g.reserve(ctx, tx, p, pol, mostOf(*req, pr), now)
		return err
	})
	if err == nil && refused != nil {
		err = refused
	}
	return pr, res, err
}

// MaxInputTokens bounds a call's input estimate: no model takes more,
// and a document's bytes, base64 images and pages, far overstate its
// tokens.
const MaxInputTokens = 1_000_000

// mostOf is what a call is held at: all of max_tokens in output, and its
// body in input at a token per three bytes, at most MaxInputTokens, priced
// as cache writes when those cost more. The input is an estimate: tokens
// the body does not show (the provider's tool prompt, a page's image)
// may make the call cost more, and its line then ends past its cap by
// that much, announced at 100%. Held at the most an int64 holds.
func mostOf(req request, pr price) money.Micro {
	in := max(pr.input, pr.cacheWrite)
	inCost, err1 := money.TokenCost(min((int64(len(req.body))+2)/3, MaxInputTokens), in)
	outCost, err2 := money.TokenCost(req.maxTokens, pr.output)
	if err1 != nil || err2 != nil {
		return math.MaxInt64
	}
	return inCost.Add(outCost)
}

// prepare checks the request against the forwarded fields and routes its
// alias, returning the body with the model in its place.
func (g *Gateway) prepare(body []byte) (request, error) {
	var out request
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return out, problem.BadRequest("request.invalid_body", "The request is not a JSON object.").WithCause(err)
	}
	for field := range req {
		if !slices.Contains(allowedFields, field) {
			return out, problem.BadRequest("request.invalid_body", "The gateway does not forward "+field+".")
		}
	}
	if err := json.Unmarshal(req["model"], &out.alias); err != nil || out.alias == "" {
		return out, problem.BadRequest("request.invalid_body", "model must name a model alias.")
	}
	// The budget holds all of max_tokens, so it must be known.
	if err := json.Unmarshal(req["max_tokens"], &out.maxTokens); err != nil || out.maxTokens <= 0 {
		return out, problem.BadRequest("request.invalid_body", "max_tokens must be a positive whole number.")
	}
	if err := checkTools(req["tools"]); err != nil {
		return out, err
	}
	routes := g.Routes
	if routes == nil {
		routes = DefaultRoutes
	}
	model, ok := routes[out.alias]
	if !ok {
		return out, problem.Unprocessable("agents.unknown_model", "The gateway routes no model alias "+out.alias+".",
			problem.FieldError{Field: "model", Code: "unknown", Message: "a model alias: erp-default, erp-extraction or erp-analytics"})
	}
	out.fields = req
	out.route(model)
	return out, nil
}

// enabled refuses every call while the tenant's AI is off: switched off by
// its administrators, or suspended by the platform's operators, whose
// hand wins (ADR-0036).
func enabled(pol Policy) error {
	switch {
	case pol.Suspended:
		return problem.Forbidden("agents.ai_disabled", "The platform's operators have suspended this tenant's AI.")
	case !pol.Enabled:
		return problem.Forbidden("agents.ai_disabled", "This tenant's administrators have switched its AI off.")
	}
	return nil
}

// routeUnder routes the request's alias under the tenant's policy, over the
// gateway's routes, to a model the policy allows.
func routeUnder(pol Policy, req *request) error {
	if model, ok := pol.Routes[req.alias]; ok {
		req.route(model)
	}
	if !pol.Allows(req.model) {
		return problem.Unprocessable("agents.model_not_allowed", "The alias "+req.alias+" routes to "+req.model+", which the tenant's AI policy does not allow.",
			problem.FieldError{Field: "model", Code: "not_allowed", Message: "an alias the tenant's policy routes to an allowed model"})
	}
	return nil
}

// checkTools refuses server tools: only the caller's own tools, which have
// no type, are forwarded.
func checkTools(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return problem.BadRequest("request.invalid_body", "tools must be a list of tool definitions.").WithCause(err)
	}
	for _, t := range tools {
		if typ, ok := t["type"]; ok && string(typ) != `"custom"` {
			return problem.BadRequest("request.invalid_body", "The gateway forwards the caller's own tools only, not server tools.")
		}
	}
	return nil
}

// betas reads the anthropic-beta header's values.
func betas(h http.Header) []string {
	var out []string
	for _, v := range h.Values("anthropic-beta") {
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// sessionOf reads the session a call belongs to, if it names one.
func sessionOf(h http.Header) (*uuid.UUID, error) {
	s := h.Get(SessionHeader)
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, problem.BadRequest("request.invalid_header", SessionHeader+" must be a session id.").WithCause(err)
	}
	return &id, nil
}

// ownSession checks that a named session is a running session of p's.
func ownSession(ctx context.Context, tx pgx.Tx, p reqctx.Principal, session *uuid.UUID) error {
	if session == nil {
		return nil
	}
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM agents.session WHERE id = $1 AND agent_id = $2 AND status = 'running')`, *session, p.ID).Scan(&running); err != nil {
		return fmt.Errorf("agents: read session %s: %w", *session, err)
	}
	if !running {
		return problem.NotFoundOf("agent session")
	}
	return nil
}

// priceOf is the model's price in force at now, or 422 without one: no
// call goes unmetered.
func priceOf(ctx context.Context, tx pgx.Tx, model string, now time.Time) (price, error) {
	var p price
	err := tx.QueryRow(ctx, `SELECT input, output, cache_write, cache_read, currency FROM ref.model_price
		WHERE model = $1 AND effective_from <= $2 ORDER BY effective_from DESC LIMIT 1`, model, now.UTC().Format(time.DateOnly)).
		Scan(&p.input, &p.output, &p.cacheWrite, &p.cacheRead, &p.currency)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return p, problem.Unprocessable("agents.model_unpriced", "The model "+model+" has no recorded price; an operator records it with erp-server prices load.")
	case err != nil:
		return p, fmt.Errorf("agents: read the price of %s: %w", model, err)
	}
	return p, nil
}

// usage is the part of the provider's message the ledger reads.
type usage struct {
	StopReason string `json:"stop_reason"`
	Usage      struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// row is one ledger row.
type row struct {
	id                         uuid.UUID // the call's, as its reservations name it
	alias, model, status       string
	input, output, write, read int64
	cost                       money.Micro
	currency                   string
	duration                   time.Duration
}

// meter turns the provider's answer into a ledger row: an answer that is
// not a message is a failed call with no tokens, priced at nothing.
func (g *Gateway) meter(status int, raw []byte, sendErr error, model string, pr price) row {
	r := row{model: model, status: "failed", currency: pr.currency}
	var u usage
	if sendErr != nil || status != http.StatusOK || json.Unmarshal(raw, &u) != nil {
		return r
	}
	r.status = "succeeded"
	if u.StopReason == "refusal" {
		r.status = "refused"
	}
	r.input, r.output, r.write, r.read = u.Usage.Input, u.Usage.Output, u.Usage.CacheWrite, u.Usage.CacheRead
	r.cost = cost(r, pr)
	return r
}

// cost prices the row's tokens. A cost past what an int64 holds (some
// ninety billion dollars) is held at that most, never wrapped.
func cost(r row, pr price) money.Micro {
	var total money.Micro
	for _, part := range []struct {
		tokens int64
		price  money.Micro
	}{{r.input, pr.input}, {r.output, pr.output}, {r.write, pr.cacheWrite}, {r.read, pr.cacheRead}} {
		c, err := money.TokenCost(part.tokens, part.price)
		if err != nil || c > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += c
	}
	return total
}

// recordCall writes the ledger row and adds it to the session it belongs
// to. The session's row is locked first (FOR UPDATE, ADR-0008), before the
// call's row references it: inserted first, each call's row would hold a
// key share on the session that the other's lock waits on, and two calls
// finishing together would deadlock. A session ended while the call ran
// still counts it.
func recordCall(ctx context.Context, tx pgx.Tx, p reqctx.Principal, session *uuid.UUID, r row) error {
	var spent money.Micro
	if session != nil {
		if err := tx.QueryRow(ctx, `SELECT cost_micro FROM agents.session WHERE id = $1 AND agent_id = $2 FOR UPDATE`, *session, p.ID).Scan(&spent); err != nil {
			return fmt.Errorf("agents: lock session %s: %w", *session, err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agents.model_call (tenant_id, id, principal_id, session_id, alias, model, status, input_tokens, output_tokens,
		cache_write_tokens, cache_read_tokens, cost_micro, currency, duration_ms) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		p.TenantID, r.id, p.ID, session, r.alias, r.model, r.status, r.input, r.output, r.write, r.read, r.cost, r.currency,
		r.duration.Milliseconds()); err != nil {
		return fmt.Errorf("agents: record model call: %w", err)
	}
	if session == nil {
		return nil
	}
	spent = spent.Add(r.cost)
	if _, err := tx.Exec(ctx, `UPDATE agents.session SET input_tokens = input_tokens + $2, output_tokens = output_tokens + $3,
		cost_micro = $4, cost = $5, cost_currency = $6, version = version + 1 WHERE id = $1`,
		*session, r.input+r.write+r.read, r.output, spent, spent.Minor(), r.currency); err != nil {
		return fmt.Errorf("agents: add model call to session %s: %w", *session, err)
	}
	return nil
}

// ServeHTTP serves createModelMessage behind uow.Authenticated: the
// principal is known, no transaction is held, and the ledger is written in
// a transaction of its own.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _ := reqctx.PrincipalFrom(ctx) // uow.Authenticated put it there
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxModelBody))
	if err != nil {
		problem.Write(w, r, problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large",
			"A model request holds at most 32 MB.").WithCause(err))
		return
	}
	session, err := sessionOf(r.Header)
	if err != nil {
		problem.Write(w, r, problem.From(err))
		return
	}
	// The answer may take the call's time to come: the listener's write
	// timeout is for other requests.
	_ = http.NewResponseController(w).SetWriteDeadline(clock.Now(g.Now).Add(g.timeout() + WriteMargin)) // a writer without deadlines has none to lift
	a, err := g.serve(ctx, p, call{body: body, betas: betas(r.Header), session: session, check: g.ownTx(p), record: g.ownTx(p)})
	if err != nil {
		g.fail(w, r, err)
		return
	}
	writeAnswer(w, a)
}

func (g *Gateway) fail(w http.ResponseWriter, r *http.Request, err error) {
	p := problem.From(err)
	if p.Status >= 500 {
		g.Log.ErrorContext(r.Context(), "model call failed", "error", err)
	}
	problem.Write(w, r, p)
}

func writeAnswer(w http.ResponseWriter, a answer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Erp-Model", a.model)
	w.Header().Set("Erp-Model-Cost", fmt.Sprint(int64(a.cost)))
	w.Header().Set("Erp-Model-Cost-Currency", a.currency)
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body) // the client went away if this fails; the call is recorded already
}

func (g *Gateway) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return DefaultTimeout
}

// ownTx runs the work in a transaction of p's tenant of its own; the
// ledger names p itself, so no actor is set.
func (g *Gateway) ownTx(p reqctx.Principal) inTx {
	return func(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
		tx, err := database.BeginOr(ctx, g.Pool, g.Begin, database.Scope{Role: g.AppRole, TenantID: p.TenantID})
		if err != nil {
			return fmt.Errorf("agents: begin: %w", err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after the commit; before it, fn's error is the one returned
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("agents: commit: %w", err)
		}
		return nil
	}
}

// Transport is the gateway for the server's own model calls (capture),
// made inside a unit of work: the request's principal calls, and the
// checks and the ledger run in transactions of their own. A model SDK's
// client uses it as its HTTP transport.
func (g *Gateway) Transport() http.RoundTripper { return inProcess{g} }

type inProcess struct{ g *Gateway }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents: model call outside a unit of work: %w", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("agents: read model request: %w", err)
	}
	session, err := sessionOf(req.Header)
	if err != nil {
		return nil, err
	}
	// The checks and the ledger run in transactions of their own, not the
	// request's: the call's reservation must hold against other calls
	// while it is in flight, and a capture that fails afterwards still
	// leaves the call on record (ADR-0036).
	a, err := t.g.serve(ctx, p, call{body: body, betas: betas(req.Header), session: session, check: t.g.ownTx(p), record: t.g.ownTx(p)})
	if err != nil {
		return nil, err
	}
	h := http.Header{"Content-Type": {"application/json"}}
	return &http.Response{StatusCode: a.status, Header: h, Body: io.NopCloser(bytes.NewReader(a.body)), Request: req}, nil
}
