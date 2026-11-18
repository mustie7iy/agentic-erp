// Package app is the composition root (06-engineering-process §6.7): it
// builds the public API (ADR-0014) from every context's handlers and wires
// the contexts' ports together, so that the workers in cmd/erp-server and
// provisioning use the same wiring as the API. It is the one package under
// internal/ that may import every context; the request pipeline it hands
// the result to is internal/platform/httpapi, which names none.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/analytics/watchers"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/credit"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/leases"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/processmining"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/records"
	"github.com/mustie7iy/agentic-erp/internal/revenue"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
	"github.com/mustie7iy/agentic-erp/packs"
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
	receivablesAPI = receivables.Endpoints
	salesAPI       = sales.Endpoints
	revenueAPI     = revenue.Endpoints
	collectionsAPI = collections.Endpoints
	creditAPI      = credit.Endpoints
	grcAPI         = grc.Endpoints
	recordsAPI     = records.Endpoints
	miningAPI      = processmining.Endpoints
	leasesAPI      = leases.Endpoints
	materialsAPI   = materials.Endpoints
	paymentsAPI    = payments.Endpoints
	auditAPI       = audit.Endpoints
	integrationAPI = integration.Endpoints
	agentsAPI      = agents.Endpoints
	analyticsAPI   = endpoint.Endpoints
	reportsAPI     = reports.Endpoints
	watchersAPI    = watchers.Endpoints
	cashAPI        = cash.Endpoints
	assetsAPI      = assets.Endpoints
	closingAPI     = closing.Endpoints
	consolAPI      = consolidation.Endpoints
	einvoicingAPI  = einvoicing.Endpoints
	controllingAPI = controlling.Endpoints
	taxfilingAPI   = taxfiling.Endpoints
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
	*receivablesAPI
	*salesAPI
	*revenueAPI
	*collectionsAPI
	*creditAPI
	*grcAPI
	*recordsAPI
	*miningAPI
	*leasesAPI
	*materialsAPI
	*paymentsAPI
	*auditAPI
	*integrationAPI
	*agentsAPI
	*analyticsAPI
	*reportsAPI
	*watchersAPI
	*cashAPI
	*assetsAPI
	*closingAPI
	*consolAPI
	*einvoicingAPI
	*controllingAPI
	*taxfilingAPI
}

