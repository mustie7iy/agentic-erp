package controlling

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An allocation run reversed (ADR-0064 As built T60k). The run's entry is
// reversed through the ledger's posting port on the entry's own date, in
// the run's period, in every ledger it was posted to: each receiver gives
// its share back and the sender holds again what the run charged. The
// reversal is kept beside the run, which is then no longer in force: the
// cycle may be run for the period again, as the next attempt
// (controlling.allocation_run_in_force_once), and a sender assessed by
// the run is no longer assessed for the period (senderAssessed).
//
// Locks: every cycle of the run's sender FOR UPDATE (heldCycle), as a run
// takes them, then what the ledger's reversal takes (the entry's lock,
// the period statuses, the centers, the number range), as a run's posting
// takes them after the same cycles. So a reversal and a run of one sender
// follow one another, and the reversal reads whether the run is reversed
// under that lock. No other path holds the entry's lock and then a cycle:
// the ledger's own reversal of a run's entry is refused before it locks
// anything of controlling's (ledger.entry_owned_elsewhere).

// ReverseAllocationRun implements POST /v1/allocation-runs/{id}:reverse,
// at the version of the run's cycle the caller read.
func (e *Endpoints) ReverseAllocationRun(ctx context.Context, params gen.ReverseAllocationRunParams) (gen.ReverseAllocationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// The run names its cycle for good: read before any lock, for the
	// sender's cycles to hold.
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, params.ID))
	if err := authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("allocation run"), "controlling: read allocation run %s", params.ID); err != nil {
		return nil, err
	}
	c, le, exp, err := e.heldCycle(ctx, tx, r.CycleID)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(c.Version); err != nil {
		return nil, err
	}
	if r, err = heldRun(ctx, tx, r.ID); err != nil {
		return nil, err
	}
	text := "Reversal of the run of allocation cycle " + c.Code
	rev, err := e.Postings.Reverse(ctx, r.EntryID, nil, &text)
	if err != nil {
		return nil, fmt.Errorf("controlling: reverse the entry of allocation run %s: %w", r.ID, err)
	}
	v := runReversal{EntryID: rev.ID, DocumentNumber: rev.DocumentNumber, CreatedBy: p.ID}
	if err := tx.QueryRow(ctx, `INSERT INTO controlling.allocation_run_reversal (tenant_id, run_id, journal_entry_id, document_number, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, p.TenantID, r.ID, v.EntryID, v.DocumentNumber, p.ID).Scan(&v.CreatedAt); err != nil {
		return nil, fmt.Errorf("controlling: keep the reversal of allocation run %s: %w", r.ID, err)
	}
	r.Reversal, r.SenderCode = &v, c.SenderCode
	if err := recordRunReversal(ctx, r, le.LocalCurrency, exp); err != nil {
		return nil, err
	}
	return &gen.AllocationRunHeaders{Response: runToAPI(r, le.LocalCurrency, exp)}, nil
}

// heldRun reads the run again under the sender's cycles' lock, which every
// run and reversal of the sender holds, so a reversal that committed while
// this one waited is seen: a run is reversed once (409).
func heldRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (allocationRun, error) {
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return allocationRun{}, fmt.Errorf("controlling: read allocation run %s under its cycles' lock: %w", id, err)
	}
	if r.Reversal != nil {
		return allocationRun{}, problem.Conflict("controlling.allocation_run_reversed", "The allocation run is reversed already: journal entry "+
			r.Reversal.DocumentNumber+".")
	}
	return r, nil
}

// recordRunReversal audits a run's reversal and publishes it.
func recordRunReversal(ctx context.Context, r allocationRun, currency string, exp int) error {
	day := r.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"allocation_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "code": r.CycleCode, "kind": r.Kind, "fiscal_year": r.FiscalYear,
		"fiscal_period": r.FiscalPeriod, "posting_date": day, "amount": r.Amount.Plain(exp), "currency": currency, "journal_entry_id": r.EntryID,
		"reversal_journal_entry_id": r.Reversal.EntryID, "reversal_journal_entry_number": r.Reversal.DocumentNumber}
	return audit.Record(ctx, "controlling.allocation_cycle.reverse_run", "controlling.allocation_cycle", r.CycleID, detail,
		outbox.Event{Context: "controlling", Name: "allocation_run_reversed", AggregateType: "controlling.allocation_cycle", AggregateID: r.CycleID,
			Payload: map[string]any{"allocation_run_id": r.ID, "allocation_cycle_id": r.CycleID, "legal_entity_id": r.LegalEntityID, "code": r.CycleCode,
				"name": r.CycleName, "kind": r.Kind, "fiscal_year": r.FiscalYear, "fiscal_period": r.FiscalPeriod, "posting_date": day,
				"sender_cost_center_id": r.SenderID, "sender_cost_center_code": r.SenderCode, "amount": money.EventAmount(r.Amount, currency, exp),
				"journal_entry_id": r.EntryID, "reversal_journal_entry_id": r.Reversal.EntryID, "reversal_journal_entry_number": r.Reversal.DocumentNumber}})
}
