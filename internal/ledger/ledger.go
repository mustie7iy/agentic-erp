// Package ledger is the general ledger context (03-data-model §3.7,
// ADR-0005, ADR-0006): an insert-only journal in three currencies,
// corrections by reversal, clearing of open items in a separate insert-only
// table, posting periods per legal entity and ledger, exchange rates, and
// period balances derived by a projector, and each purchase order item's
// GR/IR position on its own lines (grir.go, T33).
//
// Foreign-currency revaluation (ADR-0042, T48) values open items and
// flagged accounts' balances at a key date; its settings are kept from the
// configuration (fxsetup.go).
//
// Cost centers, profit centers and segments (ADR-0050, T56a) are the
// ledger's master data: their texts change at once, their creation and
// effective-dated periods, segment and companies only through a center
// change a controller approves (centerchange.go, CenterChanges). Their
// standard and alternative hierarchies (T56b) change the same way
// (hierarchies.go, hierarchychange.go).
//
// Journal lines carry their cost center, profit center and segment from
// T56d (ADR-0050 §5), derived and validated at posting (assignment.go).
//
// Parallel ledgers (ADR-0045, T52): the leading ledger 0L and ledgers kept
// under other accounting principles, assigned per company, and ledger
// groups (ledgers.go).
//
// A locked period returns to closed only through an approved period_unlock
// approval request (T32): the ledger opens it (RequestUnlock) and is called
// back when it completes (PeriodUnlocks).
//
// Accounts, fiscal calendars and periods are master data of the
// configuration document: the configuration context hands an activated
// document to Setup, which applies it here. Postings take their document
// numbers from the configuration's number ranges (journal_<type>, e.g.
// journal_sa for document type SA).
package ledger

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LegalEntities is what the ledger reads about legal entities and the
// tenant (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
	EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
	GroupCurrency(ctx context.Context) (string, error)
}

// Partners is what the ledger asks the partner context before posting a
// line on a reconciliation account.
type Partners interface {
	// PostingBlock says why the partner may not be posted on a
	// reconciliation account of the kind in the legal entity, or "".
	PostingBlock(ctx context.Context, partnerID, legalEntity uuid.UUID, kind string) (string, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// OrderNumbers names purchase orders by their document numbers
// (procurement context): the GR/IR report shows them beside the orders'
// ids.
type OrderNumbers interface {
	Numbers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Endpoints implements the ledger part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Partners      Partners
	Numbers       Numbers
	Orders        OrderNumbers
	Approvals     Opener // opens the unlock requests of locked periods
	// Settings gives each company's time zone (T72n4).
	Settings Settings
	// Now is the clock center changes read today by, as the controlling
	// rules' setup does (Setup.Now): a default center stays usable from
	// the company's date in its time zone on (keepsDefaultCostCenter).
	// nil: the wall clock.
	Now func() time.Time
	// People tells whether a person is still active: the owner of an entry
	// template, for whom the recurrer parks (ADR-0056).
	People People
}

// People is the identity context's directory of principals.
type People interface {
	Active(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error)
}

// days tells the companies' dates by the endpoints' clock.
func (e *Endpoints) days() companyDays {
	return companyDays{now: e.Now, entities: e.LegalEntities, settings: e.Settings}
}