// Config is what the API needs.
type Config struct {
	Pool *database.Pool
	// Consistent is the pool of the reads a write records as of one
	// database state (uow.Consistent: a cash snapshot taken on demand,
	// ADR-0051 §6), apart from Pool, so a request holding one of Pool's
	// connections never waits for another of Pool's; nil takes no snapshot
	// on demand.
	Consistent *database.Pool
	AppRole    string
	// Begin starts the API's request transactions; nil: Pool.Begin. Tests
	// set it to run a concurrent change between a request's statements.
	Begin     database.BeginFunc
	Verifier  *authn.Verifier
	Pages     *pagination.Codec
	FieldKeys *fieldcrypt.Keyring // seals bank accounts and tax identifiers with each tenant's key
	Extractor capture.Extractor   // reads captured invoices; nil: capture answers 503
	// OrderExtractor reads captured customers' purchase orders (T90a);
	// nil: order capture answers 503.
	OrderExtractor capture.OrderExtractor
	Connector      payments.Connector // transmits bank files; nil: transmission answers 503
	Workflows      workflow.Client    // the workflow engine behind operations (ADR-0012); nil: none
	ExtractDir     string             // sap_file connectors read extracts below it; empty: they answer 503
	Objects        blobstore.Store    // documents, bank files, statement files and audit anchors; nil: documents and files are not kept, statements not imported
	// Loopback serves the API's reads in-process (httpapi.Loopback): New
	// gives it its handler; a worker reading as a principal (a report's
	// scheduled run, ADR-0076 §5) shares it. Nil: New makes its own.
	Loopback  *httpapi.Loopback
	Retention time.Duration    // how long statement files are locked (ERP_OBJECT_RETENTION); 0: not locked
	UploadKey []byte           // signs the API's own upload URLs (directory store); the same on every replica
	PublicURL string           // the API's public base URL, making the API's own upload URLs absolute; empty: paths
	Secrets   secrets.Resolver // resolves connectors' secret_ref; nil: env: and file: references on this replica
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
	// RequireBoundAgents accepts agents' tokens only bound to their client
	// certificates (production, ADR-0033).
	RequireBoundAgents bool
	// RefuseEBICS refuses bank_ebics connectors until EBICS is certified
	// with a bank (production, ADR-0038).
	RefuseEBICS bool
	// Model is the model gateway (agents.Gateway, ADR-0036); nil answers 503.
	Model http.Handler
	// Analytics is erp-analytics, which answers semantic queries from the
	// analytical store (store.Client, ADR-0044); nil answers them from the
	// database.
	Analytics store.Querier
	// EvalCell lifts the bound of agents' autonomy by their type's
	// certification, on a cell that runs live evaluations (ERP_EVAL_CELL,
	// ADR-0039): a type's first run needs agents at its suite's levels.
	EvalCell bool
	// RefuseUnsignedPacks refuses applying a country pack no tax specialist
	// has signed off (production, ADR-0048); elsewhere packs can be tried.
	RefuseUnsignedPacks bool
	// Packs are the country packs the API lists and applies (ADR-0048);
	// nil: those embedded in the image (packs.FS). Tests release their own.
	Packs fs.FS
	// UnreleasedPacks is a directory of further packs taken as they are
	// (ERP_PACKS_DIR, refused in production): a demonstration's or a
	// test's pack not yet released (ADR-0048). Empty: none.
	UnreleasedPacks string
	// Providers is the client e-invoicing providers are called through
	// (einvoicing.NewClient, ADR-0061 §3); nil: a fetch is refused, 503.
	Providers *egress.Client
	// SAP is the client sap_odata connectors' systems are read through
	// (sap.NewClient, ADR-0041 As built, product owner 2026-11-16); nil:
	// a run from one is refused, 503.
	SAP *egress.Client
	// WebhookReceiver is the walkthrough's webhook receiver, the one
	// loopback address a subscription may name (ERP_WALKTHROUGH_RECEIVER,
	// refused outside development, ADR-0041 As built); zero: none.
	WebhookReceiver netip.AddrPort
	// SFTPControl judges every address bank and treasury channels dial
	// (egress.DialControl: public addresses, and the walkthrough's SFTP
	// stand-in, ERP_WALKTHROUGH_SFTP, refused outside development); nil:
	// public addresses only.
	SFTPControl func(network, address string, c syscall.RawConn) error
}

// certifications bounds agents' autonomy by their type's certification
// (ADR-0039), unless the cell runs live evaluations.
func certifications(cfg Config) agents.Certifications {
	return agents.Certifications{Agents: identity.AutonomousAgents, Waived: cfg.EvalCell}
}

