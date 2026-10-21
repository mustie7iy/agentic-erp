package payments

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The reconciliation agent's proposals (ADR-0047, T49g). For a line the
// rules left unmatched or open, the agent proposes, in one of its
// sessions, the payments the line clears, a posting rule, or its
// exclusion, with a reason: a live match of kind agent, open, that
// reserves nothing and never posts, and that the rules never take up
// again. A person decides it: accepting matches or excludes the line by it
// as the person's own decision (kind manual, so the person posts it and
// cannot approve the posting), rejecting makes the line unmatched again,
// and the person's own match or exclusion overrides it. A session proposes
// once per line: the same proposal again is the first.

// What an agent proposes, and what a person did with it.
const (
	ProposeMatch      = "match"
	ProposeExclude    = "exclude"
	OutcomeAccepted   = "accepted"
	OutcomeRejected   = "rejected"
	OutcomeOverridden = "overridden"
)

// Sessions is what payments asks of the agents context, through the
// adapter in internal/app: that a session is the calling agent's and
// running, with the agents context's problem when it is not.
type Sessions interface {
	Running(ctx context.Context, id uuid.UUID) error
}

// LineProposal is an agent's proposal for a line: the payments it clears,
// a posting rule, or its exclusion (one of them), and why.
type LineProposal struct {
	SessionID   uuid.UUID
	PaymentIDs  []uuid.UUID
	PostingRule string
	Exclude     bool
	Reason      string
}

// what is the proposal's kind: match or exclude.
func (in LineProposal) what() string {
	if in.Exclude {
		return ProposeExclude
	}
	return ProposeMatch
}

// ProposeLine records the calling agent's proposal for a line of a
// statement it may access, whose version passes ifMatch. created is false
// when the session proposed the same for the line before: that proposal is
// returned as it is now, whatever the version, so a retry of a proposal
// whose answer was lost finds it.
func (e *Endpoints) ProposeLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, in LineProposal) (_ Match, created bool, err error) {
	if n := len(slices.DeleteFunc([]bool{len(in.PaymentIDs) > 0, in.PostingRule != "", in.Exclude}, func(b bool) bool { return !b })); n != 1 {
		return Match{}, false, problem.Unprocessable("request.validation_failed", "A proposal names the payments the line clears, a posting rule, or its exclusion: one of them.",
			problem.FieldError{Field: "payment_ids", Code: "invalid", Message: "payment_ids, posting_rule or exclude"})
	}
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return Match{}, false, err
	}
	if err := e.Sessions.Running(ctx, in.SessionID); err != nil {
		return Match{}, false, err
	}
	s, tx, err := visibleStatement(ctx, statement)
	if err != nil {
		return Match{}, false, err
	}
	var version int32
	if err := lockStatement(ctx, tx, s, func(v int32) error { version = v; return nil }); err != nil {
		return Match{}, false, err
	}
	targets, err := e.proposedTargets(ctx, tx, s, in)
	if err != nil {
		return Match{}, false, err
	}
	if prior, ok, err := sessionProposal(ctx, tx, in, statement, lineNo, targets); ok || err != nil {
		prior.StatementVersion = version
		return prior, false, err
	}
	if err := ifMatch(version); err != nil {
		return Match{}, false, err
	}
	if err := proposable(ctx, tx, statement, lineNo); err != nil {
		return Match{}, false, err
	}
	d := openLine{p: p, tx: tx, s: s, lineNo: lineNo}
	if err := rejectLive(ctx, tx, p, s, lineNo, rejection{}); err != nil { // a rule's open match: the proposal is the line's now
		return Match{}, false, err
	}
	m := newMatch{kind: MatchAgent, reason: in.Reason, status: MatchOpen, session: &in.SessionID, proposal: new(in.what())}
	if !in.Exclude {
		m.candidates = [][]MatchTarget{targets}
	}
	id, err := writeMatch(ctx, tx, p, s, lineNo, m)
	if err != nil {
		return Match{}, false, err
	}
	out, err := d.answer(ctx, id)
	return out, true, err
}

// proposedTargets checks what a proposal names as a person's match is
// checked (manualTargets): payments the line may clear, or a posting rule
// of the company; an exclusion names none.
func (e *Endpoints) proposedTargets(ctx context.Context, tx pgx.Tx, s Statement, in LineProposal) ([]MatchTarget, error) {
	if in.Exclude {
		return nil, nil
	}
	return e.manualTargets(ctx, tx, s, ManualMatch{PaymentIDs: in.PaymentIDs, PostingRule: in.PostingRule})
}

