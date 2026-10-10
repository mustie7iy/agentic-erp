// Package ledger is the general ledger context (03-data-model §3.7,
// ADR-0005, ADR-0006): an insert-only journal in three currencies,
// corrections by reversal, clearing of open items in a separate insert-only
// table, posting periods per legal entity and ledger, exchange rates, and
// period balances derived by a projector.
//
// Accounts, fiscal calendars and periods are master data of the
// configuration document: the configuration context hands an activated
// document to Setup, which applies it here. Postings take their document
// numbers from the configuration's number ranges (journal_<type>, e.g.
// journal_sa for document type SA).
package ledger

import (
	"context"

	"github.com/google/uuid"

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

// Endpoints implements the ledger part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Partners      Partners
	Numbers       Numbers
}
