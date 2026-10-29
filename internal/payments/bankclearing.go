package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// BankClearings moves a matched statement line on when the approval of its
// posting completes (approvals.Handler for approvals.BankClearing), in the
// deciding transaction. Each acts only while the line waits for that
// request, so a request the match no longer names changes nothing and
// fails the decision: a second callback for the same request posts
// nothing.
type BankClearings struct {
	Endpoints *Endpoints
}

// waiting is a line that waits for the approval of its posting.
type waiting struct {
	s    Statement
	l    postable
	date time.Time
	why  *string
}

// waitingLine locks the statement and the line of the match, and returns
// them when the line waits for the request. The decider may not see the
// statement's company: the request is what authorises the posting, so the
// statement is read without the company filter.
func waitingLine(ctx context.Context, tx pgx.Tx, match, request uuid.UUID) (waiting, error) {
	s, err := scanStatement(tx.QueryRow(ctx, statementSelect+` WHERE s.id = (SELECT statement_id FROM payments.statement_match WHERE id = $1)`, match))
	if err != nil {
		return waiting{}, fmt.Errorf("payments: statement of match %s: %w", match, err)
	}
	if err := lockStatement(ctx, tx, s, nil); err != nil {
		return waiting{}, err
	}
	w := waiting{s: s}
	var named *uuid.UUID
	var date *time.Time
	err = tx.QueryRow(ctx, `SELECT l.line_no, l.amount, l.booking_date, l.status, m.id, m.approval_request_id, m.posting_date, m.posting_reason
		FROM payments.statement_match m JOIN payments.bank_statement_line l ON l.tenant_id = m.tenant_id AND l.statement_id = m.statement_id AND l.line_no = m.line_no
		WHERE m.id = $1 FOR UPDATE OF l, m`, match).Scan(&w.l.LineNo, &w.l.Amount, &w.l.BookingDate, &w.l.Status, &w.l.MatchID, &named, &date, &w.why)
	if err != nil {
		return waiting{}, fmt.Errorf("payments: line of match %s: %w", match, err)
	}
	if w.l.Status != "pending_approval" || named == nil || *named != request {
		return waiting{}, fmt.Errorf("payments: match %s (line %s) does not wait for request %s", match, w.l.Status, request)
	}
	w.date = *date // a request has its date (statement_match_posting_date)
	return w, nil
}

// Approved posts the line as approved, checked again: a period closed, a
// gap opened or a payment cleared since it was submitted refuses the
// decision, and the request stays pending.
func (b BankClearings) Approved(ctx context.Context, match, request uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	w, err := waitingLine(ctx, tx, match, request)
	if err != nil {
		return err
	}
	in, err := b.Endpoints.bankPosting(ctx, tx, w.s, w.l, w.date)
	if err != nil {
		return err
	}
	why := ""
	if w.why != nil {
		why = *w.why
	}
	if err := postMatch(ctx, tx, p, b.Endpoints.Postings, w.s, w.l, in, why, &request); err != nil {
		return err
	}
	if err := b.Endpoints.reconcile(ctx, tx, p, w.s); err != nil {
		return err
	}
	_, err = bumpStatement(ctx, tx, p, w.s.ID)
	return err
}

// Rejected returns the line to matched: the posting is not made, and a
// person posts it again (on another date), matches it again or excludes
// it. The match keeps the rejected request, so the rules' run does not
// submit it again.
func (BankClearings) Rejected(ctx context.Context, match, request uuid.UUID, comment string) error {
	return endPosting(ctx, match, request, "posting_rejected", comment)
}

// Cancelled returns the line to matched: its requester withdrew the
// request.
func (BankClearings) Cancelled(ctx context.Context, match, request uuid.UUID, comment string) error {
	return endPosting(ctx, match, request, "posting_withdrawn", comment)
}

func endPosting(ctx context.Context, match, request uuid.UUID, action, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	w, err := waitingLine(ctx, tx, match, request)
	if err != nil {
		return err
	}
	if err := setLineStatus(ctx, tx, p, w.s.ID, w.l.LineNo, MatchMatched); err != nil {
		return err
	}
	if _, err := bumpStatement(ctx, tx, p, w.s.ID); err != nil {
		return err
	}
	return record(ctx, "payments.statement_match."+action, match, map[string]any{"bank_statement_id": w.s.ID, "line_no": w.l.LineNo,
		"approval_request_id": request, "comment": comment})
}

// Barred bars whoever matched the line by hand from approving its posting
// (segregation of duties, ADR-0047); a rule's match bars nobody but the
// requester.
func (BankClearings) Barred(ctx context.Context, match, actor uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var barred bool
	// Matches are never deleted: the request's match is there.
	if err := tx.QueryRow(ctx, `SELECT kind = 'manual' AND created_by = $2 FROM payments.statement_match WHERE id = $1`, match, actor).Scan(&barred); err != nil {
		return false, fmt.Errorf("payments: who matched match %s: %w", match, err)
	}
	return barred, nil
}

// Resources is where the API reads each match (approvals.Locator): the
// statement whose line carries it, which lists the line's match_id.
func (BankClearings) Resources(ctx context.Context, matches []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, statement_id FROM payments.statement_match WHERE id = ANY($1)`, matches)
	out := map[uuid.UUID]string{}
	var match, statement uuid.UUID
	_, err = pgx.ForEachRow(rows, []any{&match, &statement}, func() error { out[match] = "/v1/bank-statements/" + statement.String(); return nil })
	if err != nil {
		return out, fmt.Errorf("payments: statements of %d matches: %w", len(matches), err)
	}
	return out, nil
}
