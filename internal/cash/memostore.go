package cash

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	problem.RegisterConstraint("memo_record_override_once", func() *problem.Problem {
		return problem.Conflict("cash.occurrence_overridden", "The occurrence already has an override; change, realise or cancel that record instead.")
	})
	problem.RegisterConstraint("memo_record_batch_reference", func() *problem.Problem {
		return problem.Conflict("cash.memo_upload_concurrent", "A record of the batch key already has this reference; upload the batch again.")
	})
	for _, c := range []string{"memo_record_item_active", "memo_record_item_direction"} {
		problem.RegisterConstraint(c, func() *problem.Problem {
			return invalid("cash.liquidity_item_invalid", "The liquidity item is not active, or not for the amount's direction.", "liquidity_item",
				"an active liquidity item of the amount's direction")
		})
	}
}

// memoSelect reads memo records with their currency's minor-unit digits.
const memoSelect = `SELECT m.id, m.legal_entity_id, m.house_bank_id, m.currency, c.exponent, m.amount, m.value_date, m.expires_on, m.counts_until,
		m.certainty, m.liquidity_item, m.description, m.reference, m.recurrence, m.recurrence_until, m.recurrence_count, m.recurrence_of,
		m.occurrence_date, m.status, m.origin, m.session_id, m.reason, m.evidence, m.batch_key, m.realised_statement_id, m.realised_line_no,
		m.decided_by, m.decided_at, m.comment, m.version, m.created_at, m.created_by, m.updated_at, m.updated_by
	FROM cash.memo_record m JOIN ref.currency c ON c.code = m.currency`

func scanMemo(r pgx.Row) (MemoRecord, error) {
	var m MemoRecord
	var frequency *string
	var until *time.Time
	var count *int32
	var statement *uuid.UUID
	var line *int32
	err := r.Scan(&m.ID, &m.LegalEntityID, &m.HouseBankID, &m.Currency, &m.Exponent, &m.Amount, &m.ValueDate, &m.ExpiresOn, &m.CountsUntil,
		&m.Certainty, &m.Item, &m.Description, &m.Reference, &frequency, &until, &count, &m.RecurrenceOf,
		&m.OccurrenceDate, &m.Status, &m.Origin, &m.SessionID, &m.Reason, &m.Evidence, &m.BatchKey, &statement, &line,
		&m.DecidedBy, &m.DecidedAt, &m.Comment, &m.Version, &m.CreatedAt, &m.CreatedBy, &m.UpdatedAt, &m.UpdatedBy)
	if frequency != nil {
		m.Recurrence = &Recurrence{Frequency: *frequency, Until: until, Count: count}
	}
	if statement != nil && line != nil {
		m.RealisedBy = &LineRef{StatementID: *statement, LineNo: *line}
	}
	return m, errs.Wrapf(err, "scan memo record")
}

// memo reads a memo record of a legal entity the caller may act in, locked
// for the change when forUpdate; any other is not found.
func memo(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (MemoRecord, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return MemoRecord{}, err
	}
	q := memoSelect + ` WHERE m.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF m`
	}
	m, err := scanMemo(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(m.LegalEntityID)):
		return MemoRecord{}, problem.NotFoundOf("memo record")
	case err != nil:
		return MemoRecord{}, fmt.Errorf("cash: memo record %s: %w", id, err)
	}
	return m, nil
}

// memoMeta is what a new record is besides its values: its status and
// origin, an agent's session, reason and evidence, an upload's batch, and
// a decision made with it.
type memoMeta struct {
	status, origin string
	session        *uuid.UUID
	reason         *string
	evidence       []LineRef
	batchKey       *string
	realisedBy     *LineRef
	decided        bool
	comment        *string
}

