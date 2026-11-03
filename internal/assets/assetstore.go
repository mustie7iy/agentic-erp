package assets

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// termsInUse refuses a change of terms depreciation has been posted with.
// The database refuses it too (asset_terms_in_use); its problem is not
// registered, since every change of terms reads the asset FOR UPDATE and
// checks first, and so does every posting that sets booked_through.
func termsInUse() *problem.Problem {
	return problem.Conflict("assets.terms_in_use", "Depreciation has been posted with the asset's terms in this ledger, so they are fixed; "+
		"a change in estimate is not offered.")
}

// assetSelect reads assets with their current centers, those of the
// assignment with the highest seq (ADR-0057 §8).
const assetSelect = `SELECT a.id, a.legal_entity_id, a.asset_number, a.class_code, a.description, a.status, a.capitalised_on, a.last_seq,
		c.cost_center_id, c.profit_center_id, a.version, a.created_at, a.created_by, a.updated_at, a.updated_by
	FROM assets.asset a
	LEFT JOIN LATERAL (SELECT r.cost_center_id, r.profit_center_id FROM assets.asset_assignment r
		WHERE r.tenant_id = a.tenant_id AND r.asset_id = a.id ORDER BY r.seq DESC LIMIT 1) c ON true`

func scanAsset(r pgx.Row) (Asset, error) {
	var a Asset
	err := r.Scan(&a.ID, &a.LegalEntityID, &a.Number, &a.Class, &a.Description, &a.Status, &a.CapitalisedOn, &a.LastSeq,
		&a.CostCenterID, &a.ProfitCenterID, &a.Version, &a.CreatedAt, &a.CreatedBy, &a.UpdatedAt, &a.UpdatedBy)
	return a, errs.Wrapf(err, "scan asset")
}

// readAsset reads an asset of a legal entity the caller may act in, with
// its terms, locked for a change when forUpdate; any other is not found.
func readAsset(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Asset, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Asset{}, err
	}
	q := assetSelect + ` WHERE a.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF a`
	}
	a, err := scanAsset(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(a.LegalEntityID)):
		return Asset{}, problem.NotFoundOf("asset")
	case err != nil:
		return Asset{}, fmt.Errorf("assets: asset %s: %w", id, err)
	}
	terms, err := readTerms(ctx, tx, []uuid.UUID{id})
	a.Terms = terms[id]
	return a, err
}

// readTerms reads the assets' terms, by asset.
func readTerms(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID][]Terms, error) {
	rows, _ := tx.Query(ctx, `SELECT asset_id, ledger_id, method, useful_life_months, trim_scale(declining_factor)::text, residual_value,
			depreciation_start, booked_through_year, booked_through_period
		FROM assets.asset_terms WHERE asset_id = ANY($1)`, ids)
	out := map[uuid.UUID][]Terms{}
	var asset uuid.UUID
	var t Terms
	_, err := pgx.ForEachRow(rows, []any{&asset, &t.LedgerID, &t.Method, &t.UsefulLifeMonths, &t.DecliningFactor, &t.Residual,
		&t.DepreciationStart, &t.BookedYear, &t.BookedPeriod}, func() error {
		out[asset] = append(out[asset], t)
		t = Terms{} // the next row's pointers are scanned into a new value
		return nil
	})
	return out, errs.Wrapf(err, "assets: terms of %d assets", len(ids))
}

// insertAsset writes a new planned asset, its first assignment (seq 1,
// valid from -infinity: the asset's centers from its creation) and its
// terms.
func insertAsset(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, number string, in NewAsset, pc *uuid.UUID, terms []Terms) error {
	if _, err := tx.Exec(ctx, `INSERT INTO assets.asset (tenant_id, id, legal_entity_id, asset_number, class_code, description, status, last_seq,
			created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, 'planned', 1, $7, $7)`,
		p.TenantID, id, in.LegalEntityID, number, in.Class, in.Description, p.ID); err != nil {
		return fmt.Errorf("assets: insert asset %s: %w", number, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.asset_assignment (tenant_id, asset_id, seq, valid_from, cost_center_id, profit_center_id, created_by)
		VALUES ($1, $2, 1, '-infinity', $3, $4, $5)`, p.TenantID, id, in.CostCenterID, pc, p.ID); err != nil {
		return fmt.Errorf("assets: assign asset %s: %w", number, err)
	}
	return writeTerms(ctx, tx, p, id, terms)
}

// writeTerms inserts or replaces the asset's terms in their ledgers.
func writeTerms(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, terms []Terms) error {
	for _, t := range terms {
		if _, err := tx.Exec(ctx, `INSERT INTO assets.asset_terms AS t (tenant_id, asset_id, ledger_id, method, useful_life_months, declining_factor,
				residual_value, depreciation_start, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $9)
			ON CONFLICT (tenant_id, asset_id, ledger_id) DO UPDATE SET method = EXCLUDED.method, useful_life_months = EXCLUDED.useful_life_months,
				declining_factor = EXCLUDED.declining_factor, residual_value = EXCLUDED.residual_value,
				depreciation_start = EXCLUDED.depreciation_start, version = t.version + 1, updated_by = $9`,
			p.TenantID, id, t.LedgerID, t.Method, t.UsefulLifeMonths, t.DecliningFactor, t.Residual, t.DepreciationStart, p.ID); err != nil {
			return fmt.Errorf("assets: terms of asset %s in ledger %s: %w", id, t.LedgerID, err)
		}
	}
	return nil
}

// touch moves the asset's version and sets its description: its version
// moves with every change of its master or terms (ADR-0057 §5).
func touch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, description string) error {
	_, err := tx.Exec(ctx, `UPDATE assets.asset SET description = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, description, p.ID)
	return errs.Wrapf(err, "assets: change asset %s", id)
}

// setStatus moves the asset to a status, guarded by the state trigger
// (assets.check_asset).
func setStatus(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, status string) error {
	_, err := tx.Exec(ctx, `UPDATE assets.asset SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, status, p.ID)
	return errs.Wrapf(err, "assets: asset %s to %s", id, status)
}

// record audits a change of an asset, with its terms per ledger, and
// publishes it as AssetChanged, as the asset is after it.
func record(ctx context.Context, a Asset, action, change string) error {
	terms := make([]map[string]any, len(a.Terms))
	for i, t := range a.Terms {
		terms[i] = map[string]any{"ledger_id": t.LedgerID, "method": t.Method, "useful_life_months": t.UsefulLifeMonths,
			"declining_factor": t.DecliningFactor, "residual_value": int64(t.Residual), "depreciation_start": outbox.Date(t.DepreciationStart)}
	}
	detail := map[string]any{"legal_entity_id": a.LegalEntityID, "asset_number": a.Number, "class_code": a.Class, "description": a.Description,
		"status": a.Status, "cost_center_id": a.CostCenterID, "profit_center_id": a.ProfitCenterID, "terms": terms}
	return audit.Record(ctx, "assets.asset."+action, "assets.asset", a.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_changed", AggregateType: "assets.asset", AggregateID: a.ID,
			Payload: map[string]any{"asset_id": a.ID, "legal_entity_id": a.LegalEntityID, "asset_number": a.Number, "class_code": a.Class,
				"change": change, "status": a.Status, "version": a.Version}})
}
