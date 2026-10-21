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

// ManualMatch is a person's match of a line: the payments it clears, or
// the posting rule that posts it, and why.
type ManualMatch struct {
	PaymentIDs  []uuid.UUID
	PostingRule string
	Reason      string
}

// MatchLine matches the line by hand, replacing its live match; an
// agent's proposal it replaces is overridden.
func (e *Endpoints) MatchLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, in ManualMatch) (Match, error) {
	if (len(in.PaymentIDs) == 0) == (in.PostingRule == "") {
		return Match{}, problem.Unprocessable("request.validation_failed", "A match names the payments it clears or a posting rule, not both.",
			problem.FieldError{Field: "payment_ids", Code: "invalid", Message: "payment_ids or posting_rule"})
	}
	return e.decideLine(ctx, statement, lineNo, ifMatch, OutcomeOverridden, func(ctx context.Context, d openLine) (newMatch, error) {
		targets, err := e.manualTargets(ctx, d.tx, d.s, in)
		return newMatch{kind: MatchManual, reason: in.Reason, status: MatchMatched, candidates: [][]MatchTarget{targets}}, err
	})
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

// lineState reads the line's status, locking it, and its live match.
func lineState(ctx context.Context, tx pgx.Tx, statement uuid.UUID, lineNo int32) (string, *Match, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM payments.bank_statement_line WHERE statement_id = $1 AND line_no = $2 FOR UPDATE`, statement, lineNo).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, problem.NotFoundOf("bank statement line")
	}
	if err != nil {
		return "", nil, fmt.Errorf("payments: line %d of statement %s: %w", lineNo, statement, err)
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
