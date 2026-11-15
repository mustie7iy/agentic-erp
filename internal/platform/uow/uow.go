// Package uow runs every authenticated request as one unit of work
// (ADR-0014): one database transaction scoped to the tenant and actor, the
// principal resolved and required active, idempotency for unsafe methods in
// the same transaction (ADR-0008), audit entries written last (ADR-0011) and
// the response sent only after commit. Act is the same unit of work for a
// job that runs without a request: a worker round or a workflow activity.
package uow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// MaxBody is the largest request body accepted (04-api-design §4.11).
const MaxBody = 5 << 20

// Retention is how long idempotency keys are kept (ADR-0008).
const Retention = 7 * 24 * time.Hour

// replayed headers: what a client needs to use a replayed response.
var replayed = []string{"Content-Type", "Etag", "Location"}

// Middleware is the unit of work.
type Middleware struct {
	Pool *database.Pool
	Role string // erp_app
	Log  *slog.Logger
	Now  func() time.Time
	// Begin starts the request transaction; nil means Pool.Begin. Tests
	// replace it to inject database failures.
	Begin database.BeginFunc
	// Sessions checks a person's session (identity.Sessions, ADR-0031);
	// nil checks none.
	Sessions Sessions
	// RequireBoundAgents accepts an agent's token only bound to its client
	// certificate by a provider that binds tokens (production, ADR-0033).
	RequireBoundAgents bool
	// Agents admits an agent's request in its transaction: the tenant's AI
	// kill switch (agents.Admit, ADR-0036), so an agent stopped mid-run
	// cannot go on acting; nil admits every agent.
	Agents func(ctx context.Context, tx pgx.Tx) error
	// Snapshot reports whether a request reads as of one database state
	// (the contract's x-snapshot, which marks GETs and reads by
	// x-autonomy: read alone): its transaction then runs at REPEATABLE
	// READ, so every statement sees the state of the first, as a cash
	// position read from several contexts needs (ADR-0051 §6). A marked
	// POST, such as a depreciation run's preview, still writes its
	// idempotency record, read-write at that level: only a concurrent
	// request of the same key can then fail to serialise (503, retried
	// with the key). Nil keeps READ COMMITTED for every request.
	Snapshot func(*http.Request) bool
}

// Sessions checks a person's session: its lifetime and idle timeout. It
// runs before the request's transaction begins and reads what it needs in
// its own, so a request never holds one connection while it waits for
// another.
type Sessions interface {
	Check(ctx context.Context, c reqctx.Claims) error
}

func (m *Middleware) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, m.Pool, m.Begin, s)
}

// Unsafe reports whether requests of the method change state and carry a
// body: the unit of work reads, bounds and hashes those bodies.
func Unsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// Wrap runs next inside the unit of work. It needs authenticated claims in
// the request context (authn.Middleware).
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := m.serve(w, r, next); err != nil {
			// A safe method's transaction is read-only, as errorHandler
			// translates its errors (ADR-0040).
			from := problem.From
			if !Unsafe(r.Method) {
				from = problem.FromReadOnly
			}
			p := from(err)
			if p.Status >= 500 {
				m.Log.ErrorContext(r.Context(), "request failed", "error", err)
			}
			problem.Write(w, r, p)
		}
	})
}

type request struct {
	key  string
	hash []byte
}

