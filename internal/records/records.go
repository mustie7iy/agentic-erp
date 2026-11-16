// Package records is the records management context (ADR-0079;
// 02-architecture §2.2): which fiscal years of a company's documents are
// past retention under the tenant's retention rules (T99a), and the
// archiving runs that write a year past retention to an archive file in
// object storage with a manifest (T99b). The rules themselves are a
// section of the configuration document (companies[].retention[]),
// changed only through its change requests and approval; none is shipped
// or defaulted for any jurisdiction.
//
// The context is a leaf (ADR-0079 §1): it reads tenancy's legal
// entities, the active configuration and the ledger's fiscal calendar
// and journal through its ports, by the types their published APIs give,
// and no context imports it. Its own schema holds the archiving runs and
// its outbox. It changes nothing anywhere else: archiving writes a file
// and deletes nothing (ADR-0079 §5).
package records

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LegalEntities reads the tenant's legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Settings reads the active configuration version and its number.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Calendar reads a legal entity's set-up fiscal years (ledger context).
type Calendar interface {
	Years(ctx context.Context, legalEntity uuid.UUID) ([]ledger.FiscalYearSpan, error)
}

// Journal reads a company's fiscal year for an archiving run (ledger
// context, ledger.Archive): whether it is closed, and its journal
// entries with their lines.
type Journal interface {
	YearClosed(ctx context.Context, legalEntity uuid.UUID, year int16) (bool, error)
	Entries(ctx context.Context, legalEntity uuid.UUID, year int16, fn func(ledger.ArchivedEntry) error) error
}

// Requesters tells whether the person who asked for a run may still act
// (identity context, identity.Actors): one blocked or deprovisioned since
// is refused with a business problem (PO 2026-11-03).
type Requesters interface {
	MayAct(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// People names principals (identity context): who asked for a run.
type People interface {
	PersonNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Endpoints implements the records part of the generated API.
type Endpoints struct {
	LegalEntities LegalEntities
	Settings      Settings
	Calendar      Calendar
	Journal       Journal
	People        People
	Privacy       Privacy // the partner context's blocked personal data (T100c)
	Pages         *pagination.Codec
	Objects       blobstore.Store // archive files; nil: no run is asked for and no file is read
	// PartEntries is the most entries a part of an archive file holds; 0
	// is ArchivePartEntries (tests write parts of a few).
	PartEntries int
	Now         func() time.Time // nil: the wall clock
}
