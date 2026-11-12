package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ConfirmRun confirms a proposed dunning run once (ADR-0065 §8, T76b):
// each of its items' level and dunning date, the run's key date, is
// recorded in collections.dunned_item, the run becomes confirmed, and its
// audit record, DunningRunChanged and one DunningNoticeIssued per
// customer are written, all in the request's transaction.
//
// The run moves by an update guarded by its status (ADR-0008): a second
// confirmation finds no proposed run and is refused, 409
// collections.invalid_status, so it records and publishes nothing. The
// confirmations of one company wait for each other, and each compares its
// proposal with the ledger's open items and the recorded levels again: a
// proposal with an item cleared since, or with an item another confirmed
// run has dunned since (at a higher level or at the one it had), is stale
// and refused, 409
// collections.proposal_stale, and nothing of it is recorded.
func (e *Endpoints) ConfirmRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	run, err := GetRun(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if err := ifMatch(run.Version); err != nil {
		return Run{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, run.LegalEntityID) // a stored run's legal entity exists
	if err != nil {
		return Run{}, fmt.Errorf("collections: company of dunning run %s: %w", id, err)
	}
	if err := markConfirmed(ctx, tx, p, run); err != nil {
		return Run{}, err
	}
	notices, err := proposal(ctx, tx, id)
	if err != nil {
		return Run{}, err
	}
	if err := e.fresh(ctx, tx, run, notices); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunned_item (tenant_id, fiscal_year, journal_entry_id, line_no, dunning_run_id, level, dunning_date)
		SELECT $1, i.fiscal_year, i.journal_entry_id, i.line_no, i.dunning_run_id, i.level, $3
		FROM collections.dunning_notice_item i WHERE i.dunning_run_id = $2`, p.TenantID, id, run.KeyDate); err != nil {
		return Run{}, fmt.Errorf("collections: record the levels of dunning run %s: %w", id, err)
	}
	confirmed, err := load(ctx, tx, id)
	if err != nil {
		return Run{}, err
	}
	return confirmed, record(ctx, "collections.dunning_run.confirm", confirmed, le, Proposed, issued(confirmed, le, notices)...)
}

// markConfirmed moves the run from proposed to confirmed, guarded by its
// status: a run that is not proposed, or no longer, is refused. It first
// takes the company's lock, held to the end of the transaction, so that
// of two proposals of a company confirmed at once the second reads what
// the first recorded and is found stale where they share an item.
func markConfirmed(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run Run) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('collections.dunning:' || $1::text, 0))`, run.LegalEntityID); err != nil {
		return fmt.Errorf("collections: lock the dunning of company %s: %w", run.LegalEntityID, err)
	}
	tag, err := tx.Exec(ctx, `UPDATE collections.dunning_run SET status = 'confirmed', version = version + 1, updated_by = $2
		WHERE id = $1 AND status = 'proposed'`, run.ID, p.ID)
	if err != nil {
		return fmt.Errorf("collections: mark dunning run %s confirmed: %w", run.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return problem.Conflict("collections.invalid_status", "The dunning run is not proposed any more; only a proposed run is confirmed.")
	}
	return nil
}

// proposal reads what the run proposed: its notices by customer number,
// the order its events are published in, each with its items.
func proposal(ctx context.Context, tx pgx.Tx, run uuid.UUID) ([]Notice, error) {
	rows, _ := tx.Query(ctx, noticeSelect+` WHERE dunning_run_id = $1 ORDER BY customer_number, id`, run)
	notices, err := pgx.CollectRows(rows, scanNotice)
	if err != nil {
		return nil, fmt.Errorf("collections: read the notices of dunning run %s: %w", run, err)
	}
	return notices, items(ctx, tx, notices)
}

// fresh compares the proposal with the ledger's open items and what is
// recorded of them now (ADR-0065 §8): an item that is no longer open was
// cleared since the run proposed it, and one dunned more often than when
// the run proposed it was dunned by another confirmed run since. Either
// makes the proposal stale. The times an item was dunned are compared,
// not its level: a confirmation leaves the level of an item at the
// highest level, or short of the next level's days, as it was.
func (e *Endpoints) fresh(ctx context.Context, tx pgx.Tx, run Run, notices []Notice) error {
	if len(notices) == 0 {
		return nil // nothing was proposed; and the reader, given no customer, reads every customer's items
	}
	customers := make([]uuid.UUID, len(notices))
	var refs []ledger.ItemRef
	for i, n := range notices {
		customers[i] = n.CustomerID
		for _, it := range n.Items {
			refs = append(refs, it.ItemRef)
		}
	}
	open, err := e.OpenItems.Receivable(ctx, run.LegalEntityID, customers)
	if err != nil {
		return fmt.Errorf("collections: open customer items of dunning run %s: %w", run.ID, err)
	}
	isOpen := make(map[ledger.ItemRef]bool, len(open))
	for _, it := range open {
		isOpen[it.ItemRef] = true
	}
	levels, err := recorded(ctx, tx, refs)
	if err != nil {
		return err
	}
	var cleared, moved int
	for _, n := range notices {
		for _, it := range n.Items {
			switch {
			case !isOpen[it.ItemRef]:
				cleared++
			case levels[it.ItemRef].Times != it.PreviousDunnings:
				moved++
			}
		}
	}
	if cleared+moved == 0 {
		return nil
	}
	return problem.Conflict("collections.proposal_stale", fmt.Sprintf("The proposal is stale: of its %d items, %d cleared and %d dunned by another run "+
		"since it was started. Start a new run.", len(refs), cleared, moved))
}

// issued are the events of a confirmed run's notices, DunningNoticeIssued
// one per customer in the notices' order. Each names its customer by id
// and number, never by name (product owner, 2026-11-05), and carries the
// notice's totals, not its items.
func issued(r Run, le tenancy.Entity, notices []Notice) []outbox.Event {
	events := make([]outbox.Event, len(notices))
	for i, n := range notices {
		events[i] = outbox.Event{Context: "collections", Name: "dunning_notice_issued", AggregateType: "collections.dunning_notice", AggregateID: n.ID,
			Payload: map[string]any{"dunning_notice_id": n.ID, "dunning_run_id": r.ID, "legal_entity_id": r.LegalEntityID, "legal_entity_code": le.Code,
				"customer_id": n.CustomerID, "customer_number": n.CustomerNumber, "dunning_date": r.KeyDate.Format(time.DateOnly), "level": n.Level,
				"item_count": n.ItemCount, "overdue_amount": money.EventAmount(n.OverdueAmount, r.Currency, r.Exponent)}}
	}
	return events
}
