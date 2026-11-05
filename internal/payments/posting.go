package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Matched statement lines are posted (ADR-0047, T49f) to the house bank's
// main bank account, against what the match clears: each payment's line on
// the bank's clearing account, cleared with the posting, or the posting
// rule's account. A posting is dated on the line's booking date, or on a
// date in an open period a person gives with a reason; a locked or closed
// period refuses it with the ledger's error. It waits for an approval
// request (approvals.BankClearing) unless the company's tier for the
// line's amount has no steps, and whoever matched the line by hand does
// not approve it. The rules' matches are posted as they are made, on
// import and on a run of the rules, by whoever ran them, unless an agent
// did (agents never post) or the bank has a statement in gap; a person
// posts the others (POST .../lines/{line_no}:post).

// StatementDocumentType is the document type of statement postings, G/L
// account documents (SAP SA) like manual entries, told apart by their
// source context; they draw numbers from the journal_sa number range.
const StatementDocumentType = "SA"

// Approvals opens approval requests (workflow context): of statement
// lines' postings, and of large payment runs' releases (ADR-0038), which
// the release asks whether it needs and withdraws when it no longer waits
// for them.
type Approvals interface {
	Open(ctx context.Context, in approvals.NewRequest) (id uuid.UUID, needed bool, err error)
	Required(ctx context.Context, in approvals.NewRequest) (bool, error)
	Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error
}

// LinePosting is a person's posting of a matched line: on its booking
// date, or on Date with the reason why.
type LinePosting struct {
	Date   *time.Time
	Reason string
}

// postable is a matched line to post, with its live match.
type postable struct {
	LineNo      int32
	Amount      money.Minor
	BookingDate time.Time
	Status      string
	MatchID     *uuid.UUID
}

// PostLine posts a matched line of a statement the caller may access,
// whose version passes ifMatch, or asks for the approval of its posting.
func (e *Endpoints) PostLine(ctx context.Context, statement uuid.UUID, lineNo int32, ifMatch func(int32) error, in LinePosting) (Match, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return Match{}, err
	}
	if (in.Date == nil) != (in.Reason == "") {
		return Match{}, problem.Unprocessable("request.validation_failed", "A posting date other than the booking date is given with the reason.",
			problem.FieldError{Field: "reason", Code: "invalid", Message: "with posting_date, and only with it"})
	}
	s, tx, err := visibleStatement(ctx, statement)
	if err != nil {
		return Match{}, err
	}
	if err := lockStatement(ctx, tx, s, ifMatch); err != nil {
		return Match{}, err
	}
	l, err := postableLine(ctx, tx, statement, lineNo)
	if err != nil {
		return Match{}, err
	}
	date := l.BookingDate
	if in.Date != nil {
		date = *in.Date
	}
	// A later date takes a line out of a locked period; an earlier one
	// would book it before the bank did, which no statement's figures
	// explain (T49h).
	if date.Before(l.BookingDate) {
		return Match{}, problem.Unprocessable("request.validation_failed", "A line posts on its booking date or later.",
			problem.FieldError{Field: "posting_date", Code: "invalid", Message: "on or after the booking date " + l.BookingDate.Format(time.DateOnly)})
	}
	if err := e.submit(ctx, tx, p, s, l, date, in.Reason); err != nil {
		return Match{}, err
	}
	if err := e.reconcile(ctx, tx, p, s); err != nil {
		return Match{}, err
	}
	version, err := bumpStatement(ctx, tx, p, statement)
	if err != nil {
		return Match{}, err
	}
	rows, _ := tx.Query(ctx, matchSelect+` WHERE m.id = $1`, *l.MatchID)
	out, err := pgx.CollectExactlyOneRow(rows, scanMatch)
	if err != nil {
		return Match{}, fmt.Errorf("payments: read match %s: %w", *l.MatchID, err)
	}
	out.StatementVersion = version
	return out, nil
}

// postableLine locks the line and returns it with its live match; it must
// be matched.
func postableLine(ctx context.Context, tx pgx.Tx, statement uuid.UUID, lineNo int32) (postable, error) {
	rows, _ := tx.Query(ctx, `SELECT l.line_no, l.amount, l.booking_date, l.status, m.id FROM payments.bank_statement_line l
		LEFT JOIN payments.statement_match m ON m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no AND m.status <> 'rejected'
		WHERE l.statement_id = $1 AND l.line_no = $2 FOR UPDATE OF l`, statement, lineNo)
	l, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[postable])
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return postable{}, problem.NotFoundOf("bank statement line")
	case err != nil:
		return postable{}, fmt.Errorf("payments: line %d of statement %s to post: %w", lineNo, statement, err)
	case l.Status == "pending_approval" || l.Status == "posted":
		return postable{}, problem.Conflict("payments.statement_line_posting", fmt.Sprintf("Line %d is %s already.", lineNo, strings.ReplaceAll(l.Status, "_", " ")))
	case l.Status != MatchMatched:
		return postable{}, problem.Conflict("payments.statement_line_not_matched", fmt.Sprintf("Line %d is %s; only a matched line is posted.", lineNo, l.Status))
	}
	return l, nil
}