// New builds the public API handler.
func New(cfg Config) (http.Handler, error) {
	if cfg.Log == nil {
		// Required, not defaulted: what the start finds (a country pack
		// without its sign-off) must reach the server's log.
		return nil, errors.New("app: a logger is required (Config.Log)")
	}
	library, err := countryPacks(cfg)
	if err != nil {
		return nil, err
	}
	upload, loopback := uploads(cfg), loopbackOf(cfg)
	// The seed of every tenant's semantic model until a change request
	// gives it its own: unreadable, the API does not start.
	if _, err := governedModel(); err != nil {
		return nil, err
	}
	// People's sessions, recorded per replica (ADR-0031), and ended by
	// endCurrentSession through the same instance, which forgets them.
	sessions := &identity.Sessions{Pool: cfg.Pool, AppRole: cfg.AppRole, Now: cfg.Now}
	selling, crediting := salesAndCredit(cfg)
	queries := analyticsEndpoints(cfg, loopback).Init()
	h := handler{
		operationsAPI: &operations.Endpoints{Pages: cfg.Pages, Workflows: cfg.Workflows, Log: cfg.Log},
		tenancyAPI:    &tenancy.Endpoints{Pages: cfg.Pages, Days: configuration.Settings{}, Now: cfg.Now},
		identityAPI:   &identity.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Certifications: certifications(cfg), Sessions: sessions},
		configAPI: &configuration.Endpoints{Pages: cfg.Pages, Packs: library, RefuseUnsignedPacks: cfg.RefuseUnsignedPacks,
			Dependencies: Configuration(cfg)},
		partnerAPI: &partner.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, OpenItems: ledger.OpenItems{}, Keys: cfg.FieldKeys, Schemas: configuration.Settings{}, Rules: configuration.Settings{},
			PaymentMethods: payments.PaymentMethods{LegalEntities: tenancy.Directory{}}, PlanningGroups: cash.PlanningGroups{}, Approvals: opener()},
		ledgerAPI:      Ledger(cfg),
		workflowAPI:    &approvals.Endpoints{Pages: cfg.Pages, Decider: approvals.Decider{Roles: identity.Directory{}, AgentKind: identity.AgentKind, Metrics: cfg.Metrics, Handlers: approvalHandlers(cfg)}},
		procurementAPI: procurementEndpoints(cfg),
		payablesAPI:    payablesEndpoints(cfg, upload),
		salesAPI:       selling,
		creditAPI:      crediting,
		paymentsAPI:    Payments(cfg),
		auditAPI:       Audit(cfg),
		integrationAPI: Integration(cfg),
		agentsAPI:      &agents.Endpoints{Pages: cfg.Pages, Countries: tenancy.Countries, Certifications: certifications(cfg)},
		analyticsAPI:   queries,
		reportsAPI:     Reports(cfg, queries),
		watchersAPI:    Watchers(cfg, queries),
		cashAPI:        Cash(cfg),
		assetsAPI:      Assets(cfg),
		closingAPI:     Closing(cfg),
		consolAPI:      Consolidation(cfg),
		einvoicingAPI:  EInvoicing(cfg),
		controllingAPI: controllingEndpoints(cfg),
		taxfilingAPI:   taxFiling(cfg, library),
	}
	h.laterContexts(cfg)
	return httpapi.New(h, httpapi.Pipeline{
		Pool: cfg.Pool, Begin: cfg.Begin, AppRole: cfg.AppRole, Verifier: cfg.Verifier, Limits: cfg.Limits, RequestTimeout: cfg.RequestTimeout,
		// The API-hosted upload is the one route outside the generated
		// server: a signed URL instead of a bearer token, a body above the
		// request limit, and no unit of work (the upload itself is not
		// idempotent work; the capture that consumes it is).
		Routes: map[string]http.Handler{"PUT /v1/uploads/{id}/content": upload},
		Log:    cfg.Log, Metrics: cfg.Metrics, Tracer: cfg.Tracer, Meter: cfg.Meter, Now: cfg.Now,
		Sessions:           sessions,
		RequireBoundAgents: cfg.RequireBoundAgents,
		Agents:             agents.Admit,
		Model:              cfg.Model,
		Loopback:           loopback,
	})
}

// controllingEndpoints is the controlling context as the API wires it.
func controllingEndpoints(cfg Config) *controlling.Endpoints {
	return &controlling.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Ledger: ledger.AssignmentBalances{},
		Settings: configuration.Settings{}, Now: cfg.Now, Postings: controllingPostings(), Calendar: calendar(), Sales: sales.Orders{},
		Partners: partner.Payees{}}
}

// taxFiling is the tax filing context as the API wires it.
func taxFiling(cfg Config, library *countrypack.Library) *taxfiling.Endpoints {
	return &taxfiling.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Settings: configuration.Settings{}, Now: cfg.Now,
		Items: ledger.TaxItems{}, Packs: library, Approvals: opener(), Periods: ledger.ReturnPeriods{Releases: payments.PeriodReleases{}}}
}

// loopbackOf is cfg's loopback, or a new one.
func loopbackOf(cfg Config) *httpapi.Loopback {
	if cfg.Loopback != nil {
		return cfg.Loopback
	}
	return &httpapi.Loopback{}
}

// analyticsEndpoints is the analytics context as the API and the
// simulation worker wire it: the semantic model read from the tenant's
// configuration (ADR-0044, T55g).
func analyticsEndpoints(cfg Config, loopback http.RoundTripper) *endpoint.Endpoints {
	return &endpoint.Endpoints{Configs: configuration.Settings{}, Companies: companyCodes{}, Reader: cfg.Analytics, Loopback: loopback, Pages: cfg.Pages,
		Cash: NewAnalyticsCash(cfg), Log: cfg.Log, Now: cfg.Now}
}

