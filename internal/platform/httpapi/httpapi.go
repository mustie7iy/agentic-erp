// Package httpapi assembles the public API (ADR-0014): telemetry,
// authentication and the unit of work around the server generated from
// api/openapi.yaml, with authorization per operation and every error as an
// RFC 9457 problem. Contexts plug in by implementing their part of the
// generated Handler interface; operations not yet built answer 501.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/middleware"
	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/ogen-go/ogen/openapi"
	"github.com/ogen-go/ogen/validate"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// unimplemented sits one level deeper than the contexts' endpoints, so an
// implemented method always wins over the generated 501 stub.
type unimplemented struct{ gen.UnimplementedHandler }

// Each context's endpoint set is embedded under an alias, so the field
// names differ although every context calls its type Endpoints.
type (
	operationsAPI  = operations.Endpoints
	tenancyAPI     = tenancy.Endpoints
	identityAPI    = identity.Endpoints
	configAPI      = configuration.Endpoints
	partnerAPI     = partner.Endpoints
	ledgerAPI      = ledger.Endpoints
	workflowAPI    = approvals.Endpoints
	procurementAPI = procurement.Endpoints
	payablesAPI    = payables.Endpoints
	paymentsAPI    = payments.Endpoints
	auditAPI       = audit.Endpoints
	integrationAPI = integration.Endpoints
	agentsAPI      = agents.Endpoints
)

// handler is the generated Handler: one embedded endpoint set per context.
type handler struct {
	unimplemented
	*operationsAPI
	*tenancyAPI
	*identityAPI
	*configAPI
	*partnerAPI
	*ledgerAPI
	*workflowAPI
	*procurementAPI
	*payablesAPI
	*paymentsAPI
	*auditAPI
	*integrationAPI
	*agentsAPI
}

// Config is what the API needs.
type Config struct {
	Pool       *database.Pool
	AppRole    string
	Verifier   *authn.Verifier
	Pages      *pagination.Codec
	FieldKeys  *fieldcrypt.Keyring // seals bank accounts and tax identifiers with each tenant's key
	Extractor  capture.Extractor   // reads captured invoices; nil: capture answers 503
	Connector  payments.Connector  // transmits bank files; nil: transmission answers 503
	Workflows  workflow.Client     // the workflow engine behind operations (ADR-0012); nil: none
	ExtractDir string              // sap_file connectors read extracts below it; empty: they answer 503
	Objects    blobstore.Store     // documents, bank files and audit anchors; nil: documents and files are not kept
	UploadKey  []byte              // signs the API's own upload URLs (directory store); the same on every replica
	PublicURL  string              // the API's public base URL, making the API's own upload URLs absolute; empty: paths
	Secrets    secrets.Resolver    // resolves connectors' secret_ref; nil: env: and file: references on this replica
	// Limits enforces the request quotas of 04 §4.11; nil applies none.
	Limits *ratelimit.Limiter
	// RequestTimeout is the deadline of one request, so that a saturated
	// database sheds load as 503 instead of queueing; 0 sets none.
	RequestTimeout time.Duration
	Log            *slog.Logger
	Metrics        *telemetry.Metrics
	Tracer         trace.TracerProvider
	Meter          metric.MeterProvider // per-operation metrics of the generated server
	Now            func() time.Time
}

