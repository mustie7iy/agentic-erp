// Package agents is the agents context (03-data-model §3.14): agents work
// through the public API like any client (04-api-design §4.13) and record
// here, also through the API, the sessions they work in, every tool call
// they make and the recommendations they leave for people. Agents
// recommend; people decide: only a person records what was done with a
// recommendation.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Session statuses.
const (
	Running       = "running"
	Completed     = "completed"
	SessionFailed = "failed"
	Refused       = "refused"
)

// Endpoints implements the agents part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
	// Countries reads the tenant's countries, whose tax identifiers a tool
	// call's record is redacted of (tenancy.Countries); nil: IBANs and card
	// numbers only.
	Countries Countries
	Now       func() time.Time // the usage report's current month; nil: the wall clock
	// Certifications keeps the AI policy's routes on models the tenant's
	// agents are certified on (ADR-0039).
	Certifications Certifications
}

// Session is a row of agents.session.
type Session struct {
	ID           uuid.UUID
	AgentID      uuid.UUID
	Purpose      string
	Model        string
	Status       string
	InputTokens  int64
	OutputTokens int64
	Cost         money.Minor
	Currency     string
	Exponent     int
	StartedAt    time.Time
	EndedAt      *time.Time
	Version      int32
	UpdatedAt    time.Time
	// AutonomyLevel is the agent's level when the session started, on the
	// response that starts it only: the agent offers its model the tools
	// of that level (ADR-0036).
	AutonomyLevel *int32
}

const sessionSelect = `SELECT s.id, s.agent_id, s.purpose, s.model, s.status, s.input_tokens, s.output_tokens, s.cost, s.cost_currency, c.exponent,
		s.started_at, s.ended_at, s.version, s.updated_at
	FROM agents.session s JOIN ref.currency c ON c.code = s.cost_currency`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.AgentID, &s.Purpose, &s.Model, &s.Status, &s.InputTokens, &s.OutputTokens, &s.Cost, &s.Currency, &s.Exponent,
		&s.StartedAt, &s.EndedAt, &s.Version, &s.UpdatedAt)
	return s, errs.Wrapf(err, "scan agent session")
}

func getSession(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Session, error) {
	q := sessionSelect + ` WHERE s.id = $1`
	if lock {
		q += ` FOR UPDATE OF s`
	}
	s, err := scanSession(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, problem.NotFoundOf("agent session")
	}
	if err != nil {
		return s, fmt.Errorf("agents: read session %s: %w", id, err)
	}
	return s, nil
}

// ownRunning loads a session the caller may write to: its own, running.
func ownRunning(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, notFound func() error) (Session, error) {
	s, err := getSession(ctx, tx, id, true)
	if problem.From(err).Status == 404 {
		return s, notFound()
	}
	switch {
	case err != nil:
		return s, err
	case s.AgentID != p.ID:
		return s, problem.Forbidden("agents.not_your_session", "Only the session's agent writes to it.")
	case s.Status != Running:
		return s, problem.Conflict("agents.session_ended", "The session has "+s.Status+".")
	}
	return s, nil
}

// Sessions answers other contexts whether a session is the calling
// agent's and running, refusing it as a recommendation's session is
// refused: the payments context's proposals for statement lines name the
// session they are made in (T49g, ADR-0047).
type Sessions struct{}

// Running checks the session, locking it as a recommendation does.
func (Sessions) Running(ctx context.Context, id uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	_, err = ownRunning(ctx, tx, p, id, func() error {
		return problem.ReferenceNotFound("session_id", "agent session", "a running session of yours")
	})
	return err
}