// governedModel is the semantic layer's seed model, built in; tests
// replace it.
var governedModel = analytics.DefaultModel

// companyCodes names legal entities by their company codes for analytics.
type companyCodes struct{}

func (companyCodes) CompanyCode(ctx context.Context, id uuid.UUID) (string, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return le.Code, ok, err
}

// countryPacks loads the country packs: a pack that is invalid, or not as
// released, stops the server from starting; a pack without its tax
// specialist's sign-off is logged, since the pilot needs it.
func countryPacks(cfg Config) (*countrypack.Library, error) {
	fsys := cfg.Packs
	if fsys == nil {
		fsys = packs.FS
	}
	lib, err := countrypack.LoadAll(fsys)
	if err == nil && cfg.UnreleasedPacks != "" {
		err = lib.AddUnreleased(os.DirFS(cfg.UnreleasedPacks))
	}
	if err != nil {
		return nil, fmt.Errorf("app: country packs: %w", err)
	}
	for _, w := range lib.Warnings {
		cfg.Log.Warn("country pack", "path", w.Path, "finding", w.Message)
	}
	return lib, nil
}

// Configuration is the configuration context's dependencies as the API
// wires them, for provisioning to bootstrap a tenant the same way.
func Configuration(cfg Config) configuration.Dependencies {
	return configuration.Dependencies{
		LegalEntities: tenancy.Directory{},
		Sections: []configuration.Section{ledger.Setup{LegalEntities: tenancy.Directory{}, Now: cfg.Now}, partner.PaymentTerms{},
			partner.WithholdingCodes{LegalEntities: tenancy.Directory{}},
			partner.PaymentMethodCodes{LegalEntities: tenancy.Directory{}},
			payments.HouseBanks{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Channels: integration.BankChannels{}, Keys: cfg.FieldKeys},
			payments.PaymentMethods{LegalEntities: tenancy.Directory{}}, endpoint.Section{},
			// After the other sections that lock supplier codes: a partner
			// change takes the planning group lock last too.
			cash.Setup{}, partner.PlanningGroupCodes{LegalEntities: tenancy.Directory{}},
			taxfiling.Setup{LegalEntities: tenancy.Directory{}, Keys: cfg.FieldKeys}, closing.Setup{Plans: closingPlans{}},
			consolidation.Setup{Companies: consolidationCompanies{}}},
		Approvals: opener(),
		Simulator: configuration.Simulators{payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates()},
			endpoint.Simulation{Endpoints: analyticsEndpoints(cfg, nil), Now: cfg.Now},
			ledger.AssignmentSimulation{LegalEntities: tenancy.Directory{}, Now: cfg.Now}, cashSimulation{cash.Simulation{Endpoints: Cash(cfg)}}},
		Regressor: payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates()},
		Actors:    identity.Actors{},
		RuleSamplers: map[string]rules.Sampler{"supplier_invoice": payables.RuleSamples{}, "purchase_order": procurement.RuleSamples{},
			"business_partner": partner.RuleSamples{}},
		Disclosures: []configuration.Disclosure{payables.Disclosure{}, endpoint.Disclosure{Endpoints: analyticsEndpoints(cfg, nil)},
			ledger.AssignmentDisclosure{}, cashDisclosure{}},
	}
}

// Ledger is the ledger context as the API wires it, for the recurring
// entries' activity to park with the same ports (ADR-0056).
func Ledger(cfg Config) *ledger.Endpoints {
	return &ledger.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, CostObjects: controlling.CostObjects{},
		Numbers: configuration.Numbers{}, Orders: procurement.Orders{}, Approvals: opener(), Settings: configuration.Settings{}, TaxReturns: taxfiling.Holds{},
		PartnerNames: LedgerPartners{}, Now: cfg.Now, People: identity.Actors{}, PersonNames: LedgerPeople{}, Releases: payments.PeriodReleases{}}
}