// New builds the public API handler.
func New(cfg Config) (http.Handler, error) {
	opener := approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}}
	uploads := &payables.Uploads{Objects: cfg.Objects, Secret: cfg.UploadKey, PublicURL: cfg.PublicURL, Pool: cfg.Pool, AppRole: cfg.AppRole, Now: cfg.Now}
	h := handler{
		operationsAPI: &operations.Endpoints{Pages: cfg.Pages, Workflows: cfg.Workflows, Log: cfg.Log},
		tenancyAPI:    &tenancy.Endpoints{Pages: cfg.Pages},
		identityAPI:   &identity.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}},
		configAPI:     &configuration.Endpoints{Pages: cfg.Pages, Dependencies: Configuration(cfg)},
		partnerAPI: &partner.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: cfg.FieldKeys,
			Approvals: opener},
		ledgerAPI: &ledger.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Partners: partner.Postings{},
			Numbers: configuration.Numbers{}},
		workflowAPI: &approvals.Endpoints{Pages: cfg.Pages, Decider: approvals.Decider{Roles: identity.Directory{}, Handlers: map[string]approvals.Handler{
			approvals.Requisition:      procurement.Approvals{},
			approvals.PartnerChange:    partner.Approvals{},
			approvals.ConfigChange:     configuration.Approvals{},
			approvals.InvoiceException: payables.Approvals{},
		}}},
		procurementAPI: &procurement.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{},
			Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{}, Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}},
			Settings: configuration.Settings{}, Approvals: opener, Invoices: payables.Invoices{}},
		payablesAPI: &payables.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{},
			Orders: procurement.Orders{}, Postings: ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{},
				Numbers: configuration.Numbers{}}, Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{},
			Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}}, Settings: configuration.Settings{}, Approvals: opener,
			Payments: payments.Reservations{}, OrderNumbers: procurement.Orders{}, Extractor: cfg.Extractor, Objects: cfg.Objects, Uploads: uploads},
		paymentsAPI:    Payments(cfg),
		auditAPI:       &audit.Endpoints{Pages: cfg.Pages, Anchors: cfg.Objects},
		integrationAPI: Integration(cfg),
		agentsAPI:      &agents.Endpoints{Pages: cfg.Pages},
	}
	srv, err := gen.NewServer(h, security{},
		gen.WithErrorHandler(errorHandler(cfg.Log)),
		gen.WithMiddleware(authorize(authz.New(api.MustOperations()))),
		gen.WithNotFound(func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, r, problem.NotFound("No such endpoint."))
		}),
		gen.WithMethodNotAllowed(func(w http.ResponseWriter, r *http.Request, allowed string) {
			w.Header().Set("Allow", allowed)
			problem.Write(w, r, problem.New(http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed",
				"Allowed: "+allowed+"."))
		}),
		gen.WithTracerProvider(cfg.Tracer),
		gen.WithMeterProvider(cfg.Meter),
	)
	if err != nil {
		return nil, err
	}
	unit := &uow.Middleware{Pool: cfg.Pool, Role: cfg.AppRole, Log: cfg.Log, Now: cfg.Now}
	chain := unit.Wrap(srv)
	if cfg.Limits != nil {
		chain = cfg.Limits.Middleware(chain)
	}
	chain = cfg.Verifier.Middleware(chain)
	if cfg.RequestTimeout > 0 {
		chain = deadline(cfg.RequestTimeout, chain)
	}
	// The API-hosted upload is the one route outside the generated server:
	// a signed URL instead of a bearer token, a body above the request
	// limit, and no unit of work (the upload itself is not idempotent work;
	// the capture that consumes it is).
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/uploads/{id}/content", uploads)
	mux.Handle("/", chain)
	return telemetry.Middleware(cfg.Log, cfg.Metrics, cfg.Tracer)(mux), nil
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

// Configuration is the configuration context's dependencies as the API
// wires them, for provisioning to bootstrap a tenant the same way.
func Configuration(cfg Config) configuration.Dependencies {
	return configuration.Dependencies{
		LegalEntities: tenancy.Directory{},
		Sections: []configuration.Section{ledger.Setup{LegalEntities: tenancy.Directory{}}, partner.PaymentTerms{},
			payments.HouseBanks{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: cfg.FieldKeys}},
		Approvals: approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}},
		Simulator: payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}},
		Regressor: payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}},
	}
}

// Payments is the payments context as the API wires it, for the proposal
// worker to build runs with the same ports.
func Payments(cfg Config) *payments.Endpoints {
	return &payments.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, OpenItems: ledger.OpenItems{},
		Postings: ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}},
		Payees:   partner.Payees{Keys: cfg.FieldKeys}, Numbers: configuration.Numbers{}, Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}},
		Settings: configuration.Settings{}, Keys: cfg.FieldKeys, Connector: cfg.Connector, Objects: cfg.Objects}
}

// Integration is the integration context as the API wires it: migration
// runs load through the same application services as the API.
func Integration(cfg Config) *integration.Endpoints {
	postings := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	partners := partner.Ports{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: cfg.FieldKeys}
	orders := &procurement.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{},
		Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{}, Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}},
		Settings: configuration.Settings{}, Invoices: payables.Invoices{}}
	invoices := &payables.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{},
		Orders: procurement.Orders{}, Postings: postings, Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{},
		Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}}, Settings: configuration.Settings{}, Payments: payments.Reservations{}}
	return &integration.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Settings: configuration.Settings{},
		Partners: partner.Migration{Ports: partners}, Suppliers: partner.Suppliers{}, Orders: orders, OrderValues: procurement.Orders{},
		Invoices: invoices, InvoiceCounts: payables.Invoices{}, Postings: postings, Reports: ledger.Reports{}, Secrets: resolver(cfg.Secrets),
		ExtractDir: cfg.ExtractDir, Workflows: cfg.Workflows != nil}
}

// resolver is the configured secret resolver, or the replica's own
// environment and files.
func resolver(r secrets.Resolver) secrets.Resolver {
	if r != nil {
		return r
	}
	local := secrets.Local{}
	return secrets.Multi{secrets.SchemeEnv: local, secrets.SchemeFile: local}
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
		if err := a.Authorize(req.Context, req.OperationID); err != nil {
			return middleware.Response{}, err
		}
		return next(req)
	}
}

// errorHandler writes every error from the generated server as a problem.
func errorHandler(log *slog.Logger) ogenerrors.ErrorHandler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
		p := toProblem(err)
		if p.Status >= 500 {
			log.ErrorContext(ctx, "request failed", "error", err, "trace_id", reqctx.TraceID(ctx))
		}
		problem.Write(w, r, p)
	}
}

func toProblem(err error) *problem.Problem {
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
	return problem.From(err)
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
// readiness and metrics, the development issuer's token endpoint when dev is
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
	return mux
}
