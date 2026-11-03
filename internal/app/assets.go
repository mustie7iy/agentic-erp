package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Assets is the assets context as the API wires it (ADR-0057 §2): a leaf
// that reads the configuration, tenancy, ledger and operations contexts
// through the adapters below, so none of them imports it and it imports
// none of them.
func Assets(cfg Config) *assets.Endpoints {
	return &assets.Endpoints{Settings: configuration.Settings{}, Companies: assetCompanies{}, Ledger: assetLedger{}, Centers: assetCenters{},
		Numbers: configuration.Numbers{}, Runs: assetRuns{}, Pages: cfg.Pages, Now: cfg.Now}
}

// assetCompanies gives the assets context a legal entity by company code
// or id, with its local currency.
type assetCompanies struct{}

func assetCompany(le tenancy.Entity) assets.Company {
	return assets.Company{ID: le.ID, Code: le.Code, Name: le.Name, Currency: le.LocalCurrency}
}

func (assetCompanies) ByCode(ctx context.Context, code string) (assets.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.EntityByCode(ctx, code)
	return assetCompany(le), ok, err
}

func (assetCompanies) Company(ctx context.Context, id uuid.UUID) (assets.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return assetCompany(le), ok, err
}

// assetLedger gives the assets context the ledgers and fiscal years.
type assetLedger struct{}

func (assetLedger) Ledgers(ctx context.Context, legalEntity *uuid.UUID) ([]assets.LedgerRef, error) {
	ls, err := ledger.AssetReads{}.Ledgers(ctx, legalEntity)
	out := make([]assets.LedgerRef, len(ls))
	for i, l := range ls {
		out[i] = assets.LedgerRef{ID: l.ID, Code: l.Code, Name: l.Name}
	}
	return out, err
}

func (assetLedger) FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error) {
	return ledger.Calendar{LegalEntities: tenancy.Directory{}}.FiscalYear(ctx, legalEntity, date)
}

// assetCenters gives the assets context an asset's centers derived and
// checked by the ledger, and centers' names.
type assetCenters struct{}

func (assetCenters) Assignment(ctx context.Context, legalEntity uuid.UUID, date time.Time, year int16, account string,
	costCenter, profitCenter *uuid.UUID) (assets.Assignment, error) {
	a, err := ledger.AssetReads{}.Assignment(ctx, legalEntity, date, year, account, costCenter, profitCenter)
	return assets.Assignment{ProfitCenterID: a.ProfitCenterID, CostCenterRequired: a.CostCenterRequired}, err
}

func (assetCenters) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]assets.CenterName, error) {
	names, err := ledger.AssetReads{}.CenterNames(ctx, ids)
	out := make(map[uuid.UUID]assets.CenterName, len(names))
	for id, n := range names {
		out[id] = assets.CenterName{Code: n.Code, Name: n.Name}
	}
	return out, err
}

// assetRuns keeps a depreciation run's Operation (ADR-0057 §7).
type assetRuns struct{}

func (assetRuns) Start(ctx context.Context, resource string, request any) (gen.Operation, error) {
	o, err := operations.StartFor(ctx, assets.KindDepreciate, request, resource)
	if err != nil {
		return gen.Operation{}, err
	}
	return operations.ToAPI(o), nil
}

func (assetRuns) Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error {
	_, err := operations.Succeed(ctx, id, resource, result)
	return err
}

func (assetRuns) Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error {
	_, err := operations.Fail(ctx, id, p)
	return err
}
