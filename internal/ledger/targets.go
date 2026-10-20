package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// ledgerChoice is the ledgers a manual entry posts to (ADR-0045): the
// ledger it names alone, the members of the ledger group it names, or,
// naming neither, every ledger its company keeps.
type ledgerChoice struct {
	// ledger is the ledger named, the group's representative, or the
	// leading ledger: the one whose entry stands for the posting.
	ledger uuid.UUID
	// ledgers are the ledgers posted to, ledger first; nil: every ledger
	// of the company.
	ledgers  []uuid.UUID
	group    *uuid.UUID
	specific bool
}

// chooseLedgers resolves the ledgers a manual entry of the legal entity le
// names, each of them one the company keeps.
func chooseLedgers(ctx context.Context, tx pgx.Tx, le uuid.UUID, in NewEntry) (ledgerChoice, error) {
	switch {
	case in.LedgerID != nil && in.LedgerGroup != nil:
		return ledgerChoice{}, problem.Unprocessable("ledger.ledger_or_group", "An entry names a ledger or a ledger group, not both.",
			problem.FieldError{Field: "ledger_group", Code: "conflict", Message: "a ledger group, or ledger_id, not both"})
	case in.LedgerGroup != nil:
		return groupLedgers(ctx, tx, le, *in.LedgerGroup)
	}
	ledger, err := ledgerID(ctx, tx, in.LedgerID)
	if err != nil || in.LedgerID == nil {
		return ledgerChoice{ledger: ledger}, err
	}
	var kept bool
	if err := tx.QueryRow(ctx, `SELECT ledger.company_keeps($1, $2)`, le, ledger).Scan(&kept); err != nil {
		return ledgerChoice{}, fmt.Errorf("ledger: ledgers of %s: %w", le, err)
	}
	if !kept {
		return ledgerChoice{}, notKept("ledger_id")
	}
	return ledgerChoice{ledger: ledger, ledgers: []uuid.UUID{ledger}, specific: true}, nil
}

// groupLedgers resolves a ledger group by its code: its members, the
// representative first, every one a ledger the company le keeps.
func groupLedgers(ctx context.Context, tx pgx.Tx, le uuid.UUID, code string) (ledgerChoice, error) {
	var c ledgerChoice
	var group uuid.UUID
	var kept bool
	err := tx.QueryRow(ctx, `SELECT g.id, array_agg(m.ledger_id ORDER BY m.ledger_id <> g.representative_ledger_id, l.code),
			bool_and(ledger.company_keeps($2, m.ledger_id))
		FROM ledger.ledger_group g
		JOIN ledger.ledger_group_member m ON m.tenant_id = g.tenant_id AND m.group_id = g.id
		JOIN ledger.ledger l ON l.tenant_id = m.tenant_id AND l.id = m.ledger_id
		WHERE g.code = $1 GROUP BY g.id`, code, le).Scan(&group, &c.ledgers, &kept)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return c, problem.Unprocessable("ledger.unknown_ledger_group", "No ledger group has the code "+code+".",
			problem.FieldError{Field: "ledger_group", Code: "not_found", Message: "a ledger group of the tenant"})
	case err != nil:
		return c, fmt.Errorf("ledger: ledger group %s: %w", code, err)
	case !kept:
		return c, notKept("ledger_group")
	}
	c.ledger, c.group = c.ledgers[0], &group
	return c, nil
}

// notKept refuses a posting to a ledger its company does not keep; field
// names the request field that chose it.
func notKept(field string) *problem.Problem {
	return problem.Unprocessable("ledger.ledger_not_kept", "The legal entity does not keep the ledger; a company posts only to the ledgers the configuration assigns it.",
		problem.FieldError{Field: field, Code: "not_kept", Message: "a ledger the legal entity keeps"})
}

// groupCopies are the ledgers of orig's posting group, its own first, and
// the copies of orig in the others by ledger: what reversing orig reverses.
// An entry posted before posting groups stands alone. A copy is reversed
// with its first entry, not by itself.
func groupCopies(ctx context.Context, tx pgx.Tx, orig Entry) ([]uuid.UUID, map[uuid.UUID]uuid.UUID, error) {
	switch {
	case orig.PostingGroupID == nil:
		return []uuid.UUID{orig.LedgerID}, nil, nil
	case *orig.PostingGroupID != orig.ID:
		return nil, nil, problem.Conflict("ledger.entry_is_copy", "The journal entry is a copy of entry "+orig.PostingGroupID.String()+
			" in another ledger; reverse that entry, which reverses every copy.")
	}
	rows, _ := tx.Query(ctx, `SELECT id, ledger_id FROM ledger.journal_entry WHERE fiscal_year = $1 AND posting_group_id = $2 AND id <> $2 ORDER BY id`,
		orig.FiscalYear, orig.ID)
	ledgers, copies := []uuid.UUID{orig.LedgerID}, map[uuid.UUID]uuid.UUID{}
	var id, ledger uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&id, &ledger}, func() error {
		ledgers, copies[ledger] = append(ledgers, ledger), id
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("ledger: copies of entry %s: %w", orig.ID, err)
	}
	return ledgers, copies, nil
}