// readRequest validates the idempotency key and hashes the request.
func readRequest(r *http.Request, w http.ResponseWriter) (*request, error) {
	if !Unsafe(r.Method) {
		return nil, nil
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, problem.PreconditionRequired("Idempotency-Key")
	}
	if len(key) < 8 || len(key) > 255 {
		return nil, problem.BadRequest("request.invalid_idempotency_key", "Idempotency-Key must be 8 to 255 characters.",
			problem.FieldError{Field: "Idempotency-Key", Code: "invalid_length", Message: "8 to 255 characters"})
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		return nil, problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large",
			"The request body exceeds 5 MB; large documents go to upload URLs.").WithCause(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h := sha256.New()
	h.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n"))
	h.Write(body)
	return &request{key: key, hash: h.Sum(nil)}, nil
}

func (m *Middleware) serve(w http.ResponseWriter, r *http.Request, next http.Handler) error {
	who, err := callerOf(r.Context())
	if err != nil {
		return err
	}
	req, err := readRequest(r, w)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if err := m.admit(ctx, who); err != nil {
		return err
	}
	tx, err := m.begin(ctx, m.scope(r, who.tenant(), req == nil))
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // no-op after commit

	principal, err := who.resolve(ctx, tx)
	if err != nil {
		return err
	}
	if err := m.authorize(ctx, tx, who, principal); err != nil {
		return err
	}
	info := reqctx.InfoFrom(ctx)
	info.TenantID, info.PrincipalID = principal.TenantID, principal.ID
	if err := database.SetActor(ctx, tx, principal.ID); err != nil {
		return err
	}
	if req != nil {
		done, err := m.claimKey(ctx, tx, principal, req, w)
		if err != nil || done {
			return err
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT uow"); err != nil {
			return fmt.Errorf("uow: savepoint: %w", err)
		}
	}

	ctx, buf, committed := handlerContext(ctx, tx, principal, req == nil)
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	ctx = context.WithValue(ctx, omitKey{}, rec)
	next.ServeHTTP(rec, r.WithContext(ctx))

	if err := m.finish(ctx, tx, principal, req, rec, buf); err != nil {
		return err
	}
	if rec.status < http.StatusBadRequest { // finish committed the request's work
		committed.Run()
	}
	rec.send(w)
	return nil
}

// scope is the request's transaction: read-only for a safe method, at
// REPEATABLE READ for one that reads as of one database state (Snapshot).
func (m *Middleware) scope(r *http.Request, tenant uuid.UUID, safe bool) database.Scope {
	s := database.Scope{Role: m.Role, TenantID: tenant, ReadOnly: safe}
	if m.Snapshot != nil && m.Snapshot(r) {
		s.Isolation = pgx.RepeatableRead
	}
	return s
}

// handlerContext is what the handler runs with: the transaction, the
// principal, read-only for a safe method, the audit buffer finish writes
// and what runs once the work committed.
func handlerContext(ctx context.Context, tx pgx.Tx, principal reqctx.Principal, readOnly bool) (context.Context, *audit.Buffer, *reqctx.Commit) {
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal)
	if readOnly {
		ctx = reqctx.WithReadOnly(ctx)
	}
	ctx, buf := audit.WithBuffer(ctx)
	ctx, committed := reqctx.WithCommit(ctx)
	return ctx, buf, committed
}

// admit checks what the token alone decides, before the transaction. A
// bound token without its certificate is refused before its session is
// touched: replayed by whoever stole it, it would otherwise keep its
// owner's session alive (ADR-0033).
func (m *Middleware) admit(ctx context.Context, who caller) error {
	if who.acting != nil {
		return nil // no token: work in the process (callerOf)
	}
	claims := who.claims
	if err := authn.CheckBinding(claims, reqctx.ClientCertFrom(ctx), false); err != nil {
		m.Log.WarnContext(ctx, "certificate binding refused", "issuer", claims.Issuer, "subject", claims.Subject, "bound", true)
		return err
	}
	if m.Sessions != nil {
		return m.Sessions.Check(ctx, claims)
	}
	return nil
}

// authorize checks what the token must prove for the principal it
// resolved to, and whether an agent may act at all. Multi-factor is asked
// of people only: services and agents use their own credentials, which
// prove no second factor (ADR-0031). identity.Sessions applies the same
// rule to decide which tokens start or touch a session; the two stay in
// step.
func (m *Middleware) authorize(ctx context.Context, tx pgx.Tx, who caller, principal reqctx.Principal) error {
	claims := who.claims
	if who.acting != nil {
		// Work in the process proves nothing of a token: it acts as the
		// principal it was set up by, as uow.Act does, and an agent is
		// still held by the kill switch.
		return m.admitAgent(ctx, tx, principal)
	}
	if principal.Kind == reqctx.KindHuman {
		if err := authn.CheckMFA(claims); err != nil {
			return err
		}
	}
	if m.RequireBoundAgents && principal.Kind == reqctx.KindAgent {
		if err := authn.CheckBinding(claims, reqctx.ClientCertFrom(ctx), true); err != nil {
			m.Log.WarnContext(ctx, "certificate binding refused", "principal_id", principal.ID, "kind", principal.Kind, "bound", claims.CertThumbprint != "")
			return err
		}
	}
	return m.admitAgent(ctx, tx, principal)
}

