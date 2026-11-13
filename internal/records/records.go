// Package records is the records management context (ADR-0079;
// 02-architecture §2.2): which fiscal years of a company's documents are
// past retention under the tenant's retention rules (T99a). The rules
// themselves are a section of the configuration document
// (companies[].retention[]), changed only through its change requests
// and approval; none is shipped or defaulted for any jurisdiction.
//
// The context is a leaf (ADR-0079 §1): it reads tenancy's legal
// entities, the active configuration and the ledger's fiscal calendar
// through its ports, by the types their published APIs give, and no
// context imports it. It stores nothing in T99a and changes nothing
// anywhere: a year past retention may be archived or erased only by a
// later step.
package records

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
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

// Endpoints implements the records part of the generated API.
type Endpoints struct {
	LegalEntities LegalEntities
	Settings      Settings
	Calendar      Calendar
	Now           func() time.Time // nil: the wall clock
}