// Payments is the payments context as the API wires it, for the proposal
// worker to build runs with the same ports.
func Payments(cfg Config) *payments.Endpoints {
	return &payments.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, OpenItems: ledger.OpenItems{},
		Postings: postings(),
		Payees:   partner.Payees{Keys: cfg.FieldKeys}, Numbers: configuration.Numbers{}, Calendar: calendar(),
		Settings: configuration.Settings{}, Actors: identity.Actors{}, Roles: identity.Directory{}, Keys: cfg.FieldKeys, Connector: cfg.Connector, Objects: cfg.Objects,
		Retention: cfg.Retention, Channels: integration.BankChannels{Secrets: resolver(cfg.Secrets), Control: cfg.SFTPControl},
		Accounts: ledger.Accounts{}, DownPayments: downPayments{}, Approvals: opener(), Sessions: agents.Sessions{}, Now: cfg.Now}
}

// laterContexts sets the handler's contexts of the later phases, each wired
// from the configuration alone (receivables, revenue, collections, GRC,
// records, process mining, leases, materials), so New lists only those it
// builds from what it has set up.
func (h *handler) laterContexts(cfg Config) {
	h.receivablesAPI = Receivables(cfg)
	h.revenueAPI = Revenue(cfg)
	h.collectionsAPI = Collections(cfg)
	h.grcAPI = GRC(cfg)
	h.recordsAPI = Records(cfg)
	h.miningAPI = ProcessMining(cfg)
	h.leasesAPI = Leases(cfg)
	h.materialsAPI = Materials(cfg)
}

// approvalHandlers are the contexts that own each approval object type:
// each moves its objects on when their requests complete and says where the
// API reads them (approvals.Locator).
func approvalHandlers(cfg Config) map[string]approvals.Handler {
	return map[string]approvals.Handler{
		approvals.Requisition:       procurement.Approvals{Endpoints: procurementEndpoints(cfg)},
		approvals.PartnerChange:     partner.Approvals{Schemas: configuration.Settings{}},
		approvals.ConfigChange:      configuration.Approvals{},
		approvals.InvoiceException:  payables.Approvals{},
		approvals.PeriodUnlock:      ledger.PeriodUnlocks{},
		approvals.ManualEntry:       ledger.ManualEntries{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, TaxReturns: taxfiling.Holds{}},
		approvals.PurchaseOrder:     procurement.OrderReleases{Endpoints: procurementEndpoints(cfg)},
		approvals.OrderChange:       procurement.OrderChanges{Endpoints: procurementEndpoints(cfg)},
		approvals.BankClearing:      payments.BankClearings{Endpoints: Payments(cfg)},
		approvals.CenterChange:      ledger.CenterChanges{LegalEntities: tenancy.Directory{}, Settings: configuration.Settings{}, Now: cfg.Now},
		approvals.PaymentRunRelease: payments.ReleaseApprovals{Endpoints: Payments(cfg)},
		approvals.GRIRClearing:      procurement.GRIRClearings{Endpoints: procurementEndpoints(cfg)},
		approvals.TaxReturn:         taxfiling.TaxReturns{Endpoints: &taxfiling.Endpoints{Items: ledger.TaxItems{}}},
		approvals.MaterialChange:    materials.Approvals{Endpoints: Materials(cfg)},
	}
}

// Integration is the integration context as the API wires it: migration
// runs load through the same endpoints the API serves, built the same way.
func Integration(cfg Config) *integration.Endpoints {
	partners := partner.Ports{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: cfg.FieldKeys, Schemas: configuration.Settings{}, Rules: configuration.Settings{},
		PaymentMethods: payments.PaymentMethods{LegalEntities: tenancy.Directory{}}, PlanningGroups: cash.PlanningGroups{}}
	return &integration.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Settings: configuration.Settings{},
		Partners: partner.Migration{Ports: partners}, Suppliers: partner.Suppliers{Settings: configuration.Settings{}}, Orders: procurementEndpoints(cfg), OrderLocks: procurement.Orders{}, OrderValues: procurement.Orders{},
		Invoices: payablesEndpoints(cfg, uploads(cfg)), InvoiceCounts: payables.Invoices{}, Invoiced: payables.Invoices{}, Postings: postings(), Reports: ledger.Reports{}, Centers: ledger.CenterMigration{}, Actuals: ledger.AssignmentBalances{},
		Secrets: resolver(cfg.Secrets), Requesters: identity.Actors{}, Keys: cfg.FieldKeys, Resolver: net.DefaultResolver, Receiver: cfg.WebhookReceiver, ExtractDir: cfg.ExtractDir, SAP: sapClient(cfg.SAP), Workflows: cfg.Workflows != nil, RefuseEBICS: cfg.RefuseEBICS,
		Now: cfg.Now} // the clock a migration run's day is read from (the company's date, review:batch63)
}

