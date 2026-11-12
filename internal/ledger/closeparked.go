package ledger

import (
	"context"
	"fmt"
	"time"

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

// ParkedStatus is a parked entry as an accrual proposal derives its
// status from it (ADR-0058 §4, T64f): its status; its debit total in
// Currency, once posted its journal entry's in local currency (what was
// posted, whatever a person changed before), before that its lines' as
// they stand in the entry's currency; once posted, its journal entry (the
// posting group's first) with its document number; and
// once that entry is reversed, the reversal's id and document number.
type ParkedStatus struct {
	ID              uuid.UUID
	Status          string
	Debits          money.Minor
	Currency        string
	JournalEntryID  *uuid.UUID
	DocumentNumber  *string
	ReversalEntryID *uuid.UUID
	ReversalNumber  *string
}

// Statuses returns the parked entries among ids by id; one of a legal
// entity the caller may not act in, or none, is absent. It reads every
// entry asked for in one statement, so a list of proposals reads its
// entries once.
func (ParkedEntries) Statuses(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ParkedStatus, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT p.id, p.status, coalesce(j.currency_lc, p.currency),
			CASE WHEN j.id IS NULL THEN (SELECT coalesce(sum(l.amount) FILTER (WHERE l.amount > 0), 0) FROM ledger.parked_entry_line l
				WHERE l.tenant_id = p.tenant_id AND l.parked_entry_id = p.id AND NOT l.is_removed)
			ELSE (SELECT coalesce(sum(l.amount_lc) FILTER (WHERE l.amount_lc > 0), 0) FROM ledger.journal_line l
				WHERE l.tenant_id = j.tenant_id AND l.fiscal_year = j.fiscal_year AND l.journal_entry_id = j.id) END::bigint,
			j.id, j.document_number, rj.id, rj.document_number
		FROM ledger.parked_entry p
		LEFT JOIN ledger.journal_entry j ON j.tenant_id = p.tenant_id AND j.fiscal_year = p.journal_fiscal_year AND j.id = p.journal_entry_id
		LEFT JOIN ledger.reversal r ON r.tenant_id = j.tenant_id AND r.original_entry_id = j.id
		LEFT JOIN ledger.journal_entry rj ON rj.tenant_id = r.tenant_id AND rj.fiscal_year = r.reversal_year AND rj.id = r.reversal_entry_id
		WHERE p.id = ANY($1) AND ($2 OR p.legal_entity_id = ANY($3))`, ids, access.All, access.IDs)
	// A fresh row each time: pgx reuses a pointer field it scanned before.
	ss, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParkedStatus, error) {
		var s ParkedStatus
		return s, errs.Wrapf(r.Scan(&s.ID, &s.Status, &s.Currency, &s.Debits, &s.JournalEntryID, &s.DocumentNumber,
			&s.ReversalEntryID, &s.ReversalNumber), "scan parked entry status")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: statuses of %d parked entries: %w", len(ids), err)
	}
	out := make(map[uuid.UUID]ParkedStatus, len(ss))
	for _, s := range ss {
		out[s.ID] = s
	}
	return out, nil
}

// Cancel cancels a parked entry for the reason as
// POST /v1/parked-journal-entries/{id}:cancel does, as the request's
// principal: locked, refused unless it is parked
// (ledger.invalid_parked_status) and unless the caller may act on it (the
// owner rule, ledger.not_entry_owner: a person cancelling an agent's entry
// no person has taken on becomes its owner), audited and published as
// ParkedJournalEntryChanged. The caller answers for the version: an
// accrual proposal's rejection checks its own (ADR-0058 §8).
func (ParkedEntries) Cancel(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := cancelEntry(ctx, id, func(int32) error { return nil }, reason)
	return err
}

// ProfitCenter is the profit center a manual entry's line on the account
// with the centers given takes when it is posted in the legal entity on
// date, in fiscal year year: the one given, its cost center's, the
// account's default cost center's, or the company's default profit
// center, as a posting derives and checks it (assign, ADR-0050 §5); nil
// for none. An accrual's credit line takes the expense line's (ADR-0058
// §8). Refusals are the posting's, of line 1.
func (CloseAccounts) ProfitCenter(ctx context.Context, legalEntity uuid.UUID, year int16, date time.Time, account uuid.UUID,
	costCenter, profitCenter *uuid.UUID) (*uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	lines := []Line{{AccountID: account, CostCenterID: costCenter, ProfitCenterID: profitCenter}}
	if err := assign(ctx, tx, legalEntity, year, date, lines, false); err != nil {
		return nil, err
	}
	return lines[0].ProfitCenterID, nil
}

// CloseAccount is an account as an accrual proposal reads it (T64e): its
// number and name, whether it is a balance sheet account, and whether it
// is postable by hand in the company the read is for: active, set up and
// not blocked there, no reconciliation account, not its GR/IR account
// and no secondary cost element (Accounts.IsPostable's rule).
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
				AND a.cost_element <> 'secondary' AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $1
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
