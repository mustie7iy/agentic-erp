package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Asset statuses (ADR-0057 §5).
const (
	StatusPlanned   = "planned"
	StatusActive    = "active"
	StatusRetired   = "retired"
	StatusCancelled = "cancelled"
)

// Asset is a stored asset with its current centers (its latest
// assignment's) and its terms per ledger.
type Asset struct {
	ID, LegalEntityID            uuid.UUID
	Number, Class, Description   string
	Status                       string
	CapitalisedOn                *time.Time
	LastSeq                      int32
	CostCenterID, ProfitCenterID *uuid.UUID
	Terms                        []Terms
	Values                       map[uuid.UUID]Values // by ledger, from its movements
	Version                      int32
	CreatedAt, UpdatedAt         time.Time
	CreatedBy, UpdatedBy         uuid.UUID
}

// Terms are an asset's depreciation terms in one ledger (ADR-0057 §7);
// Residual is in minor units of the company's local currency, and
// BookedYear and BookedPeriod are set once depreciation is posted.
type Terms struct {
	LedgerID                 uuid.UUID
	Method                   string
	UsefulLifeMonths         int32
	DecliningFactor          *string
	Residual                 money.Minor
	DepreciationStart        *time.Time
	BookedYear, BookedPeriod *int16
}

// Values are an asset's values in one ledger, the sums of its movements
// there (ADR-0057 §5): cost and accumulated depreciation (negative, a
// credit) in local and group currency.
type Values struct {
	Cost, CostGC, Accumulated, AccumulatedGC money.Minor
}

// booked reports whether depreciation has been posted in the terms'
// ledger, after which they are fixed (decided by default 8).
func (t Terms) booked() bool { return t.BookedYear != nil }

// NewAsset is an asset to create.
type NewAsset struct {
	LegalEntityID                uuid.UUID
	Class, Description           string
	CostCenterID, ProfitCenterID *uuid.UUID
}

// company is the legal entity the caller may act in, else not found.
func (e *Endpoints) company(ctx context.Context, id uuid.UUID) (Company, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Company{}, err
	}
	if !access.Allows(id) {
		return Company{}, problem.NotFoundOf("legal entity")
	}
	co, ok, err := e.Companies.Company(ctx, id)
	if err != nil {
		return Company{}, fmt.Errorf("assets: legal entity %s: %w", id, err)
	}
	if !ok {
		return Company{}, problem.NotFoundOf("legal entity")
	}
	return co, nil
}

// classOf is the class and its accounts in the company, refusing a class
// the active document lacks (422 assets.class_unknown) or one the company
// has no asset accounts for (422 assets.class_not_in_company).
func classOf(doc *config.Config, co Company, code string) (*config.AssetClass, *config.AssetAccounts, error) {
	cls := doc.AssetClass(code)
	if cls == nil {
		return nil, nil, invalid("assets.class_unknown", "The configuration has no asset class "+code+".", "class_code", "an asset class of the configuration")
	}
	var accounts *config.AssetAccounts
	if c := doc.Company(co.Code); c != nil {
		accounts = c.AssetAccountsOf(code)
	}
	if accounts == nil {
		return nil, nil, invalid("assets.class_not_in_company", fmt.Sprintf("Company %s has no asset accounts for class %s, so it cannot hold its assets.", co.Code, code),
			"class_code", "a class the company has asset accounts for")
	}
	return cls, accounts, nil
}