// submit posts the matched line on the date, or has it wait for the
// approval of its posting. Before it waits, the house bank, the targets
// and the period are checked, so a line dated in a locked period never
// waits; what only the ledger's posting checks (an account not open-item
// managed, a missing rate) refuses the approval instead, and the request
// stays pending for the approver to reject.
func (e *Endpoints) submit(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement, l postable, date time.Time, reason string) error {
	in, err := e.bankPosting(ctx, tx, s, l, date)
	if err != nil {
		return err
	}
	le, _, err := e.LegalEntities.Entity(ctx, s.LegalEntityID) // the statement's: it exists
	if err != nil {
		return fmt.Errorf("payments: legal entity %s of statement %s: %w", s.LegalEntityID, s.ID, err)
	}
	amount, currency := l.Amount, s.Currency
	if amount < 0 {
		amount = -amount
	}
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.BankClearing, ObjectID: *l.MatchID,
		Title:  fmt.Sprintf("Bank statement %s line %d of %s: %s %s", s.Ref, l.LineNo, date.Format(time.DateOnly), l.Amount.Plain(s.Exponent), s.Currency),
		Amount: &amount, Currency: &currency, LegalEntityID: s.LegalEntityID, Company: le.Code, Date: date})
	if err != nil {
		return err
	}
	if !needed {
		return postMatch(ctx, tx, p, e.Postings, s, le.Code, l, in, reason, nil)
	}
	if err := e.Postings.Postable(ctx, s.LegalEntityID, date); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.statement_match SET posting_date = $2, posting_reason = nullif($3, ''), approval_request_id = $4,
			version = version + 1, updated_by = $5 WHERE id = $1`, *l.MatchID, date, reason, request, p.ID); err != nil {
		return fmt.Errorf("payments: submit the match of line %d of statement %s: %w", l.LineNo, s.ID, err)
	}
	if err := setLineStatus(ctx, tx, p, s.ID, l.LineNo, "pending_approval"); err != nil {
		return err
	}
	return record(ctx, "payments.statement_match.submit", *l.MatchID, map[string]any{"bank_statement_id": s.ID, "line_no": l.LineNo,
		"approval_request_id": request, "posting_date": date.Format(time.DateOnly), "reason": reason})
}

func setLineStatus(ctx context.Context, tx pgx.Tx, p reqctx.Principal, statement uuid.UUID, lineNo int32, status string) error {
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_statement_line SET status = $3, version = version + 1, updated_by = $4 WHERE statement_id = $1 AND line_no = $2`,
		statement, lineNo, status, p.ID); err != nil {
		return fmt.Errorf("payments: set the status of line %d of statement %s: %w", lineNo, statement, err)
	}
	return nil
}