// admitAgent admits an agent's request through the tenant's AI kill
// switch (Agents); a person or service passes.
func (m *Middleware) admitAgent(ctx context.Context, tx pgx.Tx, principal reqctx.Principal) error {
	if m.Agents != nil && principal.Kind == reqctx.KindAgent {
		return m.Agents(ctx, tx)
	}
	return nil
}

// caller is who a request comes from: a verified token's claims, or the
// principal work in the process acts as (reqctx.WithActing, which only
// the loopback carries in: no request can set it; ADR-0076 §5).
type caller struct {
	claims reqctx.Claims
	acting *reqctx.Principal
}

// callerOf is the request's caller; neither claims nor an acting
// principal is 401.
func callerOf(ctx context.Context) (caller, error) {
	if c, ok := reqctx.ClaimsFrom(ctx); ok {
		return caller{claims: c}, nil
	}
	if p, ok := reqctx.ActingFrom(ctx); ok {
		return caller{acting: &p}, nil
	}
	return caller{}, problem.Unauthorized("A bearer token is required.")
}

// tenant is the tenant the caller acts in.
func (c caller) tenant() uuid.UUID {
	if c.acting != nil {
		return c.acting.TenantID
	}
	return c.claims.TenantID
}

// resolve finds the caller's active principal in an active tenant: the
// token's subject of its issuer, or the acting principal by its id.
func (c caller) resolve(ctx context.Context, tx pgx.Tx) (reqctx.Principal, error) {
	if c.acting != nil {
		return resolvePrincipal(ctx, tx, c.acting.TenantID, `p.id = $2`, c.acting.ID)
	}
	return resolve(ctx, tx, c.claims)
}

// Act runs fn as actor in tx: the unit of work of a job that has no request
// (a worker round, a workflow activity, provisioning). It records the actor
// for the row triggers, puts the transaction, the actor and an audit buffer
// in fn's context, and writes fn's audit entries (ADR-0011) once fn has
// succeeded, stamped now (the wall clock when now is nil). The caller
// begins the transaction, reads what decides the actor and commits, so its
// errors keep the context only it knows. Middleware does not use it: a
// request's audit flush depends on the response status (finish).
func Act(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now func() time.Time, fn func(ctx context.Context) error) error {
	if err := database.SetActor(ctx, tx, actor.ID); err != nil {
		return err
	}
	ctx, buf := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor))
	if err := fn(ctx); err != nil {
		return err
	}
	return buf.Flush(ctx, tx, actor, reqctx.TraceID(ctx), clock.Now(now))
}

// outcomeStatus lists the 4xx statuses a business outcome may have: only
// candidates, as businessOutcome decides (read it, never this map). Other
// 4xx answers reject the request itself (400, 401, 403, 405, 412, 413,
// 428, 429): nothing is stored, and a corrected retry with the same key
// runs.
var outcomeStatus = map[int]bool{
	http.StatusNotFound: true, http.StatusConflict: true, http.StatusGone: true, http.StatusUnprocessableEntity: true,
}

// businessOutcome reports whether a failed answer is a business outcome,
// stored with the key so a retry gets the same answer. An answer carrying
// Retry-After is not, whatever its status: it says the same request may
// succeed later (409 request.in_progress of a lock timeout, as 429 and 503
// do), so storing it would replay it to the very retry it invites
// (ADR-0008, as built). It is the one place that decides, for every
// problem that sets RetryAfter.
func businessOutcome(rec *recorder) bool {
	return outcomeStatus[rec.status] && rec.header.Get("Retry-After") == ""
}