// StartSession opens a running session for the calling agent.
func StartSession(ctx context.Context, purpose, model string) (Session, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Session{}, err
	}
	if p.Kind != reqctx.KindAgent {
		return Session{}, problem.Forbidden("agents.not_an_agent", "Only agents start agent sessions.")
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO agents.session (tenant_id, id, agent_id, purpose, model, status, cost_currency)
		VALUES ($1, $2, $3, $4, $5, 'running', 'USD')`, p.TenantID, id, p.ID, purpose, model); err != nil {
		return Session{}, fmt.Errorf("agents: insert session %s: %w", id, err)
	}
	s, err := getSession(ctx, tx, id, false)
	if err != nil {
		return Session{}, err
	}
	level, err := authz.Level(ctx, tx, p.ID)
	if err != nil {
		return Session{}, err
	}
	s.AutonomyLevel = &level
	return s, audit.Add(ctx, audit.Entry{Action: "agents.session.start", ObjectType: "agents.session", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"purpose": purpose, "model": model}})
}

// Usage is how a session ended. Its tokens and cost are not the
// client's to say: they are the sum of the session's model calls through
// the gateway (ADR-0036).
type Usage struct {
	Status string
}

// EndSession ends the caller's running session.
func EndSession(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, u Usage) (Session, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Session{}, err
	}
	s, err := ownRunning(ctx, tx, p, id, func() error { return problem.NotFoundOf("agent session") })
	if err != nil {
		return Session{}, err
	}
	if err := ifMatch(s.Version); err != nil {
		return Session{}, err
	}
	if !slices.Contains([]string{Completed, SessionFailed, Refused}, u.Status) {
		return Session{}, problem.Unprocessable("agents.invalid_status", "A session ends completed, failed or refused.",
			problem.FieldError{Field: "status", Code: "invalid", Message: "completed, failed or refused"})
	}
	if _, err := tx.Exec(ctx, `UPDATE agents.session SET status = $2, ended_at = now(), version = version + 1 WHERE id = $1`, id, u.Status); err != nil {
		return Session{}, fmt.Errorf("agents: end session %s: %w", id, err)
	}
	ended, err := getSession(ctx, tx, id, false)
	if err != nil {
		return Session{}, err
	}
	return ended, audit.Add(ctx, audit.Entry{Action: "agents.session.end", ObjectType: "agents.session", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"status": u.Status, "input_tokens": ended.InputTokens, "output_tokens": ended.OutputTokens}})
}

// ToolCall is a row of agents.tool_call.
type ToolCall struct {
	SessionID     uuid.UUID
	Seq           int32
	Tool          string
	Input         map[string]json.RawMessage
	ResultSummary *string
	IsError       bool
	CalledAt      time.Time
}

const toolCallColumns = `session_id, seq, tool, input, result_summary, is_error, called_at`

func scanToolCall(row pgx.Row) (ToolCall, error) {
	var c ToolCall
	err := row.Scan(&c.SessionID, &c.Seq, &c.Tool, &c.Input, &c.ResultSummary, &c.IsError, &c.CalledAt)
	return c, errs.Wrapf(err, "scan tool call")
}

// RecordToolCall appends a tool call to the caller's running session, its
// input and summary minimized (ADR-0036). Tool calls are not audited one by
// one: the API calls they describe are.
func RecordToolCall(ctx context.Context, session uuid.UUID, tool string, input map[string]json.RawMessage, summary *string, isError bool,
	countries Countries) (ToolCall, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ToolCall{}, err
	}
	if _, err := ownRunning(ctx, tx, p, session, func() error { return problem.NotFoundOf("agent session") }); err != nil {
		return ToolCall{}, err
	}
	input, summary, err = minimize(ctx, tx, countries, input, summary)
	if err != nil {
		return ToolCall{}, err
	}
	c, err := scanToolCall(tx.QueryRow(ctx, `INSERT INTO agents.tool_call (tenant_id, session_id, seq, tool, input, result_summary, is_error)
		SELECT $1, $2, coalesce(max(seq), 0) + 1, $3, $4, $5, $6 FROM agents.tool_call WHERE session_id = $2
		RETURNING `+toolCallColumns, p.TenantID, session, tool, input, summary, isError))
	if err != nil {
		return c, fmt.Errorf("agents: record tool call on session %s: %w", session, err)
	}
	return c, nil
}

// Recommendation is a row of agents.recommendation with its session's agent.
type Recommendation struct {
	ID         uuid.UUID
	SessionID  uuid.UUID
	AgentID    uuid.UUID
	ObjectType string
	ObjectID   uuid.UUID
	Kind       string
	Content    string
	Outcome    *string
	DecidedBy  *uuid.UUID
	Version    int32
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const recommendationSelect = `SELECT r.id, r.session_id, s.agent_id, r.object_type, r.object_id, r.kind, r.content, r.outcome, r.decided_by, r.version,
		r.created_at, r.updated_at
	FROM agents.recommendation r JOIN agents.session s ON s.tenant_id = r.tenant_id AND s.id = r.session_id`

func scanRecommendation(row pgx.Row) (Recommendation, error) {
	var r Recommendation
	err := row.Scan(&r.ID, &r.SessionID, &r.AgentID, &r.ObjectType, &r.ObjectID, &r.Kind, &r.Content, &r.Outcome, &r.DecidedBy, &r.Version,
		&r.CreatedAt, &r.UpdatedAt)
	return r, errs.Wrapf(err, "scan recommendation")
}

func getRecommendation(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Recommendation, error) {
	q := recommendationSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRecommendation(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, problem.NotFoundOf("recommendation")
	}
	if err != nil {
		return r, fmt.Errorf("agents: read recommendation %s: %w", id, err)
	}
	return r, nil
}

// NewRecommendation is a recommendation as an agent makes it.
type NewRecommendation struct {
	SessionID        uuid.UUID
	ObjectType, Kind string
	ObjectID         uuid.UUID
	Content          string
}

// Recommend records an open recommendation in the caller's running session.
func Recommend(ctx context.Context, in NewRecommendation) (Recommendation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Recommendation{}, err
	}
	if _, err := ownRunning(ctx, tx, p, in.SessionID, func() error {
		return problem.ReferenceNotFound("session_id", "agent session", "a running session of yours")
	}); err != nil {
		return Recommendation{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO agents.recommendation (tenant_id, id, session_id, object_type, object_id, kind, content)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, id, in.SessionID, in.ObjectType, in.ObjectID, in.Kind, in.Content); err != nil {
		return Recommendation{}, fmt.Errorf("agents: insert recommendation %s: %w", id, err)
	}
	r, err := getRecommendation(ctx, tx, id, false)
	if err != nil {
		return Recommendation{}, err
	}
	return r, record(ctx, "agents.recommendation.create", id, map[string]any{"object_type": r.ObjectType, "object_id": r.ObjectID, "kind": r.Kind},
		event(r, "recommendation_made", map[string]any{"agent_id": r.AgentID, "content": r.Content}))
}

// Outcomes people record for recommendations.
var outcomes = []string{"accepted", "overridden", "ignored"}

// Decide records what a person did with an open recommendation.
func Decide(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, outcome, comment string) (Recommendation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Recommendation{}, err
	}
	r, err := getRecommendation(ctx, tx, id, true)
	if err != nil {
		return Recommendation{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Recommendation{}, err
	}
	if !slices.Contains(outcomes, outcome) {
		return Recommendation{}, problem.Unprocessable("agents.invalid_outcome", "A recommendation is accepted, overridden or ignored.",
			problem.FieldError{Field: "outcome", Code: "invalid", Message: strings.Join(outcomes, ", ")})
	}
	if r.Outcome != nil {
		return Recommendation{}, problem.Conflict("agents.already_decided", "The recommendation was already "+*r.Outcome+".")
	}
	if _, err := tx.Exec(ctx, `UPDATE agents.recommendation SET outcome = $2, decided_by = $3, version = version + 1 WHERE id = $1`,
		id, outcome, p.ID); err != nil {
		return Recommendation{}, fmt.Errorf("agents: decide recommendation %s: %w", id, err)
	}
	decided, err := getRecommendation(ctx, tx, id, false)
	if err != nil {
		return Recommendation{}, err
	}
	return decided, record(ctx, "agents.recommendation.decide", id, map[string]any{"outcome": outcome, "comment": comment},
		event(decided, "recommendation_decided", map[string]any{"outcome": outcome, "decided_by": p.ID}))
}

func event(r Recommendation, name string, payload map[string]any) outbox.Event {
	payload["recommendation_id"], payload["session_id"], payload["object_type"], payload["object_id"], payload["kind"] =
		r.ID, r.SessionID, r.ObjectType, r.ObjectID, r.Kind
	return outbox.Event{Context: "agents", Name: name, AggregateType: "agents.recommendation", AggregateID: r.ID, Payload: payload}
}

func record(ctx context.Context, action string, id uuid.UUID, detail map[string]any, ev outbox.Event) error {
	return audit.Record(ctx, action, "agents.recommendation", id, detail, ev)
}

func sessionToAPI(s Session) gen.AgentSession {
	out := gen.AgentSession{ID: s.ID, Version: s.Version, AgentID: s.AgentID, Purpose: s.Purpose, Model: s.Model, Status: s.Status,
		InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt}
	if s.EndedAt != nil {
		out.EndedAt.SetTo(*s.EndedAt)
		out.Cost.SetTo(apitypes.Money(s.Cost, s.Currency, s.Exponent))
	}
	if s.AutonomyLevel != nil {
		out.AutonomyLevel.SetTo(*s.AutonomyLevel)
	}
	return out
}

func toolCallToAPI(c ToolCall) gen.AgentToolCall {
	out := gen.AgentToolCall{SessionID: c.SessionID, Seq: c.Seq, Tool: c.Tool, Input: gen.AgentToolCallInput{}, IsError: c.IsError, CalledAt: c.CalledAt}
	for k, v := range c.Input {
		out.Input[k] = jx.Raw(v)
	}
	if c.ResultSummary != nil {
		out.ResultSummary.SetTo(*c.ResultSummary)
	}
	return out
}

func recommendationToAPI(r Recommendation) gen.Recommendation {
	out := gen.Recommendation{ID: r.ID, Version: r.Version, SessionID: r.SessionID, AgentID: r.AgentID, ObjectType: r.ObjectType, ObjectID: r.ObjectID,
		Kind: r.Kind, Content: r.Content, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.Outcome != nil {
		out.Outcome.SetTo(*r.Outcome)
		out.DecidedBy.SetTo(*r.DecidedBy)
	}
	return out
}

// StartAgentSession implements POST /v1/agent-sessions.
func (e *Endpoints) StartAgentSession(ctx context.Context, req *gen.AgentSessionStart, _ gen.StartAgentSessionParams) (gen.StartAgentSessionRes, error) {
	s, err := StartSession(ctx, req.Purpose, req.Model)
	if err != nil {
		return nil, err
	}
	return &gen.AgentSessionHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Location: gen.NewOptString("/v1/agent-sessions/" + s.ID.String()),
		Response: sessionToAPI(s)}, nil
}

// GetAgentSession implements GET /v1/agent-sessions/{id}.
func (e *Endpoints) GetAgentSession(ctx context.Context, params gen.GetAgentSessionParams) (gen.GetAgentSessionRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	s, err := getSession(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetAgentSessionOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: sessionToAPI(s)}, nil
}

var sessionSort = pagination.Sort{Default: "started_at", DefaultDesc: true, Fields: []pagination.Field{{Name: "started_at", Column: "s.started_at", Type: "timestamptz"}}}

func instant(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.999999Z07:00") }

// ListAgentSessions implements GET /v1/agent-sessions.
func (e *Endpoints) ListAgentSessions(ctx context.Context, params gen.ListAgentSessionsParams) (gen.ListAgentSessionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	agent := apitypes.Ptr[uuid.UUID](params.AgentID)
	ks, err := e.Pages.Keyset(sessionSort, fmt.Sprintf("listAgentSessions|%s|%s", agent, params.Status.Value), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (`+sessionSelect+` WHERE ($1::uuid IS NULL OR s.agent_id = $1) AND ($2 = '' OR s.status = $2)) s WHERE true`,
		[]any{agent, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	sessions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Session, error) { return scanSession(r) })
	if err != nil {
		return nil, fmt.Errorf("agents: list sessions: %w", err)
	}
	items, next := apitypes.PageOf(ks, sessions, func(s Session) (string, uuid.UUID) { return instant(s.StartedAt), s.ID }, sessionToAPI)
	return &gen.AgentSessionPageHeaders{Response: gen.AgentSessionPage{Items: items, NextPageToken: next}}, nil
}

