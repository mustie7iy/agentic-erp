package collections

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// CancelRun cancels a confirmed dunning run once (T76i, ADR-0065 As
// built): the run becomes cancelled with the reason, the levels its
// confirmation recorded no longer count (recorded reads confirmed runs
// alone), so its items are back at the levels of the runs before it, and
// each debit memo it posted its notices' charges and interest with
// (T76h) is reversed through receivables, on the memo's posting date.
// Its audit record, DunningRunChanged and one DunningNoticeCancelled per
// notice are written, all in the request's transaction.
//
// It takes the company's dunning lock, as a confirmation does, and moves
// the run by an update guarded by its status (ADR-0008): a second
// cancellation, or one of a run not confirmed, is refused, 409
// collections.invalid_status. Under the lock it refuses, 409
// collections.run_superseded, a run whose items or memos a later
// confirmed run has dunned: that run's levels were reached from this
// one's, so it is cancelled first. What receivables or the ledger
// refuses refuses the cancellation, nothing of it done: a memo whose item
// is cleared (receivables.document_cleared, or ledger.entry_cleared when
// the clearing committed meanwhile), a posting date in a period not open
// (ledger.period_not_open).
func (e *Endpoints) CancelRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	// Read without a lock, as a confirmation reads: the update that
	// cancels is guarded by the run's status.
	run, err := GetRun(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if err := ifMatch(run.Version); err != nil {
		return Run{}, err
	}
	if err := problem.RequireText("reason", reason); err != nil {
		return Run{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, run.LegalEntityID) // a stored run's legal entity exists
	if err != nil {
		return Run{}, fmt.Errorf("collections: company of dunning run %s: %w", id, err)
	}
	if err := markCancelled(ctx, tx, p, run, reason); err != nil {
		return Run{}, err
	}
	if err := superseded(ctx, tx, run); err != nil {
		return Run{}, err
	}
	notices, err := proposal(ctx, tx, id)
	if err != nil {
		return Run{}, err
	}
	if err := charged(ctx, tx, notices); err != nil {
		return Run{}, err
	}
	if err := e.reverseCharges(ctx, tx, notices, reason); err != nil {
		return Run{}, err
	}
	cancelled, err := load(ctx, tx, id)
	if err != nil {
		return Run{}, err
	}
	return cancelled, record(ctx, "collections.dunning_run.cancel", cancelled, le, Confirmed, withdrawn(cancelled, le, notices)...)
}

// markCancelled moves the run from confirmed to cancelled with the
// reason, guarded by its status, under the company's dunning lock.
func markCancelled(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run Run, reason string) error {
	if err := lockDunning(ctx, tx, run.LegalEntityID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE collections.dunning_run SET status = 'cancelled', cancellation_reason = $3, version = version + 1, updated_by = $2
		WHERE id = $1 AND status = 'confirmed'`, run.ID, p.ID, reason)
	if err != nil {
		return fmt.Errorf("collections: mark dunning run %s cancelled: %w", run.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return problem.Conflict("collections.invalid_status", "The dunning run is not confirmed, or not any more; only a confirmed run is cancelled.")
	}
	return nil
}

// superseded refuses the cancellation when a later confirmed run dunned
// one of the run's items, proposing it from the run's recorded level (its
// previous dunnings above the run's: confirmations of a company wait for
// each other, and each compares the times an item was dunned), or the
// customer item of a memo the run posted. The latest such run is named,
// the one to cancel first.
func superseded(ctx context.Context, tx pgx.Tx, run Run) error {
	var later uuid.UUID
	var key time.Time
	err := tx.QueryRow(ctx, `SELECT r.id, r.key_date FROM collections.dunning_run r
		WHERE r.status = 'confirmed' AND r.legal_entity_id = $2 AND r.id IN (
			SELECT d.dunning_run_id FROM collections.dunning_notice_item i
			JOIN collections.dunned_item d ON d.fiscal_year = i.fiscal_year AND d.journal_entry_id = i.journal_entry_id AND d.line_no = i.line_no
			JOIN collections.dunning_notice_item j ON j.dunning_run_id = d.dunning_run_id AND j.fiscal_year = d.fiscal_year
				AND j.journal_entry_id = d.journal_entry_id AND j.line_no = d.line_no
			WHERE i.dunning_run_id = $1 AND d.dunning_run_id <> $1 AND j.previous_dunnings > i.previous_dunnings
			UNION
			SELECT d.dunning_run_id FROM collections.dunning_charge c
			JOIN collections.dunned_item d ON d.fiscal_year = c.journal_entry_year AND d.journal_entry_id = c.journal_entry_id
			WHERE c.dunning_run_id = $1)
		ORDER BY r.key_date DESC, r.id DESC LIMIT 1`, run.ID, run.LegalEntityID).Scan(&later, &key)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("collections: later runs of dunning run %s: %w", run.ID, err)
	}
	return problem.Conflict("collections.run_superseded", fmt.Sprintf("A later confirmed dunning run, %s at key date %s, dunned items of this run "+
		"from the levels it recorded, or the debit memos it posted; cancel that run first.", later, key.Format(time.DateOnly)))
}

// reverseCharges reverses the debit memos the run's notices were charged
// with, in the notices' order, each on its own posting date with the
// cancellation's reason, and records each reversal on its charge, once
// (the update guarded by a charge not yet reversed).
func (e *Endpoints) reverseCharges(ctx context.Context, tx pgx.Tx, notices []Notice, reason string) error {
	for i, n := range notices {
		c := n.Charge
		if c == nil {
			continue
		}
		v, err := e.Invoices.ReverseSourced(ctx, c.InvoiceID, n.ID, receivables.Reversal{Reason: reason})
		if err != nil {
			return problem.Led(fmt.Errorf("collections: reverse debit memo %s of dunning notice %s: %w", c.InvoiceNumber, n.ID, err),
				"Debit memo "+c.InvoiceNumber+" of customer "+n.CustomerNumber+" cannot be reversed: ")
		}
		r := v.Reversal // a reversed memo names its reversal (customer_invoice_reversed)
		rev := ChargeReversal{EntryYear: r.EntryYear, EntryID: r.EntryID, Date: r.PostingDate}
		if _, err := tx.Exec(ctx, `UPDATE collections.dunning_charge SET reversal_entry_year = $2, reversal_entry_id = $3, reversal_date = $4
			WHERE dunning_notice_id = $1 AND reversal_entry_id IS NULL`, n.ID, rev.EntryYear, rev.EntryID, rev.Date); err != nil {
			return fmt.Errorf("collections: record the reversal of debit memo %s: %w", c.InvoiceNumber, err)
		}
		notices[i].Charge.Reversal = &rev
	}
	return nil
}

// withdrawn are the events of a cancelled run's notices,
// DunningNoticeCancelled one per customer in the notices' order: each
// names its customer by id and number, never by name, its level, and the
// debit memo its charge was posted with and the memo's reversal, null for
// none.
func withdrawn(r Run, le tenancy.Entity, notices []Notice) []outbox.Event {
	events := make([]outbox.Event, len(notices))
	for i, n := range notices {
		payload := map[string]any{"dunning_notice_id": n.ID, "dunning_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "legal_entity_code": le.Code,
			"customer_id": n.CustomerID, "customer_number": n.CustomerNumber, "dunning_date": r.KeyDate.Format(time.DateOnly), "level": n.Level,
			"customer_invoice_id": nil, "customer_invoice_number": nil, "reversal_entry_id": nil, "reversal_date": nil}
		if c := n.Charge; c != nil && c.Reversal != nil {
			payload["customer_invoice_id"], payload["customer_invoice_number"] = c.InvoiceID, c.InvoiceNumber
			payload["reversal_entry_id"], payload["reversal_date"] = c.Reversal.EntryID, c.Reversal.Date.Format(time.DateOnly)
		}
		events[i] = outbox.Event{Context: "collections", Name: "dunning_notice_cancelled", AggregateType: "collections.dunning_notice", AggregateID: n.ID,
			Payload: payload}
	}
	return events
}
