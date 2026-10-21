package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// What people do with statement lines (ADR-0047, T49e): run the matching
// rules on a statement again, match a line by hand, exclude it with a
// reason, and read the matches. A person's decision replaces the line's
// live match, which becomes rejected; both are audited.

// MatchStatement runs the matching rules on the statement's unmatched
// lines and those a rule left open, and returns the statement with its
// lines.
func (e *Endpoints) MatchStatement(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Statement, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return Statement{}, err
	}
	s, tx, err := visibleStatement(ctx, id)
	if err != nil {
		return Statement{}, err
	}
	if err := e.matchStatement(ctx, tx, p, s, ifMatch); err != nil {
		return Statement{}, err
	}
	return GetStatement(ctx, id)
}

// ManualMatch is a person's match of a line: the payments it clears, the
// posting rule that posts it, or (Incoming) the incoming payments clearing
// account it posts to, and why.
type ManualMatch struct {
	PaymentIDs  []uuid.UUID
	PostingRule string
	Incoming    bool
	Reason      string
}

// MatchLine matches the line by hand, replacing its live match; an
// agent's proposal it replaces is overridden.
func (e *Endpoints) MatchLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, in ManualMatch) (Match, error) {
	if n := len(slices.DeleteFunc([]bool{len(in.PaymentIDs) > 0, in.PostingRule != "", in.Incoming}, func(b bool) bool { return !b })); n != 1 {
		return Match{}, problem.Unprocessable("request.validation_failed", "A match names the payments it clears, a posting rule, or the incoming payments clearing account: one of them.",
			problem.FieldError{Field: "payment_ids", Code: "invalid", Message: "payment_ids, posting_rule or incoming"})
	}
	return e.decideLine(ctx, statement, lineNo, ifMatch, OutcomeOverridden, func(ctx context.Context, d openLine) (newMatch, error) {
		var targets []MatchTarget
		var err error
		if in.Incoming {
			targets, err = incomingTarget(ctx, d.tx, d.s, lineNo)
		} else {
			targets, err = e.manualTargets(ctx, d.tx, d.s, in)
		}
		return newMatch{kind: MatchManual, reason: in.Reason, status: MatchMatched, candidates: [][]MatchTarget{targets}}, err
	})
}

