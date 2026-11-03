// Package assets is the asset accounting context (ADR-0057; SAP FI-AA):
// asset classes, assets, their movements, acquisitions, retirements,
// transfers and depreciation runs. It is a leaf (ADR-0057 §2): it reads
// the other contexts only through ports it declares, which the
// composition root (internal/app) adapts and which run in the request's
// transaction under row-level security (ADR-0003), and no context imports
// it (internal/layout).
//
// T57a builds its classes: the asset classes and each company's asset
// accounts are configuration (ADR-0057 §3, internal/config), validated
// there and in ledger.Setup, which makes the acquisition cost and
// accumulated depreciation accounts asset reconciliation accounts that
// only this context posts to. ListAssetClasses reads them from the active
// document.
//
// T57b builds the asset master (ADR-0057 §5, §7, §8): assets numbered from
// their company's asset range, with depreciation terms per ledger the
// company keeps, copied from their class and changeable until depreciation
// is posted, and their cost and profit center as insert-only assignment
// rows, the profit center derived from the cost center (ADR-0050 §5).
// Every change is audited (ADR-0011) and published as AssetChanged through
// the context's outbox (ADR-0007); a change reads the asset FOR UPDATE and
// checks the caller's version (ADR-0008); amounts are minor units
// (ADR-0005); the schema is the tenant's under RLS (ADR-0003). The ports
// below are the leaf's to the ledger, tenancy, configuration and
// operations contexts, assets.Runs (a depreciation run's Operation, T57f)
// among them.
package assets

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Settings reads the active configuration (configuration context), whose
// asset classes and asset accounts the context reads.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Company is a legal entity as the assets context reads it: its company
// code, name and local currency.
type Company struct {
	ID                   uuid.UUID
	Code, Name, Currency string
}

// Companies reads legal entities (tenancy context): the one of a company
// code, and the one of an id, ok false when there is none.
type Companies interface {
	ByCode(ctx context.Context, code string) (Company, bool, error)
	Company(ctx context.Context, id uuid.UUID) (Company, bool, error)
}

// LedgerRef is a ledger by its code and name.
type LedgerRef struct {
	ID         uuid.UUID
	Code, Name string
}

// Ledger reads the ledger context: the tenant's ledgers (leading first,
// then by code), those a company keeps when legalEntity is given, and the
// fiscal year a company's date falls in (422 ledger.no_fiscal_period
// outside every one).
type Ledger interface {
	Ledgers(ctx context.Context, legalEntity *uuid.UUID) ([]LedgerRef, error)
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Assignment is an asset's centers as Centers derives them: its profit
// center, and whether it needs a cost center it was not given.
type Assignment struct {
	ProfitCenterID     *uuid.UUID
	CostCenterRequired bool
}

// CenterName is a cost or profit center by its code and name.
type CenterName struct{ Code, Name string }

// Centers reads the ledger's cost and profit centers (ADR-0057 §2, §8):
// an asset's centers derived and checked on a date as a posting on its
// depreciation account (by number) would (ledger.AssetReads.Assignment),
// and centers' codes and names by id.
type Centers interface {
	Assignment(ctx context.Context, legalEntity uuid.UUID, date time.Time, year int16, account string, costCenter, profitCenter *uuid.UUID) (Assignment, error)
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CenterName, error)
}

// Numbers issues document numbers from the company's number ranges
// (configuration context), in the request's transaction.
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// KindDepreciate is the kind of a depreciation run's Operation (ADR-0057 §7).
const KindDepreciate = "assets.depreciate"

// Runs keeps a depreciation run's Operation (operations context, ADR-0057
// §2, §7): Start writes it queued, for the run's resource path, in the
// request's transaction, which the workflows dispatcher then starts;
// Succeed and Fail finish it from the run's activities. assets imports
// neither operations nor the workflow engine.
type Runs interface {
	Start(ctx context.Context, resource string, request any) (gen.Operation, error)
	Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error
	Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error
}

// Endpoints implements the assets context's operations of the public API.
type Endpoints struct {
	Settings  Settings
	Companies Companies
	Ledger    Ledger
	Centers   Centers
	Numbers   Numbers
	Runs      Runs
	Pages     *pagination.Codec
	Now       func() time.Time // the clock of an asset's creation date; nil: the wall clock
}
