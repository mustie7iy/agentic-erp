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

// ParkedEntries is the ledger's port for the parked entries a financial
// close makes (ADR-0058 §4, T64e): an accrual proposal's entry, parked as
// the request's principal exactly as POST /v1/parked-journal-entries
// parks one, its checks, problems and ParkedJournalEntryChanged included.
// It runs in the request's transaction; the composition root gives it the
// ledger's endpoints, whose ports the checks read.
type ParkedEntries struct {
	Endpoints *Endpoints
}

// Parked is an entry parked through ParkedEntries: its id and the debit
// total of its lines, in its currency's minor units.
type Parked struct {
	ID     uuid.UUID
	Debits money.Minor
}

// Park parks the entry as the request's principal: a person owns what they
// park, and an agent's entry no one until a person takes it on (T35d), so
// only a person posts it.
func (p ParkedEntries) Park(ctx context.Context, in NewEntry) (Parked, error) {
	pe, err := p.Endpoints.parkEntry(ctx, in)
	if err != nil {
		return Parked{}, err
	}
	return Parked{ID: pe.ID, Debits: pe.Debits}, nil
}

// CloseAccount is an account as an accrual proposal reads it (T64e): its
// number and name, whether it is a balance sheet account, and whether it
// is postable by hand in the company the read is for: active, set up and
// not blocked there, no reconciliation account and not its GR/IR account
// (Accounts.IsPostable's rule).
type CloseAccount struct {
	ID           uuid.UUID
	Number, Name string
	BalanceSheet bool
	Postable     bool
}

// Accounts returns the accounts among ids, and those set up in the legal
// entity among numbers, by number and id; one that is neither is absent.
// A legal entity the caller may not act in has none.
func (CloseAccounts) Accounts(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID, numbers []string) ([]CloseAccount, error) {
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
	rows, _ := tx.Query(ctx, `SELECT a.id, a.number, a.name, a.is_balance_sheet,
			ca.account_id IS NOT NULL AND a.status = 'active' AND NOT ca.is_posting_blocked AND a.reconciliation_kind = 'none'
				AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $1
					AND d.key = 'gr_ir' AND d.account_id = a.id)
		FROM ledger.gl_account a
		LEFT JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.id = ANY($2) OR (a.number = ANY($3) AND ca.account_id IS NOT NULL)
		ORDER BY a.number COLLATE "C", a.id`, legalEntity, ids, numbers)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CloseAccount, error) {
		var a CloseAccount
		return a, errs.Wrapf(r.Scan(&a.ID, &a.Number, &a.Name, &a.BalanceSheet, &a.Postable), "scan account")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: accounts of %s for an accrual: %w", legalEntity, err)
	}
	return out, nil
}
