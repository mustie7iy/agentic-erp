package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A statement in gap stops the posting of its bank's lines until the
// missing statement arrives (ADR-0047 Continuity). A person may instead
// accept the gap with a reason: the statement becomes imported, and the
// bank's lines post again once it has no other statement in gap. Only
// people accept (x-agent: false), and one person decides: the acceptance
// posts nothing, each line still goes through its approval tier, and what
// the missing statement held shows as unexplained in the reconciliation
// until a person posts it.

// AcceptGap accepts the gap of a statement of a legal entity the caller may
// access, whose version passes ifMatch, with the reason why, and returns
// the statement with its lines.
func AcceptGap(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Statement, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return Statement{}, err
	}
	s, tx, err := visibleStatement(ctx, id)
	if err != nil {
		return Statement{}, err
	}
	if err := lockStatement(ctx, tx, s, ifMatch); err != nil {
		return Statement{}, err
	}
	// The status is read again under the lock: an import may have closed
	// the gap since the statement was read.
	var previous uuid.UUID
	var previousClosing money.Minor
	err = tx.QueryRow(ctx, `UPDATE payments.bank_statement s SET status = 'imported', gap_accepted_at = now(), gap_accepted_by = $2, gap_reason = $3,
			version = s.version + 1, updated_by = $2
		FROM payments.bank_statement pr WHERE s.id = $1 AND s.status = 'gap' AND pr.tenant_id = s.tenant_id AND pr.id = s.previous_statement_id
		RETURNING pr.id, pr.closing_balance`, id, p.ID, reason).Scan(&previous, &previousClosing)
	if errors.Is(err, pgx.ErrNoRows) {
		return Statement{}, problem.Conflict("payments.statement_not_in_gap", "The statement is not in gap: it is imported or reconciled.")
	}
	if err != nil {
		return Statement{}, fmt.Errorf("payments: accept the gap of statement %s: %w", id, err)
	}
	payload := map[string]any{"bank_statement_id": id, "house_bank_id": s.HouseBankID, "legal_entity_id": s.LegalEntityID, "previous_statement_id": previous,
		"opening_balance":          map[string]any{"amount": s.Opening.Plain(s.Exponent), "currency": s.Currency},
		"previous_closing_balance": map[string]any{"amount": previousClosing.Plain(s.Exponent), "currency": s.Currency},
		"reason":                   reason, "status": StatementImported}
	if err := record(ctx, "payments.bank_statement.accept_gap", id, map[string]any{"previous_statement_id": previous, "reason": reason,
		"opening_balance": s.Opening.Plain(s.Exponent), "previous_closing_balance": previousClosing.Plain(s.Exponent), "status": StatementImported},
		outbox.Event{Context: "payments", Name: "bank_statement_gap_accepted", AggregateType: "payments.bank_statement", AggregateID: id, Payload: payload}); err != nil {
		return Statement{}, err
	}
	return GetStatement(ctx, id)
}
