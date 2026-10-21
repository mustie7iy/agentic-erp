// Package httpapi is the request pipeline of the public API (ADR-0014):
// telemetry, authentication and the unit of work around the server
// generated from api/openapi.yaml, with authorization per operation and
// every error as an RFC 9457 problem, deprecations announced from the
// contract (ADR-0041), and the operational endpoints; and
// what the hand-written operator APIs (control plane, global directory)
// share: Operator, NotAllowed and WriteJSON. It names no context:
// internal/app assembles the contexts' handlers into the generated Handler
// and passes it to New.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/middleware"
	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/ogen-go/ogen/openapi"
	"github.com/ogen-go/ogen/validate"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Pipeline is what wraps the generated server: authentication, quotas,
// deadlines, the unit of work and telemetry.
type Pipeline struct {
	Pool     *database.Pool
	AppRole  string
	Verifier *authn.Verifier
	// Limits enforces the request quotas of 04 §4.11; nil applies none.
	Limits *ratelimit.Limiter
	// RequestTimeout is the deadline of one request, so that a saturated
	// database sheds load as 503 instead of queueing; 0 sets none.
	RequestTimeout time.Duration
	// Routes are served beside the generated server, keyed by ServeMux
	// pattern, outside authentication and the unit of work: they
	// authenticate themselves (a signed URL, for example). A malformed
	// pattern, or one that conflicts with "/", panics in New: it is wiring
	// fixed at startup, not input.
	Routes  map[string]http.Handler
	Log     *slog.Logger
	Metrics *telemetry.Metrics
	Tracer  trace.TracerProvider
	Meter   metric.MeterProvider // per-operation metrics of the generated server
	Now     func() time.Time
	// Sessions checks people's sessions in the unit of work (ADR-0031);
	// nil checks none.
	Sessions uow.Sessions
	// RequireBoundAgents accepts agents' tokens only bound to their client
	// certificates (production, ADR-0033).
	RequireBoundAgents bool
	// Agents admits an agent's request in its transaction (uow.Middleware's
	// Agents: the tenant's AI kill switch); nil admits every agent.
	Agents func(ctx context.Context, tx pgx.Tx) error
	// Loopback, when set, is given the unit of work around the generated
	// server, so handlers can read the API in-process as their caller.
	Loopback *Loopback
	// Model is the model gateway (api/model.yaml, ADR-0036), served at its
	// contract's route behind authentication but outside the unit of work:
	// no transaction is held while a model works. Nil answers 503.
	Model http.Handler
}

// New builds the public API around h, the generated Handler the contexts
// implement (internal/app assembles it).
func New(h gen.Handler, p Pipeline) (http.Handler, error) {
	srv, err := gen.NewServer(h, security{},
		gen.WithErrorHandler(errorHandler(p.Log)),
		gen.WithMiddleware(authorize(authz.New(api.MustOperations()))),
		gen.WithNotFound(func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, r, problem.NotFound("No such endpoint."))
		}),
		gen.WithMethodNotAllowed(func(w http.ResponseWriter, r *http.Request, allowed string) {
			w.Header().Set("Allow", allowed)
			problem.Write(w, r, problem.New(http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed",
				"Allowed: "+allowed+"."))
		}),
		gen.WithTracerProvider(p.Tracer),
		gen.WithMeterProvider(p.Meter),
	)
	if err != nil {
		return nil, err
	}
	unit := &uow.Middleware{Pool: p.Pool, Role: p.AppRole, Log: p.Log, Now: p.Now, Sessions: p.Sessions, RequireBoundAgents: p.RequireBoundAgents,
		Agents: p.Agents, Snapshot: snapshot(srv, api.MustOperations())}
	chain := unit.Wrap(srv)
	if p.Loopback != nil {
		p.Loopback.handler = chain
	}
	if p.Limits != nil {
		chain = p.Limits.Middleware(chain)
	}
	chain = p.Verifier.Middleware(chain)
	if p.RequestTimeout > 0 {
		chain = deadline(p.RequestTimeout, chain)
	}
	chain = announce(srv, deprecations(api.MustOperations()), chain)
	mux := http.NewServeMux()
	for pattern, route := range p.Routes {
		mux.Handle(pattern, route)
	}
	op := api.MustModelOperations()["createModelMessage"]
	mux.Handle(op.Method+" "+op.Path, modelRoute(p, unit, op.Permission))
	mux.Handle("/", chain)
	return telemetry.Middleware(p.Log, p.Metrics, p.Tracer)(mux), nil
}