// insertMemo writes a new record and returns its id.
func insertMemo(ctx context.Context, tx pgx.Tx, p reqctx.Principal, legalEntity uuid.UUID, v memoValues, meta memoMeta) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	var statement *uuid.UUID
	var line *int32
	if meta.realisedBy != nil {
		statement, line = &meta.realisedBy.StatementID, &meta.realisedBy.LineNo
	}
	frequency, until, count := v.recurrenceColumns()
	evidence := meta.evidence
	if evidence == nil {
		evidence = []LineRef{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO cash.memo_record (tenant_id, id, legal_entity_id, house_bank_id, currency, amount, value_date, expires_on,
			counts_until, certainty, liquidity_item, description, reference, recurrence, recurrence_until, recurrence_count, recurrence_of, occurrence_date,
			status, origin, session_id, reason, evidence, batch_key, realised_statement_id, realised_line_no, decided_by, decided_at, comment,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
			CASE WHEN $27 THEN $28::uuid END, CASE WHEN $27 THEN now() END, $29, $28, $28)`,
		p.TenantID, id, legalEntity, v.houseBank, v.currency, v.amount, v.valueDate, v.expiresOn, v.countsUntil, v.certainty, v.item, v.description,
		v.reference, frequency, until, count, v.recurrenceOf, v.occurrenceDate, meta.status, meta.origin, meta.session, meta.reason, evidence,
		meta.batchKey, statement, line, meta.decided, p.ID, meta.comment)
	return id, errs.Wrapf(err, "cash: insert memo record of %s", legalEntity)
}

// recurrenceColumns are a series' rule as stored, nil for a one-off record.
func (v memoValues) recurrenceColumns() (*string, *time.Time, *int32) {
	if v.recurrence == nil {
		return nil, nil, nil
	}
	return &v.recurrence.Frequency, v.recurrence.Until, v.recurrence.Count
}

// updateValues writes a record's new values; reopen makes a cancelled
// record active again, its decision undone (an upload's row again).
func updateValues(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, v memoValues, reopen bool) error {
	frequency, until, count := v.recurrenceColumns()
	_, err := tx.Exec(ctx, `UPDATE cash.memo_record SET house_bank_id = $2, currency = $3, amount = $4, value_date = $5, expires_on = $6, counts_until = $7,
			certainty = $8, liquidity_item = $9, description = $10, reference = $11, recurrence = $12, recurrence_until = $13, recurrence_count = $14,
			status = CASE WHEN $16 THEN 'active' ELSE status END, comment = CASE WHEN $16 THEN NULL ELSE comment END,
			decided_by = CASE WHEN $16 THEN NULL ELSE decided_by END, decided_at = CASE WHEN $16 THEN NULL ELSE decided_at END,
			version = version + 1, updated_by = $15
		WHERE id = $1`, id, v.houseBank, v.currency, v.amount, v.valueDate, v.expiresOn, v.countsUntil, v.certainty, v.item, v.description, v.reference,
		frequency, until, count, p.ID, reopen)
	return errs.Wrapf(err, "cash: update memo record %s", id)
}

// decide sets a record's status by a person's decision, with the person's
// comment and the statement line that realises it, if any.
func decide(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, status string, comment *string, line *LineRef) error {
	var statement *uuid.UUID
	var no *int32
	if line != nil {
		statement, no = &line.StatementID, &line.LineNo
	}
	_, err := tx.Exec(ctx, `UPDATE cash.memo_record SET status = $2, comment = $3, realised_statement_id = $4, realised_line_no = $5, decided_by = $6,
			decided_at = now(), version = version + 1, updated_by = $6
		WHERE id = $1`, id, status, comment, statement, no, p.ID)
	return errs.Wrapf(err, "cash: %s memo record %s", status, id)
}

// record audits a change of a memo record and publishes it, as the record
// is after it.
func record(ctx context.Context, m MemoRecord, action, change string) error {
	detail := map[string]any{"legal_entity_id": m.LegalEntityID, "amount": int64(m.Amount), "currency": m.Currency, "value_date": m.ValueDate.Format(time.DateOnly),
		"liquidity_item": m.Item, "certainty": m.Certainty, "status": m.Status, "origin": m.Origin}
	if m.BatchKey != nil {
		detail["batch_key"] = *m.BatchKey
	}
	if m.Comment != nil {
		detail["comment"] = *m.Comment
	}
	if m.Reason != nil && change == "proposed" {
		detail["reason"] = *m.Reason
	}
	return audit.Record(ctx, "cash.memo_record."+action, "cash.memo_record", m.ID, detail,
		outbox.Event{Context: "cash", Name: "memo_record_changed", AggregateType: "cash.memo_record", AggregateID: m.ID,
			Payload: map[string]any{"memo_record_id": m.ID, "legal_entity_id": m.LegalEntityID, "change": change, "status": m.Status, "version": m.Version}})
}