// incomingTarget is a person's match of the line to the house bank's
// incoming payments clearing account (ADR-0047): only a credit that
// reverses nothing is an incoming payment, and only a bank that names the
// account takes one. Accounts receivable (Phase 3) clears the customer's
// items from there.
func incomingTarget(ctx context.Context, tx pgx.Tx, s Statement, lineNo int32) ([]MatchTarget, error) {
	var amount money.Minor
	var reversal bool
	var account *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT l.amount, l.is_reversal, h.incoming_gl_account_id FROM payments.bank_statement_line l
		JOIN payments.house_bank h ON h.tenant_id = l.tenant_id AND h.id = $3 WHERE l.statement_id = $1 AND l.line_no = $2`,
		s.ID, lineNo, s.HouseBankID).Scan(&amount, &reversal, &account); err != nil {
		return nil, fmt.Errorf("payments: line %d of statement %s as an incoming payment: %w", lineNo, s.ID, err)
	}
	field := problem.FieldError{Field: "incoming", Code: "invalid", Message: "an incoming payment of a house bank with incoming_gl_account"}
	if amount <= 0 || reversal {
		return nil, problem.Unprocessable("payments.statement_match_target", "Only an incoming payment, a credit that reverses nothing, "+
			"posts to the incoming payments clearing account.", field)
	}
	if account == nil {
		return nil, problem.Unprocessable("payments.statement_match_target", "The house bank has no incoming payments clearing account (incoming_gl_account).", field)
	}
	return []MatchTarget{{Candidate: 1, Incoming: true}}, nil
}

// ExcludeLine excludes the line from reconciliation, replacing its live
// match: the bank's booking clears nothing the ledger holds.
func (e *Endpoints) ExcludeLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, reason string) (Match, error) {
	return e.decideLine(ctx, statement, lineNo, ifMatch, OutcomeOverridden, func(context.Context, openLine) (newMatch, error) {
		return newMatch{kind: MatchManual, reason: reason, status: MatchExcluded}, nil
	})
}

// openLine is a line of a statement the caller may access, locked for a
// person's decision, with its live match as it was (nil: none).
type openLine struct {
	p      reqctx.Principal
	tx     pgx.Tx
	s      Statement
	lineNo int32
	live   *Match
}

// lockLine opens a line for a person's decision: under the bank's lock,
// once the statement's version passes ifMatch. A line on its way to the
// ledger (T49f) is not changed. Only people decide (the operations are
// x-agent: false); the agent proposes (proposals.go, T49g).
func lockLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error) (openLine, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return openLine{}, err
	}
	s, tx, err := visibleStatement(ctx, statement)
	if err != nil {
		return openLine{}, err
	}
	if err := lockStatement(ctx, tx, s, ifMatch); err != nil {
		return openLine{}, err
	}
	status, live, err := lineState(ctx, tx, statement, lineNo)
	if err != nil {
		return openLine{}, err
	}
	if status == "pending_approval" || status == "posted" {
		return openLine{}, problem.Conflict("payments.statement_line_posting", fmt.Sprintf("Line %d is %s; its match stays.", lineNo, strings.ReplaceAll(status, "_", " ")))
	}
	return openLine{p: p, tx: tx, s: s, lineNo: lineNo, live: live}, nil
}

// lineState reads the line's status, locking it, and its live match. The
// lines of a reconciled statement are decided for good (T49h): it never
// reopens by itself, so none is decided again.
func lineState(ctx context.Context, tx pgx.Tx, statement uuid.UUID, lineNo int32) (string, *Match, error) {
	var status string
	var reconciled bool
	err := tx.QueryRow(ctx, `SELECT l.status, s.status = 'reconciled' FROM payments.bank_statement_line l
		JOIN payments.bank_statement s ON s.tenant_id = l.tenant_id AND s.id = l.statement_id
		WHERE l.statement_id = $1 AND l.line_no = $2 FOR UPDATE OF l`, statement, lineNo).Scan(&status, &reconciled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, problem.NotFoundOf("bank statement line")
	}
	if err != nil {
		return "", nil, fmt.Errorf("payments: line %d of statement %s: %w", lineNo, statement, err)
	}
	if reconciled {
		return "", nil, problem.Conflict("payments.statement_reconciled", fmt.Sprintf("The statement is reconciled; line %d is decided for good.", lineNo))
	}
	rows, _ := tx.Query(ctx, matchSelect+` WHERE m.statement_id = $1 AND m.line_no = $2 AND m.status <> 'rejected'`, statement, lineNo)
	live, err := pgx.CollectRows(rows, scanMatch)
	if err != nil {
		return "", nil, fmt.Errorf("payments: the live match of line %d of statement %s: %w", lineNo, statement, err)
	}
	if len(live) == 0 {
		return status, nil, nil
	}
	return status, &live[0], nil // one live match per line (statement_match_live)
}

// decideLine records a person's decision on a line (lockLine): the line's
// live match is rejected, an agent's proposal with the outcome, and the
// decision recorded.
func (e *Endpoints) decideLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, outcome string,
	decide func(ctx context.Context, d openLine) (newMatch, error)) (Match, error) {
	d, err := lockLine(ctx, statement, lineNo, ifMatch)
	if err != nil {
		return Match{}, err
	}
	if err := rejectLive(ctx, d.tx, d.p, d.s, lineNo, rejection{outcome: outcome}); err != nil {
		return Match{}, err
	}
	m, err := decide(ctx, d)
	if err != nil {
		return Match{}, err
	}
	id, err := writeMatch(ctx, d.tx, d.p, d.s, lineNo, m)
	if err != nil {
		return Match{}, err
	}
	if m.status == MatchExcluded { // an exclusion may be the statement's last line to decide
		if err := e.reconcile(ctx, d.tx, d.p, d.s); err != nil {
			return Match{}, err
		}
	}
	return d.answer(ctx, id)
}

// answer reads the match after a person's decision, with the statement's
// version moved on: the If-Match of the next decision.
func (d openLine) answer(ctx context.Context, id uuid.UUID) (Match, error) {
	version, err := bumpStatement(ctx, d.tx, d.p, d.s.ID)
	if err != nil {
		return Match{}, err
	}
	rows, _ := d.tx.Query(ctx, matchSelect+` WHERE m.id = $1`, id)
	out, err := pgx.CollectExactlyOneRow(rows, scanMatch)
	if err != nil {
		return Match{}, fmt.Errorf("payments: read match %s: %w", id, err)
	}
	out.StatementVersion = version
	return out, nil
}

// rejection is what becomes of an agent's proposal a decision rejects: the
// person's outcome and, when the person turned it down, why. A rule's or
// a person's match has no outcome.
type rejection struct{ outcome, reason string }

// rejectLive rejects the line's live match, if it has one: audited, and
// for an agent's proposal with the outcome, published as
// BankStatementLineProposalDecided.
func rejectLive(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement, lineNo int32, r rejection) error {
	var id uuid.UUID
	var was, kind string
	var session *uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE payments.statement_match n SET status = 'rejected', version = n.version + 1, updated_by = $3,
			outcome = CASE WHEN n.kind = 'agent' THEN nullif($4, '') END
		FROM payments.statement_match o WHERE o.tenant_id = n.tenant_id AND o.id = n.id
			AND n.statement_id = $1 AND n.line_no = $2 AND n.status <> 'rejected' RETURNING n.id, o.status, n.kind, n.session_id`,
		s.ID, lineNo, p.ID, r.outcome).Scan(&id, &was, &kind, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("payments: reject the match of line %d of statement %s: %w", lineNo, s.ID, err)
	}
	detail := map[string]any{"bank_statement_id": s.ID, "line_no": lineNo, "previous_status": was}
	if kind != MatchAgent {
		return record(ctx, "payments.statement_match.reject", id, detail)
	}
	detail["outcome"], detail["reason"] = r.outcome, r.reason
	return record(ctx, "payments.statement_match.reject", id, detail, outbox.Event{Context: "payments", Name: "bank_statement_line_proposal_decided",
		AggregateType: "payments.statement_match", AggregateID: id, Payload: map[string]any{"statement_match_id": id, "bank_statement_id": s.ID,
			"line_no": lineNo, "house_bank_id": s.HouseBankID, "legal_entity_id": s.LegalEntityID, "session_id": session, "outcome": r.outcome}})
}