// finish decides what the transaction keeps:
//   - 2xx/3xx: everything, plus the audit entries and the stored response;
//   - a business outcome on an unsafe request: only the key, the answer
//     and the audit of a refusal (audit.Refuse), so a refused write is
//     recorded although its work rolls back, once, with the answer a
//     retry gets (ADR-0046);
//   - anything else, a retryable answer included: nothing (the deferred
//     rollback), so the key is free for the retry.
func (m *Middleware) finish(ctx context.Context, tx pgx.Tx, p reqctx.Principal, req *request, rec *recorder, buf *audit.Buffer) error {
	failed := rec.status >= 400
	if failed && (req == nil || !businessOutcome(rec)) {
		return nil
	}
	flush := buf.Flush
	if failed {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT uow"); err != nil {
			return fmt.Errorf("uow: rollback to savepoint: %w", err)
		}
		flush = buf.FlushRefusals
	}
	if err := flush(ctx, tx, p, reqctx.TraceID(ctx), m.Now()); err != nil {
		// The audit trail rejecting an entry is never the client's fault: a
		// lost connection stays the retryable 503 it translates to, anything
		// else is internal.
		if p := problem.From(err); p.Status >= http.StatusInternalServerError {
			return p
		}
		return problem.Internal(err)
	}
	if req != nil {
		if err := m.storeResponse(ctx, tx, p, req, rec); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("uow: commit: %w", err)
	}
	return nil
}

// resolve finds the active principal for the token in an active tenant. A
// principal is the subject of one issuer (ADR-0010, F7): every trusted
// issuer may name any tenant, so a subject alone would let one tenant's
// identity provider sign in as another tenant's principal whose subject it
// also issues.
func resolve(ctx context.Context, tx pgx.Tx, c reqctx.Claims) (reqctx.Principal, error) {
	return resolvePrincipal(ctx, tx, c.TenantID, `p.external_issuer = $2 AND p.external_subject = $3`, c.Issuer, c.Subject)
}

// resolvePrincipal finds the tenant's principal the condition on p
// selects ($2 on), required active in an active tenant.
func resolvePrincipal(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, cond string, args ...any) (reqctx.Principal, error) {
	var tenantStatus string
	var id *uuid.UUID
	var kind, status *string
	err := tx.QueryRow(ctx, `SELECT t.status, p.id, p.kind, p.status FROM tenancy.tenant t
		LEFT JOIN identity.principal p ON p.tenant_id = t.id AND `+cond+`
		WHERE t.id = $1`, append([]any{tenant}, args...)...).Scan(&tenantStatus, &id, &kind, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return reqctx.Principal{}, problem.Unauthorized("The token's tenant is not known here.")
	case err != nil:
		return reqctx.Principal{}, fmt.Errorf("uow: resolve principal in tenant %s: %w", tenant, err)
	case tenantStatus != "active":
		return reqctx.Principal{}, problem.Forbidden("tenancy.tenant_inactive", "The tenant is "+tenantStatus+".")
	case id == nil:
		return reqctx.Principal{}, problem.Forbidden("identity.principal_unknown", "The token's subject is not provisioned in this tenant.")
	case *status != "active":
		return reqctx.Principal{}, problem.Forbidden("identity.principal_blocked", "The principal is "+*status+".")
	}
	return reqctx.Principal{ID: *id, Kind: *kind, TenantID: tenant}, nil
}

// claimKey inserts the idempotency key, or answers from an earlier request
// with the same key: the stored response (done = true), 422 when the key was
// used for a different request, or 409 while that request is still running.
func (m *Middleware) claimKey(ctx context.Context, tx pgx.Tx, p reqctx.Principal, req *request, w http.ResponseWriter) (bool, error) {
	var fresh bool
	err := tx.QueryRow(ctx, `INSERT INTO platform.idempotency_key AS k (tenant_id, key, principal_id, request_hash, status, expires_at)
		VALUES ($1, $2, $3, $4, 'in_progress', $5)
		ON CONFLICT (tenant_id, key) DO UPDATE SET principal_id = EXCLUDED.principal_id, request_hash = EXCLUDED.request_hash,
			status = 'in_progress', response_code = NULL, response_body = NULL, response_bytes = NULL, response_headers = '{}',
			created_at = now(), expires_at = EXCLUDED.expires_at
		WHERE k.expires_at < now()
		RETURNING true`, p.TenantID, req.key, p.ID, req.hash, m.Now().Add(Retention)).Scan(&fresh)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		// Includes 55P03: the first request still holds the key → 409 Retry-After.
		return false, fmt.Errorf("uow: claim idempotency key: %w", err)
	}
	var owner uuid.UUID
	var hash []byte
	var code int
	var body []byte
	var headers map[string]string
	// An answer is kept as JSON or, when its media type is not JSON, as
	// bytes (storedBody): either is the body replayed.
	if err := tx.QueryRow(ctx, `SELECT principal_id, request_hash, coalesce(response_code, 0),
			coalesce(response_bytes, convert_to(coalesce(response_body::text, ''), 'UTF8')), response_headers
		FROM platform.idempotency_key WHERE tenant_id = $1 AND key = $2`, p.TenantID, req.key).Scan(&owner, &hash, &code, &body, &headers); err != nil {
		return false, fmt.Errorf("uow: read idempotency key: %w", err)
	}
	if owner != p.ID || !bytes.Equal(hash, req.hash) {
		return false, problem.Unprocessable("request.idempotency_key_reused",
			"This Idempotency-Key was already used for a different request.")
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(code)
	// A failed write means the client has gone: the replay changes nothing
	// and the status is already sent, so there is no one left to tell.
	_, _ = w.Write(body)
	return true, nil
}