// postMatch posts the line, records the entry on its match, audits it and
// publishes BankStatementLinePosted. Its callers hold the line FOR UPDATE
// and found it matched or waiting for this request, so a match is posted
// once; statement_match_journal_entry keeps one entry to one match. The
// event names the statement's company by company (companyCode).
func postMatch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, postings Postings, s Statement, company string, l postable, in ledger.BankPosting,
	reason string, request *uuid.UUID) error {
	entry, err := postings.PostBank(ctx, in)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.statement_match SET posting_date = $2, posting_reason = nullif($3, ''), journal_entry_year = $4,
			journal_entry_id = $5, version = version + 1, updated_by = $6 WHERE id = $1`,
		*l.MatchID, in.PostingDate, reason, entry.FiscalYear, entry.ID, p.ID); err != nil {
		return fmt.Errorf("payments: record the posting of line %d of statement %s: %w", l.LineNo, s.ID, err)
	}
	if err := setLineStatus(ctx, tx, p, s.ID, l.LineNo, "posted"); err != nil {
		return err
	}
	day := in.PostingDate.Format(time.DateOnly)
	return record(ctx, "payments.statement_match.post", *l.MatchID, map[string]any{"bank_statement_id": s.ID, "line_no": l.LineNo,
		"journal_entry_id": entry.ID, "document_number": entry.DocumentNumber, "posting_date": day, "reason": reason, "approval_request_id": request},
		outbox.Event{Context: "payments", Name: "bank_statement_line_posted", AggregateType: "payments.statement_match", AggregateID: *l.MatchID,
			Payload: withCompany(map[string]any{"statement_match_id": *l.MatchID, "bank_statement_id": s.ID, "line_no": l.LineNo, "house_bank_id": s.HouseBankID,
				"legal_entity_id": s.LegalEntityID, "amount": money.EventAmount(l.Amount, s.Currency, s.Exponent), "posting_date": day,
				"journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "document_number": entry.DocumentNumber,
				"cleared_payments": len(in.Entries), "approval_request_id": request}, company)})
}

// bankPosting is the ledger posting of the line on the date: the house
// bank must have a main bank account and no statement in gap, and the
// match's payments must pay the line's amount, or its posting rule still
// be the company's.
func (e *Endpoints) bankPosting(ctx context.Context, tx pgx.Tx, s Statement, l postable, date time.Time) (ledger.BankPosting, error) {
	var clearing uuid.UUID
	var main, incoming *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT gl_account_id, main_gl_account_id, incoming_gl_account_id FROM payments.house_bank WHERE id = $1`, s.HouseBankID).
		Scan(&clearing, &main, &incoming); err != nil {
		return ledger.BankPosting{}, fmt.Errorf("payments: accounts of house bank %s: %w", s.HouseBankID, err)
	}
	if main == nil {
		return ledger.BankPosting{}, problem.Conflict("payments.statement_bank_not_posting",
			"The house bank has no main bank account (main_gl_account): its statements are imported but never posted.")
	}
	gap, err := inGap(ctx, tx, s.HouseBankID)
	if err != nil {
		return ledger.BankPosting{}, err
	}
	if gap {
		return ledger.BankPosting{}, problem.Conflict("payments.statement_gap",
			"The house bank has a statement in gap: its lines are posted once the missing statement is imported.")
	}
	text := fmt.Sprintf("Bank statement %s line %d", s.Ref, l.LineNo)
	in := ledger.BankPosting{LegalEntityID: s.LegalEntityID, DocumentType: StatementDocumentType, PostingDate: date, Currency: s.Currency,
		BankAccountID: *main, Amount: l.Amount, ClearingAccountID: clearing, HeaderText: &text, SourceContext: "payments", SourceDocumentID: *l.MatchID}
	return in, e.postingTargets(ctx, tx, s, l, incoming, &in)
}