// EndAgentSession implements POST /v1/agent-sessions/{id}:end.
func (e *Endpoints) EndAgentSession(ctx context.Context, req *gen.AgentSessionEnd, params gen.EndAgentSessionParams) (gen.EndAgentSessionRes, error) {
	s, err := EndSession(ctx, params.ID, etag.IfMatch(params.IfMatch), Usage{Status: req.Status})
	if err != nil {
		return nil, err
	}
	return &gen.EndAgentSessionOKHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: sessionToAPI(s)}, nil
}

type seqCursor struct {
	Seq int32 `json:"s"`
}

// ListAgentToolCalls implements GET /v1/agent-sessions/{id}/tool-calls.
func (e *Endpoints) ListAgentToolCalls(ctx context.Context, params gen.ListAgentToolCallsParams) (gen.ListAgentToolCallsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	scope := "listAgentToolCalls|" + params.ID.String()
	var after int32
	if params.PageToken.Set {
		var c seqCursor
		if err := e.Pages.Decode(scope, params.PageToken.Value, &c); err != nil {
			return nil, err
		}
		after = c.Seq
	}
	if _, err := getSession(ctx, tx, params.ID, false); err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT `+toolCallColumns+` FROM agents.tool_call WHERE session_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`,
		params.ID, after, size+1)
	calls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ToolCall, error) { return scanToolCall(r) })
	if err != nil {
		return nil, fmt.Errorf("agents: list tool calls of session %s: %w", params.ID, err)
	}
	page := gen.AgentToolCallPage{Items: []gen.AgentToolCall{}}
	if len(calls) > size {
		calls = calls[:size]
		tok, _ := e.Pages.Encode(scope, seqCursor{Seq: calls[size-1].Seq}) // a number always encodes
		page.NextPageToken.SetTo(tok)
	}
	for _, c := range calls {
		page.Items = append(page.Items, toolCallToAPI(c))
	}
	return &gen.AgentToolCallPageHeaders{Response: page}, nil
}

// RecordAgentToolCall implements POST /v1/agent-sessions/{id}/tool-calls.
func (e *Endpoints) RecordAgentToolCall(ctx context.Context, req *gen.AgentToolCallCreate, params gen.RecordAgentToolCallParams) (gen.RecordAgentToolCallRes, error) {
	input := map[string]json.RawMessage{}
	for k, v := range req.Input {
		input[k] = json.RawMessage(v)
	}
	var summary *string
	if s, ok := req.ResultSummary.Get(); ok {
		summary = &s
	}
	c, err := RecordToolCall(ctx, params.ID, req.Tool, input, summary, req.IsError.Value, e.Countries)
	if err != nil {
		return nil, err
	}
	return &gen.AgentToolCallHeaders{Response: toolCallToAPI(c)}, nil
}

// CreateRecommendation implements POST /v1/recommendations.
func (e *Endpoints) CreateRecommendation(ctx context.Context, req *gen.RecommendationCreate, _ gen.CreateRecommendationParams) (gen.CreateRecommendationRes, error) {
	r, err := Recommend(ctx, NewRecommendation{SessionID: req.SessionID, ObjectType: req.ObjectType, ObjectID: req.ObjectID, Kind: req.Kind, Content: req.Content})
	if err != nil {
		return nil, err
	}
	return &gen.RecommendationHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/recommendations/" + r.ID.String()),
		Response: recommendationToAPI(r)}, nil
}

// GetRecommendation implements GET /v1/recommendations/{id}.
func (e *Endpoints) GetRecommendation(ctx context.Context, params gen.GetRecommendationParams) (gen.GetRecommendationRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := getRecommendation(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetRecommendationOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: recommendationToAPI(r)}, nil
}

var recommendationSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{{Name: "created_at", Column: "r.created_at", Type: "timestamptz"}}}

// ListRecommendations implements GET /v1/recommendations.
func (e *Endpoints) ListRecommendations(ctx context.Context, params gen.ListRecommendationsParams) (gen.ListRecommendationsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	session, object := apitypes.Ptr[uuid.UUID](params.SessionID), apitypes.Ptr[uuid.UUID](params.ObjectID)
	scope := fmt.Sprintf("listRecommendations|%s|%s|%s|%s", session, params.ObjectType.Value, object, params.Outcome.Value)
	ks, err := e.Pages.Keyset(recommendationSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (`+recommendationSelect+` WHERE ($1::uuid IS NULL OR r.session_id = $1) AND ($2 = '' OR r.object_type = $2)
		AND ($3::uuid IS NULL OR r.object_id = $3) AND ($4 = '' OR ($4 = 'open' AND r.outcome IS NULL) OR r.outcome = $4)) r WHERE true`,
		[]any{session, params.ObjectType.Value, object, params.Outcome.Value})
	rows, _ := tx.Query(ctx, q, args...)
	recs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Recommendation, error) { return scanRecommendation(r) })
	if err != nil {
		return nil, fmt.Errorf("agents: list recommendations: %w", err)
	}
	items, next := apitypes.PageOf(ks, recs, func(r Recommendation) (string, uuid.UUID) { return instant(r.CreatedAt), r.ID }, recommendationToAPI)
	return &gen.RecommendationPageHeaders{Response: gen.RecommendationPage{Items: items, NextPageToken: next}}, nil
}

// DecideRecommendation implements POST /v1/recommendations/{id}:decide.
func (e *Endpoints) DecideRecommendation(ctx context.Context, req *gen.RecommendationDecide, params gen.DecideRecommendationParams) (gen.DecideRecommendationRes, error) {
	r, err := Decide(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Outcome, req.Comment.Value)
	if err != nil {
		return nil, err
	}
	return &gen.DecideRecommendationOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: recommendationToAPI(r)}, nil
}