// invalid is a 422 on one field.
func invalid(code, detail, field, message string) *problem.Problem {
	return problem.Unprocessable(code, detail, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// createAsset creates a planned asset (ADR-0057 §5, §7, §8): numbered from
// the company's asset range of the fiscal year its date today falls in,
// with its class's terms in every ledger the company keeps that the class
// has terms in, and its first assignment (seq 1, valid from -infinity)
// with the profit center derived from the cost center today.
func (e *Endpoints) createAsset(ctx context.Context, in NewAsset) (Asset, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Asset{}, err
	}
	if err := problem.RequireText("description", in.Description); err != nil {
		return Asset{}, err
	}
	co, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return Asset{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Asset{}, fmt.Errorf("assets: configuration: %w", err)
	}
	cls, accounts, err := classOf(doc, co, in.Class)
	if err != nil {
		return Asset{}, err
	}
	today, err := doc.CompanyDay(co.Code, clock.Now(e.Now))
	if err != nil {
		return Asset{}, fmt.Errorf("assets: %w", err)
	}
	year, err := e.Ledger.FiscalYear(ctx, co.ID, today.Date)
	if err != nil {
		return Asset{}, fmt.Errorf("assets: fiscal year of %s: %w", co.Code, err)
	}
	pc, err := e.assign(ctx, co.ID, today.Date, year, accounts.Depreciation, in)
	if err != nil {
		return Asset{}, err
	}
	number, err := e.Numbers.Next(ctx, co.ID, "asset", year)
	if err != nil {
		return Asset{}, fmt.Errorf("assets: asset number of %s in %d: %w", co.Code, year, err)
	}
	terms, err := e.classTerms(ctx, co.ID, cls)
	if err != nil {
		return Asset{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := insertAsset(ctx, tx, p, id, number, in, pc, terms); err != nil {
		return Asset{}, err
	}
	return written(ctx, tx, id, "create", "created")
}

// assign derives the asset's profit center from its cost center on the
// day, as its depreciation would be posted (Centers.Assignment), and
// refuses an asset whose depreciation needs a cost center it lacks (422
// assets.cost_center_required).
func (e *Endpoints) assign(ctx context.Context, le uuid.UUID, day time.Time, year int16, account string, in NewAsset) (*uuid.UUID, error) {
	a, err := e.Centers.Assignment(ctx, le, day, year, account, in.CostCenterID, in.ProfitCenterID)
	if err != nil {
		return nil, fmt.Errorf("assets: centers: %w", err)
	}
	if a.CostCenterRequired {
		return nil, invalid("assets.cost_center_required", fmt.Sprintf("The class's depreciation account %s needs a cost center and has no default "+
			"cost center in the company; give the asset one.", account), "cost_center_id", "a cost center of the company")
	}
	return a.ProfitCenterID, nil
}

// classTerms are the class's terms in each ledger the company keeps that
// the class has terms in; a ledger without them gets terms by PATCH.
func (e *Endpoints) classTerms(ctx context.Context, le uuid.UUID, cls *config.AssetClass) ([]Terms, error) {
	ledgers, err := e.Ledger.Ledgers(ctx, &le)
	if err != nil {
		return nil, fmt.Errorf("assets: ledgers of %s: %w", le, err)
	}
	var out []Terms
	for _, l := range ledgers {
		t := cls.TermsIn(l.Code)
		if t == nil {
			continue
		}
		terms := Terms{LedgerID: l.ID, Method: t.Method, UsefulLifeMonths: int32(t.UsefulLifeMonths)}
		if t.DecliningFactor != "" {
			factor := t.DecliningFactor
			terms.DecliningFactor = &factor
		}
		out = append(out, terms)
	}
	return out, nil
}

// written reads an asset just written back, and audits and publishes the
// change.
func written(ctx context.Context, tx pgx.Tx, id uuid.UUID, action, change string) (Asset, error) {
	a, err := readAsset(ctx, tx, id, false)
	if err != nil {
		return Asset{}, err
	}
	return a, record(ctx, a, action, change)
}

// cancelAsset cancels a planned asset without movements (ADR-0057 §5):
// any other is 409 assets.asset_not_planned. A planned asset has
// movements once an acquisition onto it was reversed (T57d); they stay its
// history, so it is kept.
func (e *Endpoints) cancelAsset(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Asset, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Asset{}, err
	}
	a, err := readAsset(ctx, tx, id, true)
	if err != nil {
		return Asset{}, err
	}
	if err := ifMatch(a.Version); err != nil {
		return Asset{}, err
	}
	if a.Status != StatusPlanned {
		return Asset{}, problem.Conflict("assets.asset_not_planned", fmt.Sprintf("Asset %s is %s; only a planned asset is cancelled.", a.Number, a.Status))
	}
	var moved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM assets.movement WHERE asset_id = $1)`, id).Scan(&moved); err != nil {
		return Asset{}, fmt.Errorf("assets: movements of asset %s: %w", a.Number, err)
	}
	if moved {
		return Asset{}, problem.Conflict("assets.asset_not_planned", fmt.Sprintf("Asset %s has movements; only a planned asset without any is "+
			"cancelled.", a.Number))
	}
	if err := setStatus(ctx, tx, p, id, StatusCancelled); err != nil {
		return Asset{}, err
	}
	return written(ctx, tx, id, "cancel", "cancelled")
}