// inGap reports whether the house bank has a statement in gap: its lines
// are not posted until the missing statement arrives (ADR-0047).
func inGap(ctx context.Context, tx pgx.Tx, houseBank uuid.UUID) (bool, error) {
	var gap bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM payments.bank_statement WHERE house_bank_id = $1 AND status = 'gap')`, houseBank).Scan(&gap); err != nil {
		return false, fmt.Errorf("payments: statements in gap of house bank %s: %w", houseBank, err)
	}
	return gap, nil
}

// matchedTarget is a target of a match as it posts: a payment's posting
// and amount, a posting rule, or the incoming payments clearing account.
type matchedTarget struct {
	Year     *int16
	Entry    *uuid.UUID
	Amount   *money.Minor
	Rule     *string
	Incoming bool
}

// postingTargets sets what the posting clears, or posts against: an
// incoming payment posts against the house bank's incoming payments
// clearing account (incoming), where it is an open item until accounts
// receivable clears it (ADR-0047).
func (e *Endpoints) postingTargets(ctx context.Context, tx pgx.Tx, s Statement, l postable, incoming *uuid.UUID, in *ledger.BankPosting) error {
	rows, _ := tx.Query(ctx, `SELECT p.journal_entry_year, p.journal_entry_id, p.amount, t.posting_rule, t.incoming FROM payments.statement_match_target t
		LEFT JOIN payments.payment p ON p.tenant_id = t.tenant_id AND p.id = t.payment_id
		WHERE t.match_id = $1 ORDER BY t.candidate, t.target_no`, *l.MatchID)
	targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[matchedTarget])
	if err != nil {
		return fmt.Errorf("payments: targets of the match of line %d of statement %s: %w", l.LineNo, s.ID, err)
	}
	// A match decided has one candidate: a posting rule, the incoming
	// account, or payments (statement_match_target_one).
	switch {
	case targets[0].Rule != nil:
		in.CounterAccountID, err = e.ruleAccount(ctx, s, *targets[0].Rule)
		return err
	case targets[0].Incoming && incoming == nil:
		return problem.Conflict("payments.statement_incoming_gone", "The house bank no longer has an incoming payments clearing account "+
			"(incoming_gl_account); match the line again.")
	case targets[0].Incoming:
		in.CounterAccountID = *incoming
		return nil
	}
	var paid money.Minor
	for _, t := range targets {
		in.Entries = append(in.Entries, ledger.ItemRef{FiscalYear: *t.Year, EntryID: *t.Entry})
		paid += *t.Amount // a statement's payments, far below the overflow
	}
	if paid != -l.Amount {
		return problem.Conflict("payments.statement_amount_differs", fmt.Sprintf("The payments matched to line %d pay %s %s; the line is %s %s. Match it again.",
			l.LineNo, paid.Plain(s.Exponent), s.Currency, (-l.Amount).Plain(s.Exponent), s.Currency))
	}
	return nil
}

// ruleAccount is the account of the company's posting rule.
func (e *Endpoints) ruleAccount(ctx context.Context, s Statement, code string) (uuid.UUID, error) {
	co, err := e.statementCompany(ctx, s.LegalEntityID)
	if err != nil {
		return uuid.Nil, err
	}
	var rules []config.StatementPostingRule
	if co != nil {
		rules = co.BankStatements.PostingRules
	}
	i := slices.IndexFunc(rules, func(r config.StatementPostingRule) bool { return r.TransactionCode == code })
	if i < 0 {
		return uuid.Nil, problem.Conflict("payments.statement_posting_rule_gone", "Posting rule "+code+" is no longer one of the company's; match the line again.")
	}
	chart, err := e.Accounts.Chart(ctx, s.LegalEntityID)
	if err != nil {
		return uuid.Nil, err
	}
	return chart[rules[i].Account].ID, nil // the document names an account of its chart (config.Validate), which the ledger applied
}

// autoPost posts, or submits for approval, the lines the rules matched on
// the statement that were never submitted, unless an agent ran the rules
// (agents never post), a person or integration without
// payments.statement.post, or barred from it by a segregation-of-duties
// rule, did, or the bank has a statement in gap; the
// statement fetcher, a system principal, posts. Each line posts
// under a savepoint: a refusal (a
// locked period, a bank in gap, no rate) is rolled back and audited, and
// the line stays matched for a person. It reports whether a line moved on.
func (e *Endpoints) autoPost(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement) (bool, error) {
	if p.Kind == reqctx.KindAgent {
		return false, nil
	}
	// The fetcher imports for the bank's company; a person or integration
	// needs the permission to post, free of a segregation-of-duties rule,
	// as Authorize decides it (ADR-0010 As built 2026-11-02).
	if p.Kind != reqctx.KindSystem {
		may, err := authz.MayUse(ctx, "payments.statement.post")
		if err != nil || !may {
			return false, err
		}
	}
	if gap, err := inGap(ctx, tx, s.HouseBankID); err != nil || gap {
		return false, err // automatic posting stops for the bank until the gap closes
	}
	rows, _ := tx.Query(ctx, `SELECT l.line_no, l.amount, l.booking_date, l.status, m.id FROM payments.bank_statement_line l
		JOIN payments.statement_match m ON m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no
		WHERE l.statement_id = $1 AND l.status = 'matched' AND m.status = 'matched' AND m.kind = 'rule' AND m.approval_request_id IS NULL
		ORDER BY l.line_no FOR UPDATE OF l`, s.ID)
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[postable])
	if err != nil {
		return false, fmt.Errorf("payments: matched lines of statement %s to post: %w", s.ID, err)
	}
	moved := false
	for _, l := range lines {
		ok, err := e.tryPost(ctx, tx, p, s, l)
		if err != nil {
			return false, err
		}
		moved = moved || ok
	}
	return moved, nil
}

// tryPost submits the line under a savepoint and reports whether it did; a
// refusal rolls back what the posting wrote and recorded, and is audited.
func (e *Endpoints) tryPost(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement, l postable) (bool, error) {
	if _, err := tx.Exec(ctx, `SAVEPOINT statement_line_post`); err != nil {
		return false, fmt.Errorf("payments: savepoint before posting line %d of statement %s: %w", l.LineNo, s.ID, err)
	}
	mark := audit.Mark(ctx)
	err := e.submit(ctx, tx, p, s, l, l.BookingDate, "")
	var why *problem.Problem
	if err != nil && (!errors.As(err, &why) || why.Status >= 500) {
		return false, err
	}
	if err == nil {
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT statement_line_post`); err != nil {
			return false, fmt.Errorf("payments: release the posting of line %d of statement %s: %w", l.LineNo, s.ID, err)
		}
		return true, nil
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT statement_line_post`); err != nil {
		return false, fmt.Errorf("payments: roll back the posting of line %d of statement %s: %w", l.LineNo, s.ID, err)
	}
	audit.DiscardFrom(ctx, mark)
	return false, audit.Add(ctx, audit.Entry{Action: "payments.statement_match.post", ObjectType: "payments.statement_match", ObjectID: *l.MatchID,
		Outcome: "refused", Detail: map[string]any{"bank_statement_id": s.ID, "line_no": l.LineNo, "code": why.Code, "detail": why.Detail}})
}