// procurementEndpoints is the procurement context as the API wires it.
func procurementEndpoints(cfg Config) *procurement.Endpoints {
	return &procurement.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{Settings: configuration.Settings{}}, Partners: partner.Payees{},
		AccountNames: ledger.AssetReads{}, Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{}, Calendar: calendar(),
		Settings: configuration.Settings{}, Schemas: configuration.Settings{}, Rules: configuration.Settings{}, References: partner.References{}, Approvals: opener(),
		Invoices: payables.Invoices{}, Postings: postings(), SupplierNames: partner.Payees{}, Now: cfg.Now}
}

// payablesEndpoints is the payables context as the API wires it, capturing
// invoices through uploads.
func payablesEndpoints(cfg Config, uploads *payables.Uploads) *payables.Endpoints {
	return &payables.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{Settings: configuration.Settings{}},
		Orders: procurement.Orders{}, Postings: postings(), Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{},
		Calendar: calendar(), Settings: configuration.Settings{}, Schemas: configuration.Settings{}, Rules: configuration.Settings{}, References: partner.References{}, Rates: rates(), Approvals: opener(),
		Payments: payments.Reservations{}, Methods: payments.PaymentMethods{LegalEntities: tenancy.Directory{}}, OrderNumbers: procurement.Orders{},
		Extractor: cfg.Extractor, Objects: cfg.Objects, Uploads: uploads, ExpenseLedger: ledger.AssetReads{}, Partners: partner.Payees{}, Now: cfg.Now}
}

// Receivables is the receivables context as the API wires it (ADR-0063
// §1): the ledger, the partner context, tenancy and the configuration
// through its ports, as payables reads them. The payment terms are the
// tenant's, for customers as for suppliers: partner.Suppliers is their one
// reader.
func Receivables(cfg Config) *receivables.Endpoints {
	return &receivables.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Customers: partner.Postings{}, CustomerData: partner.Customers{},
		Terms: partner.Suppliers{Settings: configuration.Settings{}}, Partners: partner.Payees{}, Accounts: ledger.Accounts{}, References: ledger.AssetReads{},
		Postings: postings(), Entries: ledger.OpenItems{}, Receipts: ledger.OpenItems{}, OpenItems: ledger.OpenItems{}, Statements: receivablesStatements{},
		Numbers: configuration.Numbers{}, Calendar: calendar(), Settings: configuration.Settings{}, Now: cfg.Now}
}

// Sales is the sales context as the API wires it (ADR-0067 §1): the
// ledger, the partner context, tenancy and the configuration through its
// ports, as receivables reads them. The payment terms are the tenant's,
// for customers as for suppliers: partner.Suppliers is their one reader.
// A billing document posts through receivables' published API, in
// process and in the billing's transaction (ADR-0067 §9).
func Sales(cfg Config) *sales.Endpoints {
	s, _ := salesAndCredit(cfg)
	return s
}

