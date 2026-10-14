// Package app is the composition root (06-engineering-process §6.7): it
// builds the public API (ADR-0014) from every context's handlers and wires
// the contexts' ports together, so that the workers in cmd/erp-server and
// provisioning use the same wiring as the API. It is the one package under
// internal/ that may import every context; the request pipeline it hands
// the result to is internal/platform/httpapi, which names none.
package app

import (
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

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
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
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
	return httpapi.New(h, httpapi.Pipeline{
		Pool: cfg.Pool, AppRole: cfg.AppRole, Verifier: cfg.Verifier, Limits: cfg.Limits, RequestTimeout: cfg.RequestTimeout,
		// The API-hosted upload is the one route outside the generated
		// server: a signed URL instead of a bearer token, a body above the
		// request limit, and no unit of work (the upload itself is not
		// idempotent work; the capture that consumes it is).
		Routes: map[string]http.Handler{"PUT /v1/uploads/{id}/content": uploads},
		Log:    cfg.Log, Metrics: cfg.Metrics, Tracer: cfg.Tracer, Meter: cfg.Meter, Now: cfg.Now,
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
