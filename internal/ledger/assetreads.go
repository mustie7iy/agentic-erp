package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// AssetReads is what the ledger publishes to the assets context (ADR-0057
// §2, T57b): the ledgers a company keeps, an asset's centers derived and
// checked as a posting on its depreciation account would derive and check
// them (ADR-0050 §5), and the codes and names of centers. It runs in the
// request's transaction under row-level security (ADR-0003); assets reads
// no ledger table itself, and the composition root adapts it to the
// assets context's ports.
type AssetReads struct{}

// LedgerRef is a ledger by its code and name.
type LedgerRef struct {
	ID         uuid.UUID
	Code, Name string
	Leading    bool
}

// Ledgers returns the tenant's ledgers, the leading one first and then by
// code; with a legal entity, only those the company keeps (the leading
// ledger alone for a company assigned none, ADR-0045).
func (AssetReads) Ledgers(ctx context.Context, legalEntity *uuid.UUID) ([]LedgerRef, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, code, name, is_leading FROM ledger.ledger
		WHERE $1::uuid IS NULL OR ledger.company_keeps($1, id) ORDER BY NOT is_leading, code`, legalEntity)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LedgerRef, error) {
		var l LedgerRef
		return l, r.Scan(&l.ID, &l.Code, &l.Name, &l.Leading)
	})
	return out, errs.Wrapf(err, "ledger: ledgers")
}

// AssetAssignment is an asset's centers as AssetReads.Assignment derives
// them: the profit center (nil for none), and whether the asset needs a
// cost center it was not given.
type AssetAssignment struct {
	ProfitCenterID     *uuid.UUID
	CostCenterRequired bool
}

// Assignment derives and checks the centers an asset of the legal entity
// is created with on date, in fiscal year year, as a posting on its
// depreciation account (by number in the company's chart) dated then
// would (ADR-0057 §8): a cost center given must be one of the company's
// that the account may take, valid and not locked for costs on the date,
// and gives its profit center then (422 ledger.profit_center_mismatch for
// another given); without a cost center the profit center is as given,
// checked as a posting checks it, and none is derived. CostCenterRequired
// reports an account that needs a cost center while cost center
// accounting applies in the year (primary cost) and has no default in the
// company, so its depreciation could not post without one. Refusals name
// cost_center_id and profit_center_id. The centers are not locked: every
// posting checks them again.
func (AssetReads) Assignment(ctx context.Context, legalEntity uuid.UUID, date time.Time, year int16, account string,
	costCenter, profitCenter *uuid.UUID) (AssetAssignment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return AssetAssignment{}, err
	}
	// Activation sets up every account the configuration's asset accounts
	// name in the company (ADR-0057 §3), so a missing one is broken data.
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT a.id FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.number = $2`, legalEntity, account).Scan(&id); err != nil {
		return AssetAssignment{}, fmt.Errorf("ledger: account %s of %s: %w", account, legalEntity, err)
	}
	lines := []Line{{AccountID: id, CostCenterID: costCenter, ProfitCenterID: profitCenter}}
	r, err := readAssignRules(ctx, tx, legalEntity, year, lines)
	if err != nil || r.accounts == nil {
		return AssetAssignment{ProfitCenterID: profitCenter}, err
	}
	a := r.accounts[id]
	if costCenter == nil {
		required := a.element == config.CostElementPrimaryCost && r.costOn && a.defaultCC == nil
		return AssetAssignment{ProfitCenterID: profitCenter, CostCenterRequired: required},
			asAsset(checkLineProfitCenters(ctx, tx, legalEntity, date, lines, make([]lineCenters, 1), false))
	}
	if a.element == config.CostElementNone { // as deriveCostCenters refuses it, whose derivation an asset does not take
		return AssetAssignment{}, asAsset(centerRefused("ledger.cost_center_invalid", 0, "cost_center_id", "The cost center", reasonNotAllowed))
	}
	found := make([]lineCenters, 1)
	if err := checkLineCostCenters(ctx, tx, legalEntity, date, lines, r, found, false); err != nil {
		return AssetAssignment{}, asAsset(err)
	}
	if err := checkLineProfitCenters(ctx, tx, legalEntity, date, lines, found, false); err != nil {
		return AssetAssignment{}, asAsset(err)
	}
	return AssetAssignment{ProfitCenterID: lines[0].ProfitCenterID}, nil
}

// asAsset makes a refusal of the one line Assignment checks name the
// asset's own fields: lines[0].<field> becomes <field>, and a detail
// starting "Line 1: " loses that start. Other errors pass unchanged.
func asAsset(err error) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	for k := range p.Errors {
		p.Errors[k].Field = strings.TrimPrefix(p.Errors[k].Field, "lines[0].")
	}
	p.Detail = strings.TrimPrefix(p.Detail, "Line 1: ")
	return p
}

// CenterName is a cost or profit center by its code and name.
type CenterName struct{ Code, Name string }

// CenterNames returns the codes and names of the cost and profit centers
// among ids, by id; an id of neither is left out.
func (AssetReads) CenterNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CenterName, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]CenterName{}
	rows, _ := tx.Query(ctx, `SELECT id, code, name FROM ledger.cost_center WHERE id = ANY($1)
		UNION ALL SELECT id, code, name FROM ledger.profit_center WHERE id = ANY($1)`, ids)
	var id uuid.UUID
	var n CenterName
	_, err = pgx.ForEachRow(rows, []any{&id, &n.Code, &n.Name}, func() error { out[id] = n; return nil })
	return out, errs.Wrapf(err, "ledger: names of %d centers", len(ids))
}