// salesAndCredit wires the sales context and the credit context, which
// ask each other one question each (ADR-0066, As built T77b): credit
// reads the customer's confirmed orders not yet billed from sales, which
// owns them (credit.OpenOrders, answered by sales.Endpoints.OpenOrders),
// and sales asks credit, when it confirms an order, whether the order's
// value is within the customer's limit (sales.Credit, answered by
// salesCredit). Neither imports the other: credit stays a leaf.
func salesAndCredit(cfg Config) (*sales.Endpoints, *credit.Endpoints) {
	s := &sales.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Customers: partner.Postings{}, CustomerData: partner.Customers{},
		Terms: partner.Suppliers{Settings: configuration.Settings{}}, Partners: partner.Payees{},
		Accounts: ledger.Accounts{}, References: ledger.AssetReads{}, Invoices: Receivables(cfg), Entries: ledger.OpenItems{},
		Numbers: configuration.Numbers{}, Calendar: calendar(),
		Settings: configuration.Settings{}, Rates: ledger.Rates{LegalEntities: tenancy.Directory{}}, People: LedgerPeople{},
		Uploads: uploads(cfg), OrderExtractor: cfg.OrderExtractor, Finder: partner.Payees{Keys: cfg.FieldKeys}, Now: cfg.Now}
	c := &credit.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, OpenItems: ledger.OpenItems{}, Customers: partner.Customers{},
		Partners: partner.Payees{}, Orders: s}
	s.Credit = salesCredit{credit: c}
	return s, c
}

// salesCredit answers the sales context's question to the credit
// context, which sales does not import (ADR-0066 §1, As built T77b): the
// check of an order's value against its customer's limit, in sales' own
// type.
type salesCredit struct{ credit *credit.Endpoints }

func (c salesCredit) Check(ctx context.Context, legalEntity, customer uuid.UUID, amount money.Minor) (sales.CreditCheck, error) {
	x, err := c.credit.Check(ctx, legalEntity, customer, amount)
	if err != nil || x.Limit == nil {
		return sales.CreditCheck{Within: x.Within}, err
	}
	return sales.CreditCheck{HasLimit: true, Limit: x.Limit.Amount, Exposure: x.Total, Within: x.Within}, nil
}

// Revenue is the revenue recognition context as the API and the event
// consumer wire it (ADR-0068 §1): a leaf reading the sales context's
// orders, partners' names, tenancy, the ledger's calendar and accounts
// and the configuration through its ports, and posting through the
// ledger's.
func Revenue(cfg Config) *revenue.Endpoints {
	return &revenue.Endpoints{Pages: cfg.Pages, SalesOrders: sales.Orders{}, LegalEntities: tenancy.Directory{}, Partners: partner.Payees{},
		Calendar: calendar(), Settings: configuration.Settings{}, Postings: postings(), Accounts: ledger.Accounts{}, Now: cfg.Now}
}

// receivablesStatements gives receivables the statement lines behind its
// receipts' entries from the payments context, which it does not import
// (ADR-0063 §1, As built T75c).
type receivablesStatements struct{}

func (receivablesStatements) Lines(ctx context.Context, entries []ledger.ItemRef) (map[uuid.UUID]receivables.StatementLine, error) {
	lines, err := payments.StatementPostings{}.Lines(ctx, entries)
	out := make(map[uuid.UUID]receivables.StatementLine, len(lines))
	for entry, l := range lines {
		out[entry] = receivables.StatementLine{StatementID: l.StatementID, StatementRef: l.StatementRef, LineNo: l.LineNo}
	}
	return out, err
}

// Collections is the collections context as the API wires it (ADR-0065
// §1): a leaf that reads the ledger's customer open items, the partner
// context's names, people's names, tenancy and the configuration through
// its ports.
func Collections(cfg Config) *collections.Endpoints {
	return &collections.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, OpenItems: ledger.OpenItems{}, Partners: partner.Payees{},
		Settings: configuration.Settings{}, People: LedgerPeople{}, Now: cfg.Now}
}

// Audit is the audit context as the API wires it (ADR-0011): its
// evidence exports (T101a) read tenancy's legal entities, the ledger's
// journal, the approval requests on the entries' source documents and
// people's names through its ports, and write to the object store.
func Audit(cfg Config) *audit.Endpoints {
	return &audit.Endpoints{Pages: cfg.Pages, Objects: cfg.Objects, LegalEntities: tenancy.Directory{}, Journal: ledger.Archive{},
		Approvals: approvals.Evidence{}, People: LedgerPeople{}, Now: cfg.Now}
}

