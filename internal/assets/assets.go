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
package assets

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// Settings reads the active configuration (configuration context), whose
// asset classes and asset accounts the context reads.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Company is a legal entity as the assets context reads it.
type Company struct {
	ID   uuid.UUID
	Code string
	Name string
}

// Companies reads legal entities (tenancy context): the one of a company
// code, ok false when there is none.
type Companies interface {
	ByCode(ctx context.Context, code string) (Company, bool, error)
}

// Endpoints implements the assets context's operations of the public API.
type Endpoints struct {
	Settings  Settings
	Companies Companies
	Pages     *pagination.Codec
}
