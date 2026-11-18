package controlling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A settlement reversed (ADR-0064 §3, T60i). The order's latest
// settlement that is not reversed has its entry reversed through the
// ledger's posting port on the entry's own date, in the settled period:
// per account the order is debited and the receiver credited again, so
// the order's balance is what it was before the settlement. The reversal
// is kept beside the settlement, which then no longer bounds the period
// of the next settlement (notBeforeLastSettlement).
//
// Locks: the settlement's entry lock first (ledger.Postings.LockEntries),
// then the order FOR UPDATE, then what the ledger's reversal takes (the
// entry lock again, the period statuses FOR SHARE, the order FOR SHARE,
// the number range). The ledger's own reversal of an entry naming the
// order takes the entry's lock and then the order FOR SHARE, so the two
// take them in one order; a settlement takes the order FOR UPDATE and no
// entry lock, and a posting the order FOR SHARE and no entry lock.

func settlementNotFound() error { return problem.NotFoundOf("settlement of the internal order") }

// ReverseInternalOrderSettlement implements POST
// /v1/internal-orders/{id}/settlements/{settlement_id}:reverse.
func (e *Endpoints) ReverseInternalOrderSettlement(ctx context.Context, params gen.ReverseInternalOrderSettlementParams) (gen.ReverseInternalOrderSettlementRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	// The settlement row never changes: read before any lock, for the
	// entry whose lock comes first.
	s, err := scanReversedSettlement(tx.QueryRow(ctx, reversedSettlementSQL+` WHERE s.id = $1 AND s.internal_order_id = $2`, params.SettlementID, params.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, settlementNotFound()
	}
	if err != nil {
		return nil, fmt.Errorf("controlling: read settlement %s: %w", params.SettlementID, err)
	}
	if err := e.Postings.LockEntries(ctx, []uuid.UUID{s.EntryID}); err != nil {
		return nil, fmt.Errorf("controlling: lock the entry of settlement %s: %w", s.ID, err)
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, true, orderNotFound)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(o.Version); err != nil {
		return nil, err
	}
	if err := released(o); err != nil {
		return nil, err
	}
	if err := reversible(ctx, tx, s); err != nil {
		return nil, err
	}
	text := "Reversal of the settlement of internal order " + o.Code
	rev, err := e.Postings.Reverse(ctx, s.EntryID, nil, &text)
	if err != nil {
		return nil, fmt.Errorf("controlling: reverse the entry of settlement %s: %w", s.ID, err)
	}
	r := settlementReversal{EntryID: rev.ID, DocumentNumber: rev.DocumentNumber, CreatedBy: p.ID}
	if err := tx.QueryRow(ctx, `INSERT INTO controlling.order_settlement_reversal (tenant_id, settlement_id, journal_entry_id, document_number, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, p.TenantID, s.ID, r.EntryID, r.DocumentNumber, p.ID).Scan(&r.CreatedAt); err != nil {
		return nil, fmt.Errorf("controlling: keep the reversal of settlement %s: %w", s.ID, err)
	}
	s.Reversal = &r
	refs, err := e.Ledger.CostCenterRefs(ctx, []uuid.UUID{s.ReceiverID})
	if err != nil {
		return nil, fmt.Errorf("controlling: receiver of settlement %s: %w", s.ID, err)
	}
	s.ReceiverCode = refs[s.ReceiverID].Code
	if err := recordReversal(ctx, o, le.LocalCurrency, exp, s); err != nil {
		return nil, err
	}
	return &gen.InternalOrderSettlementHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: settlementToAPI(le.LocalCurrency, exp)(s)}, nil
}

// reversible refuses a settlement reversed already, or one that is not
// the order's latest not reversed: settlements are reversed from the
// latest back (SAP likewise, from memory), which keeps the periods of
// the ones left in force in the order they were settled. Read under the
// order's lock, which every settlement and reversal holds, so a reversal
// that committed while this one waited is seen.
func reversible(ctx context.Context, tx pgx.Tx, s settlement) error {
	var reversed bool
	var latest *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM controlling.order_settlement_reversal WHERE settlement_id = $2),
		(SELECT s.id FROM controlling.order_settlement s WHERE s.internal_order_id = $1
			AND NOT EXISTS (SELECT FROM controlling.order_settlement_reversal r WHERE r.tenant_id = s.tenant_id AND r.settlement_id = s.id)
		ORDER BY s.created_at DESC, s.id DESC LIMIT 1)`, s.OrderID, s.ID).Scan(&reversed, &latest); err != nil {
		return fmt.Errorf("controlling: latest settlement of internal order %s: %w", s.OrderID, err)
	}
	if reversed {
		return problem.Conflict("controlling.settlement_reversed", "The settlement is reversed already.")
	}
	if *latest != s.ID { // s is not reversed, so the order has a latest
		return problem.Conflict("controlling.settlement_not_latest", "A later settlement of the internal order is in force; reverse it first.")
	}
	return nil
}

// recordReversal audits a settlement's reversal and publishes it.
func recordReversal(ctx context.Context, o Order, currency string, exp int, s settlement) error {
	day := s.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"legal_entity_id": o.LegalEntityID, "code": o.Code, "settlement_id": s.ID, "fiscal_year": s.FiscalYear,
		"fiscal_period": s.FiscalPeriod, "posting_date": day, "amount": s.Amount.Plain(exp), "currency": currency, "journal_entry_id": s.EntryID,
		"reversal_journal_entry_id": s.Reversal.EntryID, "reversal_journal_entry_number": s.Reversal.DocumentNumber}
	return audit.Record(ctx, "controlling.internal_order.reverse_settlement", "controlling.internal_order", o.ID, detail,
		outbox.Event{Context: "controlling", Name: "internal_order_settlement_reversed", AggregateType: "controlling.internal_order", AggregateID: o.ID,
			Payload: map[string]any{"internal_order_id": o.ID, "legal_entity_id": o.LegalEntityID, "code": o.Code, "name": o.Name, "settlement_id": s.ID,
				"fiscal_year": s.FiscalYear, "fiscal_period": s.FiscalPeriod, "posting_date": day, "receiver_cost_center_id": s.ReceiverID,
				"receiver_cost_center_code": s.ReceiverCode, "amount": money.EventAmount(s.Amount, currency, exp), "journal_entry_id": s.EntryID,
				"reversal_journal_entry_id": s.Reversal.EntryID, "reversal_journal_entry_number": s.Reversal.DocumentNumber}})
}