// ProcessMining is the process mining context as the API wires it
// (ADR-0080 §1): a leaf that reads tenancy's legal entities, procurement's
// orders, payables' documents, payments' runs and the audit trail through
// its ports.
func ProcessMining(cfg Config) *processmining.Endpoints {
	return &processmining.Endpoints{LegalEntities: tenancy.Directory{}, Orders: procurement.ProcessLog{}, Invoices: payables.ProcessLog{},
		Payments: payments.ProcessLog{}, Trail: audit.Trail{}, Pages: cfg.Pages}
}

// Records is the records management context as the API wires it
// (ADR-0079 §1): a leaf that reads tenancy's legal entities, the active
// configuration and the ledger's fiscal calendar and journal through its
// ports, writes archive files to the object store (T99b), and blanks
// blocked partners' personal data through the partner context's (T100c).
func Records(cfg Config) *records.Endpoints {
	return &records.Endpoints{LegalEntities: tenancy.Directory{}, Settings: configuration.Settings{}, Calendar: calendar(), Journal: ledger.Archive{},
		People: LedgerPeople{}, Privacy: partner.Privacy{Keys: cfg.FieldKeys, OpenItems: ledger.OpenItems{}, Settings: configuration.Settings{}}, Pages: cfg.Pages, Objects: cfg.Objects, Now: cfg.Now}
}

// Credit is the credit management context as the API wires it (ADR-0066
// §1): a leaf that reads the ledger's customer open items, the partner
// context's customers and names, the sales context's open orders and
// tenancy through its ports (salesAndCredit).
func Credit(cfg Config) *credit.Endpoints {
	_, c := salesAndCredit(cfg)
	return c
}

// uploads is the API's direct-upload service (ADR-0020).
func uploads(cfg Config) *payables.Uploads {
	return &payables.Uploads{Objects: cfg.Objects, Secret: cfg.UploadKey, PublicURL: cfg.PublicURL, Pool: cfg.Pool, AppRole: cfg.AppRole, Now: cfg.Now,
		Limits: cfg.Limits}
}

// opener opens approval requests with the tenant's policies and roles.
func opener() approvals.Approvals {
	return approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}, Rates: rates()}
}

// LedgerPartners names a revaluation's partners for the ledger from the
// partner context's published names (T73i2): the ledger does not import
// the partner context. Exported for the ledger's tests, which wire their
// endpoints as the API does.
type LedgerPartners struct{}

// PartnerNames are the partners' numbers and names by id, as the partner
// context publishes them; an id that names no partner is left out.
func (LedgerPartners) PartnerNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.PartnerName, error) {
	named, err := partner.Payees{}.Names(ctx, ids)
	out := make(map[uuid.UUID]ledger.PartnerName, len(named))
	for id, n := range named {
		out[id] = ledger.PartnerName{Number: n.Number, Name: n.Name}
	}
	return out, err
}

// LedgerPeople names the people who ran the ledger's carry-forwards, and
// a dispute case's assignee and closer, from the identity context's
// directory: neither the ledger nor collections imports it. Exported for
// the ledger's tests, which wire their endpoints as the API does.
type LedgerPeople struct{}

// PersonNames are the principals' names by id; an id of none is left out.
func (LedgerPeople) PersonNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	named, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]string, len(named))
	for id, n := range named {
		out[id] = n.Name
	}
	return out, err
}

// rates converts document amounts into their company's currency for the
// contexts that compare them with configured amounts.
func rates() ledger.Rates { return ledger.Rates{LegalEntities: tenancy.Directory{}} }

// postings posts journal entries for the contexts that post.
func postings() ledger.Postings {
	return ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, TaxReturns: taxfiling.Holds{},
		Settings: configuration.Settings{}}
}

// calendar resolves fiscal periods.
func calendar() ledger.Calendar { return ledger.Calendar{LegalEntities: tenancy.Directory{}} }

// controllingPostings is the ledger's posting port as the controlling
// context posts through it (ADR-0064 §3): the one context whose lines
// name internal orders, so it is wired with the cost objects port.
func controllingPostings() ledger.Postings {
	return ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{},
		CostObjects: controlling.CostObjects{}}
}

// sapClient is the configured OData client as integration takes it: a
// nil client stays a nil interface, which integration refuses.
func sapClient(c *egress.Client) sap.Getter {
	if c == nil {
		return nil
	}
	return c
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