func (m *Middleware) storeResponse(ctx context.Context, tx pgx.Tx, p reqctx.Principal, req *request, rec *recorder) error {
	headers := map[string]string{}
	for _, h := range replayed {
		if v := rec.header.Get(h); v != "" {
			headers[h] = v
		}
	}
	body, raw, err := storedBody(rec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform.idempotency_key SET status = 'completed', response_code = $3,
		response_body = $4::jsonb, response_bytes = $6, response_headers = $5 WHERE tenant_id = $1 AND key = $2`,
		p.TenantID, req.key, rec.status, body, headers, raw); err != nil {
		return fmt.Errorf("uow: store response: %w", err)
	}
	return nil
}

// storedBody is the response body kept with the idempotency key: NULL for
// an empty body, and without the fields the handler omitted. A body whose
// declared media type is not JSON (an Excel workbook, T95d) is kept as
// its bytes (raw); any other must be JSON.
func storedBody(rec *recorder) (body any, raw []byte, err error) {
	if rec.body.Len() == 0 {
		return nil, nil, nil
	}
	if ct := rec.header.Get("Content-Type"); ct != "" && !jsonMedia(ct) && len(rec.omit) == 0 {
		return nil, bytes.Clone(rec.body.Bytes()), nil
	}
	if !json.Valid(rec.body.Bytes()) {
		return nil, nil, errors.New("uow: response body is not JSON")
	}
	if len(rec.omit) == 0 {
		return rec.body.String(), nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.body.Bytes(), &fields); err != nil {
		return nil, nil, fmt.Errorf("uow: omit fields of a response that is not an object: %w", err)
	}
	for _, f := range rec.omit {
		delete(fields, f)
	}
	// The error is safe to drop: values decoded as raw JSON are valid and
	// re-encode as they were. The keys come out sorted, as jsonb stores them.
	b, _ := json.Marshal(fields)
	return string(b), nil, nil
}

// jsonMedia reports whether the media type is JSON: application/json or
// a +json type such as application/problem+json. One that does not parse
// is not.
func jsonMedia(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

// omitKey is the context key of the request's recorder, for OmitOnReplay.
type omitKey struct{}

// OmitOnReplay keeps these top-level fields of the response body out of
// the answer stored with the idempotency key: a secret shown once (a
// webhook signing secret, ADR-0041) is not kept in the clear for the
// key's retention. A replay answers without them; a client that lost the
// answer rotates the secret. Outside a request it does nothing.
func OmitOnReplay(ctx context.Context, fields ...string) {
	if rec, ok := ctx.Value(omitKey{}).(*recorder); ok {
		rec.omit = append(rec.omit, fields...)
	}
}

// recorder buffers the response until the transaction has committed.
type recorder struct {
	omit   []string // fields left out of the stored answer (OmitOnReplay)
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	r.body.Write(b) // a bytes.Buffer write cannot fail
	return len(b), nil
}

func (r *recorder) send(w http.ResponseWriter) {
	for k, vs := range r.header {
		w.Header()[k] = vs
	}
	w.WriteHeader(r.status)
	// A failed write means the client has gone after the commit; with an
	// Idempotency-Key it replays the stored outcome, so there is nothing to
	// undo and no one left to tell.
	_, _ = w.Write(r.body.Bytes())
}
