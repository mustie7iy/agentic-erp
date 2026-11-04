package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CloseAccounts is the ledger's port for the accounts a financial close
// reads (ADR-0058 §4, T64h): a period's profit and loss per account, the
// figures of the close's variance report.
type CloseAccounts struct{}

// AccountNet is a profit and loss account with its net movement of a
// period in local currency, debit positive, as the trial balance signs it.
type AccountNet struct {
	AccountID    uuid.UUID
	Number, Name string
	Net          money.Minor
}

// ProfitAndLoss returns every profit and loss account of the legal entity
// (one set up in it, or with a movement in it) with its net movement of
// the fiscal year and period in the leading ledger, in local currency,
// exact at the moment of the read as the trial balance reads it
// (balanceRows), zero for an account without one; by account number. A
// legal entity the caller may not act in has none.
func (CloseAccounts) ProfitAndLoss(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]AccountNet, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	if !access.Allows(legalEntity) {
		return nil, nil
	}
	// Every configured tenant has its leading ledger; without one, uuid.Nil
	// reads no movement and every account is zero.
	leading, _, err := ledgerOrLeading(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `WITH b AS (`+balanceRows+`), n AS (SELECT account_id, sum(debit_lc - credit_lc)::bigint AS net FROM b GROUP BY account_id)
		SELECT a.id, a.number, a.name, coalesce(n.net, 0) FROM ledger.gl_account a LEFT JOIN n ON n.account_id = a.id
		WHERE NOT a.is_balance_sheet AND (n.account_id IS NOT NULL
			OR EXISTS (SELECT FROM ledger.company_account ca WHERE ca.tenant_id = a.tenant_id AND ca.legal_entity_id = $1 AND ca.account_id = a.id))
		ORDER BY a.number COLLATE "C"`,
		legalEntity, []uuid.UUID{leading}, fiscalYear, period, nil)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountNet, error) {
		var a AccountNet
		return a, errs.Wrapf(r.Scan(&a.AccountID, &a.Number, &a.Name, &a.Net), "scan account")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: profit and loss of %s in %d/%d: %w", legalEntity, fiscalYear, period, err)
	}
	return out, nil
}