// manualTargets checks a person's targets: payments the line may clear
// (posted, from the statement's house bank and in its currency, matched
// to no other line), or a posting rule of the company.
func (e *Endpoints) manualTargets(ctx context.Context, tx pgx.Tx, s Statement, in ManualMatch) ([]MatchTarget, error) {
	if in.PostingRule != "" {
		co, err := e.statementCompany(ctx, s.LegalEntityID)
		if err != nil {
			return nil, err
		}
		if co == nil || !slices.ContainsFunc(co.BankStatements.PostingRules, func(r config.StatementPostingRule) bool { return r.TransactionCode == in.PostingRule }) {
			return nil, problem.Unprocessable("payments.statement_match_target", "Posting rule "+in.PostingRule+" is not one of the company's.",
				problem.FieldError{Field: "posting_rule", Code: "unknown", Message: "a transaction code of the company's posting rules"})
		}
		return []MatchTarget{{Candidate: 1, PostingRule: &in.PostingRule}}, nil
	}
	m := matcher{tx: tx, s: s}
	pays, err := m.payments(ctx, "payments matched by hand", `SELECT NULL::uuid, id, end_to_end_id, amount, bank_account_id, taken FROM eligible
		WHERE id = ANY($3) AND NOT taken ORDER BY end_to_end_id`, in.PaymentIDs)
	if err != nil {
		return nil, err
	}
	if len(pays) != len(in.PaymentIDs) {
		return nil, problem.Unprocessable("payments.statement_match_target", "Each payment must be posted, from the statement's house bank, "+
			"in its currency, matched to no other line and named once.",
			problem.FieldError{Field: "payment_ids", Code: "invalid", Message: "payments the line may clear"})
	}
	return targets(1, pays), nil
}

// StatementMatches returns the matches of a statement the caller may
// access, of one line when lineNo is not 0.
func StatementMatches(ctx context.Context, statement uuid.UUID, lineNo int32) ([]Match, error) {
	_, tx, err := visibleStatement(ctx, statement)
	if err != nil {
		return nil, err
	}
	return statementMatches(ctx, tx, statement, lineNo)
}
