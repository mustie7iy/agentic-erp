package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CloseChecks is the ledger's port for a financial close's checks
// (ADR-0058 §4, §6, T64c): the parked entries a period still holds, and
// whether a ledger's foreign-currency revaluation of a key date is posted.
type CloseChecks struct{}

// OpenParked is a parked entry a close's check names: what identifies it
// to a person, and who parked it.
type OpenParked struct {
	ID            uuid.UUID
	DocumentType  string
	PostingDate   time.Time
	Status        string
	Reference     *string
	HeaderText    *string
	CreatedBy     uuid.UUID
	CreatedByKind string
}

// Parked returns how many parked entries of the legal entity with a
// posting date from from to to are in one of the statuses, and the first
// limit of them by posting date and id: none for a legal entity the
// caller may not act in.
func (CloseChecks) Parked(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, statuses []string, limit int) (int, []OpenParked, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return 0, nil, err
	}
	total := 0
	rows, _ := tx.Query(ctx, `SELECT id, document_type, posting_date, status, reference, header_text, created_by, created_by_kind, count(*) OVER ()
		FROM ledger.parked_entry WHERE ($1 OR legal_entity_id = ANY($2)) AND legal_entity_id = $3 AND posting_date BETWEEN $4 AND $5
			AND status = ANY($6)
		ORDER BY posting_date, id LIMIT $7`, access.All, access.IDs, legalEntity, from, to, statuses, limit)
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenParked, error) {
		var p OpenParked
		err := row.Scan(&p.ID, &p.DocumentType, &p.PostingDate, &p.Status, &p.Reference, &p.HeaderText, &p.CreatedBy, &p.CreatedByKind, &total)
		return p, errs.Wrapf(err, "scan parked entry")
	})
	if err != nil {
		return 0, nil, fmt.Errorf("ledger: parked entries of %s from %s to %s: %w", legalEntity, from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}
	return total, found, nil
}

// PostedRevaluation returns the posted revaluation run of the legal
// entity's ledger at the key date (at most one: revaluation_run_posted),
// ok false when there is none or the caller may not act in the legal
// entity.
func (CloseChecks) PostedRevaluation(ctx context.Context, legalEntity, ledgerID uuid.UUID, keyDate time.Time) (run uuid.UUID, ok bool, err error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM ledger.revaluation_run WHERE ($1 OR legal_entity_id = ANY($2)) AND legal_entity_id = $3 AND ledger_id = $4
		AND key_date = $5 AND status = 'posted'`, access.All, access.IDs, legalEntity, ledgerID, keyDate).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("ledger: revaluation of ledger %s of %s at %s: %w", ledgerID, legalEntity, keyDate.Format(time.DateOnly), err)
	}
	return run, true, nil
}