// sessionProposal is the session's proposal for the line, when it made
// one: the same proposal (what it proposes and its targets) answers it
// again, another is refused.
func sessionProposal(ctx context.Context, tx pgx.Tx, in LineProposal, statement uuid.UUID, lineNo int32, targets []MatchTarget) (Match, bool, error) {
	rows, _ := tx.Query(ctx, matchSelect+` WHERE m.session_id = $1 AND m.statement_id = $2 AND m.line_no = $3`, in.SessionID, statement, lineNo)
	prior, err := pgx.CollectRows(rows, scanMatch)
	if err != nil {
		return Match{}, false, fmt.Errorf("payments: the proposal of session %s for line %d of statement %s: %w", in.SessionID, lineNo, statement, err)
	}
	if len(prior) == 0 {
		return Match{}, false, nil
	}
	m := prior[0] // a session proposes once per line (statement_match_session)
	if *m.Proposal != in.what() || !sameTargets(m.Targets, targets) {
		return Match{}, false, problem.Conflict("payments.statement_line_proposed", fmt.Sprintf("This session proposed otherwise for line %d already.", lineNo))
	}
	return m, true, nil
}

// sameTargets reports whether two lists of one candidate's targets name
// the same payments or posting rule, in any order.
func sameTargets(a, b []MatchTarget) bool {
	key := func(ts []MatchTarget) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			rule := ""
			if t.PostingRule != nil {
				rule = *t.PostingRule
			}
			out[i] = optID(t.PaymentID) + "|" + rule
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

// proposable refuses a line that takes no proposal: one a person or a
// rule decided, one on its way to the ledger, and one with an agent's
// proposal for a person to decide.
func proposable(ctx context.Context, tx pgx.Tx, statement uuid.UUID, lineNo int32) error {
	status, live, err := lineState(ctx, tx, statement, lineNo)
	switch {
	case err != nil:
		return err
	case live != nil && live.Kind == MatchAgent:
		return problem.Conflict("payments.statement_line_proposed", fmt.Sprintf("Line %d has an agent's proposal for a person to decide.", lineNo))
	case status != "unmatched" && (live == nil || live.Kind != MatchRule || live.Status != MatchOpen):
		return problem.Conflict("payments.statement_line_not_open", fmt.Sprintf("Line %d is %s; only a line the rules left unmatched or open takes a proposal.", lineNo, status))
	}
	return nil
}

// noProposal is the refusal of a decision on a line without an agent's
// open proposal.
func noProposal(lineNo int32) error {
	return problem.Conflict("payments.statement_line_no_proposal", fmt.Sprintf("Line %d has no agent's proposal to decide.", lineNo))
}

// AcceptProposal matches or excludes the line by the agent's open
// proposal, as the person's own decision; its targets are checked again
// as a person's match is.
func (e *Endpoints) AcceptProposal(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, comment string) (Match, error) {
	return e.decideLine(ctx, statement, lineNo, ifMatch, OutcomeAccepted, func(ctx context.Context, d openLine) (newMatch, error) {
		live := d.live
		if live == nil || live.Kind != MatchAgent {
			return newMatch{}, noProposal(lineNo)
		}
		reason := "Accepted the agent's proposal: " + live.Reason
		if comment != "" {
			reason += " " + comment
		}
		if *live.Proposal == ProposeExclude {
			return newMatch{kind: MatchManual, reason: reason, status: MatchExcluded}, nil
		}
		in := ManualMatch{}
		for _, t := range live.Targets {
			if t.PaymentID != nil {
				in.PaymentIDs = append(in.PaymentIDs, *t.PaymentID)
			}
			if t.PostingRule != nil {
				in.PostingRule = *t.PostingRule
			}
		}
		targets, err := e.manualTargets(ctx, d.tx, d.s, in)
		return newMatch{kind: MatchManual, reason: reason, status: MatchMatched, candidates: [][]MatchTarget{targets}}, err
	})
}

// RejectProposal turns down the agent's open proposal for the line, with
// the person's reason: the line is unmatched again.
func (e *Endpoints) RejectProposal(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, reason string) (Match, error) {
	d, err := lockLine(ctx, statement, lineNo, ifMatch)
	if err != nil {
		return Match{}, err
	}
	if d.live == nil || d.live.Kind != MatchAgent {
		return Match{}, noProposal(lineNo)
	}
	if err := rejectLive(ctx, d.tx, d.p, d.s, lineNo, rejection{outcome: OutcomeRejected, reason: reason}); err != nil {
		return Match{}, err
	}
	if _, err := d.tx.Exec(ctx, `UPDATE payments.bank_statement_line SET status = 'unmatched', version = version + 1, updated_by = $3
		WHERE statement_id = $1 AND line_no = $2`, statement, lineNo, d.p.ID); err != nil {
		return Match{}, fmt.Errorf("payments: reopen line %d of statement %s: %w", lineNo, statement, err)
	}
	return d.answer(ctx, d.live.ID)
}