// modelRoute is the model gateway behind the token check, the quotas and
// an authentication of its own, which needs the operation's permission;
// without a gateway, 503. It has no request deadline: the provider's
// client bounds a call.
func modelRoute(p Pipeline, unit *uow.Middleware, permission string) http.Handler {
	if p.Model == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, r, problem.Unavailable("No model provider is configured on this server (ERP_MODEL_API_KEY).", 0))
		})
	}
	chain := unit.Authenticated(func(ctx context.Context) error { return authz.Require(ctx, permission) }, p.Model)
	if p.Limits != nil {
		chain = p.Limits.Middleware(chain)
	}
	return p.Verifier.Middleware(chain)
}

// deadline gives each request a deadline. Work past it fails with the
// context's error, which the unit of work answers as 503 with Retry-After
// (the response is buffered until commit, so nothing is half-written).
func deadline(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// security confirms the pipeline authenticated the request; the token itself
// was verified by authn before the unit of work.
type security struct{}

func (security) HandleOidc(ctx context.Context, _ gen.OperationName, _ gen.Oidc) (context.Context, error) {
	if _, ok := reqctx.PrincipalFrom(ctx); !ok {
		return ctx, problem.Unauthorized("A bearer token is required.")
	}
	return ctx, nil
}

// authorize checks the operation's permission and agent rule.
func authorize(a *authz.Authorizer) middleware.Middleware {
	return func(req middleware.Request, next middleware.Next) (middleware.Response, error) {
		reqctx.InfoFrom(req.Context).Operation = req.OperationID
		v, _ := req.Params.Path("entity")
		entity, _ := v.(string) // absent (""): the operation's permission does not depend on it
		if err := a.AuthorizeEntity(req.Context, req.OperationID, entity); err != nil {
			return middleware.Response{}, err
		}
		return next(req)
	}
}

// errorHandler writes every error from the generated server as a problem.
func errorHandler(log *slog.Logger) ogenerrors.ErrorHandler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
		from := problem.From
		if reqctx.ReadOnly(ctx) {
			from = problem.FromReadOnly
		}
		p := toProblem(err, from)
		if p.Status >= 500 {
			log.ErrorContext(ctx, "request failed", "error", err, "trace_id", reqctx.TraceID(ctx))
		}
		problem.Write(w, r, p)
	}
}

// toProblem translates the generated server's own errors and hands the
// rest to from, the translation for the request's transaction.
func toProblem(err error, from func(error) *problem.Problem) *problem.Problem {
	var (
		p     *problem.Problem
		sec   *ogenerrors.SecurityError
		param *ogenerrors.DecodeParamError
		body  *ogenerrors.DecodeRequestError
	)
	switch {
	case errors.As(err, &p):
		return p
	case errors.Is(err, ht.ErrNotImplemented):
		return problem.New(http.StatusNotImplemented, "platform.not_implemented", "Not implemented",
			"This operation is in the contract but not implemented yet.")
	case errors.As(err, &sec):
		return problem.Unauthorized("A bearer token is required.").WithCause(err)
	case errors.As(err, &param):
		if param.In == openapi.LocationHeader && missing(param.Err) {
			return problem.PreconditionRequired(param.Name)
		}
		return problem.BadRequest("request.invalid_parameter", "Parameter "+param.Name+" is invalid.",
			problem.FieldError{Field: param.Name, Code: "invalid", Message: param.Err.Error()}).WithCause(err)
	case errors.As(err, &body):
		return problem.BadRequest("request.invalid_body", "The request body does not match the contract.").WithCause(err)
	}
	return from(err)
}

// missing reports whether a parameter error means the parameter was absent.
func missing(err error) bool {
	if errors.Is(err, validate.ErrFieldRequired) {
		return true
	}
	var v *validate.Error
	if errors.As(err, &v) {
		for _, f := range v.Fields {
			if errors.Is(f.Error, validate.ErrFieldRequired) {
				return true
			}
		}
	}
	return false
}

// Admin serves the operational endpoints on the internal port: liveness,
// readiness, metrics and the build's version, the development issuer's token endpoint when dev is
// not nil, and the operators' control plane under /control/ when control is
// not nil.
func Admin(ready func(context.Context) error, m *telemetry.Metrics, dev, control http.Handler) http.Handler {
	mux := http.NewServeMux()
	if dev != nil {
		mux.Handle("POST /dev/token", dev)
	}
	if control != nil {
		mux.Handle("/control/", control)
	}
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := ready(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("GET /metrics", m.Handler())
	mux.Handle("GET /version", buildinfo.Handler())
	return mux
}
