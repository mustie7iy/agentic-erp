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
// compared with their hash. The provider's only adapter is the
// stand-in (StandIn), which production refuses; the ERP polls and opens
// no endpoint for a provider (ADR-0061 Q1, Q3). Validation, entering,
// credit notes and people's resolution of held documents follow in
// T65c-T65f.
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

// Statuses of an e-invoice (ADR-0061 §7); T65a records documents only.
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

// Connector is an einvoice_provider connector resolved for use (ADR-0061
// §3): the adapter, the provider's base URL, each company's participant
// identifier by company code, the format and the credentials.
type Connector struct {
	ID           uuid.UUID
	Name         string
	Adapter      string
	BaseURL      string
	Participants map[string]string // company code -> participant identifier
	Format       string
	Secret       string
}

// Connectors reads the einvoice_provider connectors (integration context,
// ADR-0061 §2): Provider resolves one, failing closed (one that is not
// an active einvoice_provider with credentials that resolve, or the
// stand-in in production, is refused with the integration context's
// problem); Names gives connectors' names by id.
type Connectors interface {
	Provider(ctx context.Context, id uuid.UUID) (Connector, error)
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

// Runs keeps a fetch's Operation (operations, 04 §4.8): Start queues one
// for the dispatcher; Queue records, once, one with the id a scheduled
// fetch's workflow chose, naming that workflow.
type Runs interface {
	Start(ctx context.Context, resource string, request any) (gen.Operation, error)
	Queue(ctx context.Context, id uuid.UUID, resource string, request any, workflowID string) error
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
}

// Within runs fn in a transaction of the fetch's tenant, as Fetcher: the
// workflow's activities give it, so a step talks to the provider with no
// transaction open between the transactions that read the connector and
// record what it fetched (ADR-0061 §4).
type Within = func(ctx context.Context, fn func(ctx context.Context) error) error
