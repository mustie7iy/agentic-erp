package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// fxValuation makes the legal entity's foreign-currency valuation the
// company's (ADR-0042): its rate type, the accounts flagged
// revalue_foreign_currency and each revalued account's valuation accounts.
// Valuations the document no longer has are deactivated first, since the
// database keeps a flag while an active valuation names its account
// (company_account_revalue_foreign_currency_valued), and flags are set
// before the valuations that need them.
func fxValuation(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company) error {
	n := len(co.FXValuation.Accounts)
	accounts, adjustments, gains, losses := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, a := range co.FXValuation.Accounts {
		accounts[i], adjustments[i], gains[i], losses[i] = a.Account, a.Adjustment, a.UnrealisedGain, a.UnrealisedLoss
	}
	flagged := []string{} // an empty array, not NULL: none flagged unflags them all
	for _, a := range co.ChartOfAccounts {
		if a.RevalueForeignCurrency {
			flagged = append(flagged, a.Number)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.fx_valuation_account v SET is_active = false, version = v.version + 1, updated_by = $4
		FROM ledger.gl_account g WHERE v.legal_entity_id = $1 AND v.is_active AND g.id = v.account_id AND g.chart_id = $2 AND g.number <> ALL($3)`,
		le, chart, accounts, p.ID); err != nil {
		return fmt.Errorf("ledger: deactivate FX valuations of %s: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.company_account c SET revalue_foreign_currency = g.number = ANY($3), version = c.version + 1, updated_by = $4
		FROM ledger.gl_account g WHERE c.legal_entity_id = $1 AND g.id = c.account_id AND g.chart_id = $2
			AND c.revalue_foreign_currency <> (g.number = ANY($3))`, le, chart, flagged, p.ID); err != nil {
		return fmt.Errorf("ledger: revalued accounts of %s: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.fx_valuation AS f (tenant_id, legal_entity_id, rate_type, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id, legal_entity_id) DO UPDATE SET rate_type = EXCLUDED.rate_type, version = f.version + 1, updated_by = $4
		WHERE f.rate_type <> EXCLUDED.rate_type`, p.TenantID, le, co.FXValuation.RateTypeOrAverage(), p.ID); err != nil {
		return fmt.Errorf("ledger: FX valuation rate type of %s: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.fx_valuation_account AS v (tenant_id, legal_entity_id, account_id, adjustment_account_id, gain_account_id,
			loss_account_id, created_by, updated_by)
		SELECT $1, $2, a.id, j.id, g.id, l.id, $4, $4 FROM unnest($5::text[], $6::text[], $7::text[], $8::text[]) AS d(account, adjustment, gain, loss)
		JOIN ledger.gl_account a ON a.chart_id = $3 AND a.number = d.account
		LEFT JOIN ledger.gl_account j ON j.chart_id = $3 AND j.number = d.adjustment
		JOIN ledger.gl_account g ON g.chart_id = $3 AND g.number = d.gain
		JOIN ledger.gl_account l ON l.chart_id = $3 AND l.number = d.loss
		ON CONFLICT (tenant_id, legal_entity_id, account_id) DO UPDATE SET adjustment_account_id = EXCLUDED.adjustment_account_id,
			gain_account_id = EXCLUDED.gain_account_id, loss_account_id = EXCLUDED.loss_account_id, is_active = true, version = v.version + 1, updated_by = $4
		WHERE (v.adjustment_account_id, v.gain_account_id, v.loss_account_id, v.is_active)
			IS DISTINCT FROM (EXCLUDED.adjustment_account_id, EXCLUDED.gain_account_id, EXCLUDED.loss_account_id, true)`,
		p.TenantID, le, chart, p.ID, accounts, adjustments, gains, losses); err != nil {
		return fmt.Errorf("ledger: FX valuation accounts of %s: %w", co.Code, err)
	}
	return nil
}
