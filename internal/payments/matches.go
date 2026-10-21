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

// MatchLine matches the line by hand, replacing its live match.
func (e *Endpoints) MatchLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, in ManualMatch) (Match, error) {
	if (len(in.PaymentIDs) == 0) == (in.PostingRule == "") {
		return Match{}, problem.Unprocessable("request.validation_failed", "A match names the payments it clears or a posting rule, not both.",
			problem.FieldError{Field: "payment_ids", Code: "invalid", Message: "payment_ids or posting_rule"})
	}
	return e.decideLine(ctx, statement, lineNo, ifMatch, func(ctx context.Context, tx pgx.Tx, s Statement) (newMatch, error) {
		targets, err := e.manualTargets(ctx, tx, s, in)
		return newMatch{kind: MatchManual, reason: in.Reason, status: MatchMatched, candidates: [][]MatchTarget{targets}}, err
	})
}

// ExcludeLine excludes the line from reconciliation, replacing its live
// match: the bank's booking clears nothing the ledger holds.
func (e *Endpoints) ExcludeLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, reason string) (Match, error) {
	return e.decideLine(ctx, statement, lineNo, ifMatch, func(context.Context, pgx.Tx, Statement) (newMatch, error) {
		return newMatch{kind: MatchManual, reason: reason, status: MatchExcluded}, nil
	})
}

// decideLine records a person's decision on a line of a statement the
// caller may access, whose version passes ifMatch: under the bank's lock,
// the line's live match is
// rejected and the decision recorded. A line on its way to the ledger
// (T49f) is not changed. Only people decide (the operations are x-agent:
// false); the agent proposes (T49g).
func (e *Endpoints) decideLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error,
	decide func(ctx context.Context, tx pgx.Tx, s Statement) (newMatch, error)) (Match, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return Match{}, err
	}
	s, tx, err := visibleStatement(ctx, statement)
	if err != nil {
		return Match{}, err
	}
	if err := lockStatement(ctx, tx, s, ifMatch); err != nil {
		return Match{}, err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM payments.bank_statement_line WHERE statement_id = $1 AND line_no = $2 FOR UPDATE`, statement, lineNo).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Match{}, problem.NotFoundOf("bank statement line")
	}
	if err != nil {
		return Match{}, fmt.Errorf("payments: line %d of statement %s: %w", lineNo, statement, err)
	}
	if status == "pending_approval" || status == "posted" {
		return Match{}, problem.Conflict("payments.statement_line_posting", fmt.Sprintf("Line %d is %s; its match stays.", lineNo, strings.ReplaceAll(status, "_", " ")))
	}
	if err := rejectLive(ctx, tx, p, statement, lineNo); err != nil {
		return Match{}, err
	}
	m, err := decide(ctx, tx, s)
	if err != nil {
		return Match{}, err
	}
	id, err := writeMatch(ctx, tx, p, s, lineNo, m)
	if err != nil {
		return Match{}, err
	}
	version, err := bumpStatement(ctx, tx, p, statement)
	if err != nil {
		return Match{}, err
	}
	rows, _ := tx.Query(ctx, matchSelect+` WHERE m.id = $1`, id)
	out, err := pgx.CollectExactlyOneRow(rows, scanMatch)
	if err != nil {
		return Match{}, fmt.Errorf("payments: read match %s: %w", id, err)
	}
	out.StatementVersion = version
	return out, nil
}

// rejectLive rejects the line's live match, if it has one.
func rejectLive(ctx context.Context, tx pgx.Tx, p reqctx.Principal, statement uuid.UUID, lineNo int32) error {
	var id uuid.UUID
	var was string
	err := tx.QueryRow(ctx, `UPDATE payments.statement_match n SET status = 'rejected', version = n.version + 1, updated_by = $3
		FROM payments.statement_match o WHERE o.tenant_id = n.tenant_id AND o.id = n.id
			AND n.statement_id = $1 AND n.line_no = $2 AND n.status <> 'rejected' RETURNING n.id, o.status`, statement, lineNo, p.ID).Scan(&id, &was)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("payments: reject the match of line %d of statement %s: %w", lineNo, statement, err)
	}
	return record(ctx, "payments.statement_match.reject", id, map[string]any{"bank_statement_id": statement, "line_no": lineNo, "previous_status": was})
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
