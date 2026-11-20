// Package einvoicing is the e-invoicing context (ADR-0061; SAP's Document
// and Reporting Compliance): the documents the tenant's accredited
// e-invoicing provider delivers for its companies, their statuses, and the
// provider as a port. It is a leaf (ADR-0061 §2): it reads the other
// contexts only through ports it declares, which the composition root
// (internal/app) adapts and which run in the request's transaction under
// row-level security (ADR-0003), and no context imports it
// (internal/layout).
//
// T65a builds fetching on demand (ADR-0061 §3, §4, §11, §12, §15):
// FetchEInvoices queues a fetch from an einvoice_provider connector as an
// Operation, which the workflow FetchEInvoices (internal/workflows)
// carries out through the workflows.EInvoices port that Endpoints
// implements: PendingEInvoices lists what the provider holds,
// RecordEInvoice records one document (its bytes write-once in object
// storage, the row, its audit record and EInvoiceChanged in one
// transaction) and acknowledges it to the provider after that commit, and
// FinishEInvoiceFetch ends the Operation with its counts; each is
// idempotent. ListEInvoices and GetEInvoice read the documents of the
// companies the caller may act in.
//
// T65b adds the scheduled fetch, the original and its changes (ADR-0061
// §4, §8): QueueEInvoiceFetch records the Operation of a fetch the workflow
// dispatcher starts once per period for each active connector, which then
// runs the same steps; a message recorded before whose bytes differ is
// recorded as changed (changed_hash, once per different bytes), the first
// bytes kept as the legal original, which GetEInvoiceContent serves,
// compared with their hash.
//
// E1 (po:2026-11-19) adds Evatra, the first accredited provider, as an
// adapter (Evatra: its login and token, its received documents' listing
// and content; acknowledgement local only); the stand-in (StandIn) stays
// for tests and the walkthrough, and production refuses it. The ERP polls
// and opens no endpoint for a provider (ADR-0061 Q3).
//
// T65c validates (ADR-0061 §4, §5): after recording, a fetch processes
// its connector's documents not processed yet (ProcessEInvoices), reading
// each as PINT AE (Received: no DTD, no entity) and refusing it with its
// problem, or leaving it received with what it read for entering.
// Entering, credit notes and people's resolution of held documents follow
// in T65d-T65f, sending in E3-E4.
package einvoicing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// KindFetch is the kind of a fetch's Operation (ADR-0061 §4).
const KindFetch = "einvoicing.fetch"

// Statuses of an e-invoice (ADR-0061 §7): received, and refused
// (StatusRefused, T65c).
const (
	StatusReceived = "received"
)

// Outcomes of recording one message (RecordEInvoice), the counts of a
// fetch's Operation.
const (
	Recorded        = "recorded"         // recorded by this fetch
	AlreadyRecorded = "already_recorded" // recorded by an earlier fetch; acknowledged again
	Changed         = "changed"          // recorded before, found by this fetch with other bytes
)

// FetcherActor is the principal fetches act as, in every tenant: a system
// principal acting for the connector's companies (ADR-0061 §4).
var FetcherActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:einvoice-fetcher"))

// Fetcher is the principal a fetch acts as in the tenant.
func Fetcher(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: FetcherActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// Connector is an einvoice_provider connector read for use (ADR-0061 §3):
// its tenant, the adapter, the stand-in's base URL or Evatra's environment
// and company id, each company's participant identifier by company code,
// the format, the secret_ref and, once resolved (Connectors.Credentials),
// the credentials.
type Connector struct {
	ID           uuid.UUID
	Tenant       uuid.UUID
	Name         string
	Adapter      string
	BaseURL      string            // the stand-in's
	Environment  string            // Evatra's: stage or prod
	CompanyID    string            // the company's id at Evatra
	Participants map[string]string // company code -> participant identifier
	Format       string
	SecretRef    string
	Secret       string
}

// Connectors reads the einvoice_provider connectors (integration context,
// ADR-0061 §2): Provider reads one in the caller's transaction, failing
// closed (one that is not an active einvoice_provider with a secret_ref,
// or the stand-in in production, is refused with the integration
// context's problem); Credentials resolves its credentials from the vault
// with no transaction open, so it is called after the transaction that
// read the connector committed (As built E3); Names gives connectors'
// names by id.
type Connectors interface {
	Provider(ctx context.Context, id uuid.UUID) (Connector, error)
	Credentials(ctx context.Context, c Connector) (string, error)
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Company is a legal entity as the context names it.
type Company struct {
	ID         uuid.UUID
	Code, Name string
}

// Companies reads the tenant's legal entities (tenancy context): by
// company code, and by id for a page of documents. Company access is not
// applied: the context applies it.
type Companies interface {
	ByCode(ctx context.Context, code string) (Company, bool, error)
	Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Company, error)
}

// Runs keeps a fetch's or a send's Operation (operations, 04 §4.8): Start
// queues one for the dispatcher; Queue records, once, one with the id a
// scheduled fetch's workflow chose, naming that workflow; Cancelled says
// whether one was cancelled, under its row lock with lock (a send's steps
// end its submission then, As built E3).
type Runs interface {
	Start(ctx context.Context, resource string, request any) (gen.Operation, error)
	Queue(ctx context.Context, id uuid.UUID, resource string, request any, workflowID string) error
	Cancelled(ctx context.Context, id uuid.UUID, lock bool) (bool, error)
	Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error
	Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error
}

// Endpoints are the einvoicing context's operations.
type Endpoints struct {
	Connectors Connectors
	Companies  Companies
	Runs       Runs
	// Objects keeps the documents' bytes write-once (ADR-0016); nil: a
	// fetch is refused, as there is nowhere to keep what it fetches.
	Objects blobstore.Store
	// Adapter gives the provider a connector's adapter names (Adapters).
	Adapter func(Connector) (Provider, error)
	Pages   *pagination.Codec
	Now     func() time.Time
	// Sales reads the posted customer documents that outgoing e-invoices
	// render (ADR-0061 §9, As built E2).
	Sales Sales
	// Sends keeps a send's Operation (As built E3), as Runs a fetch's.
	Sends Runs
	// Settings reads the companies' calendars, which the deadlines of the
	// worklist of documents not transmitted count business days in.
	Settings Settings
}

// Within runs fn in a transaction of the fetch's tenant, as Fetcher: the
// workflow's activities give it, so a step talks to the provider with no
// transaction open between the transactions that read the connector and
// record what it fetched (ADR-0061 §4).
type Within = func(ctx context.Context, fn func(ctx context.Context) error) error
