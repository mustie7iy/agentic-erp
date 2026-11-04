package payments

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statement lines are matched to what they clear (ADR-0047, T49e) by the
// company's matching rules (companies[].bank_statements.matching_rules),
// tried in their order: the end-to-end id of a payment, the message or
// payment information id of a bank file (a batch booking: all its
// payments), the bank transaction code of a posting rule (charges,
// interest), and the amount within a tolerance, the run date within a
// window of the booking date and, when asked, the payee's IBAN or name. The
// first rule that finds candidates decides: with exactly one, and the
// payments' amounts the line's, the line is matched; otherwise the line
// keeps an open match listing the candidates for a person, who may also
// match a line by hand or exclude it. Only unmatched lines are matched, in
// line order, under the house bank's statement lock, and a payment matched
// to a line is no other line's candidate: matching again finds the same.
// The rules' matches are posted as they are made (posting.go, T49f).

// Match kinds and statuses (payments.statement_match).
const (
	MatchRule     = "rule"
	MatchManual   = "manual"
	MatchAgent    = "agent"
	MatchOpen     = "open"
	MatchMatched  = "matched"
	MatchExcluded = "excluded"
	MatchRejected = "rejected"
)

// lineStatus is a line's status while it has a live match of the status.
var lineStatus = map[string]string{MatchOpen: "proposed", MatchMatched: MatchMatched, MatchExcluded: MatchExcluded}

// MatchTarget is what a match clears: a payment, with the bank file of a
// batch booking, a posting rule, or (Incoming, T49h) the house bank's
// incoming payments clearing account; Candidate tells an open match's
// candidates apart (1 for a match decided).
type MatchTarget struct {
	Candidate   int16      `json:"candidate"`
	PaymentID   *uuid.UUID `json:"payment_id,omitempty"`
	BankFileID  *uuid.UUID `json:"bank_file_id,omitempty"`
	PostingRule *string    `json:"posting_rule,omitempty"`
	Incoming    bool       `json:"incoming,omitempty"`
}

// Match is a row of payments.statement_match with its targets.
type Match struct {
	ID, StatementID      uuid.UUID
	LineNo               int32
	Kind                 string
	Rule                 *string
	Reason, Status       string
	Targets              []MatchTarget
	Version              int32
	CreatedAt, UpdatedAt time.Time
	CreatedBy, UpdatedBy uuid.UUID
	// StatementVersion is the statement's version after a person's
	// decision, its ETag for the next one; 0 when read.
	StatementVersion int32
	// The posting (T49f): its date and a person's reason for it, the
	// latest approval request, and the journal entry once posted.
	PostingDate       *time.Time
	PostingReason     *string
	ApprovalRequestID *uuid.UUID
	JournalEntryYear  *int16
	JournalEntryID    *uuid.UUID
	// An agent's proposal (T49g): its session, what it proposes and, once
	// a person decided it, the outcome.
	SessionID         *uuid.UUID
	Proposal, Outcome *string
}

// matchLine is what the rules read of a line.
type matchLine struct {
	LineNo          int32
	Amount          money.Minor
	Reversal        bool
	BookingDate     time.Time
	TransactionCode string
	Details         []LineDetail
	Open            *string // the signature of the line's open match by a rule, which a run takes up again
}

// found is what a rule found for a line: its candidates, each a list of
// targets, and whether the one candidate settles the line.
type found struct {
	candidates [][]MatchTarget
	settles    bool
	reason     string
}

// matcher matches a statement's lines by the company's rules.
type matcher struct {
	e     *Endpoints
	tx    pgx.Tx
	p     reqctx.Principal
	s     Statement
	rules []config.MatchingRule
	co    *config.Company
	// company is the code of the statement's company, which its events
	// name (companyCode), read once a statement.
	company string
}

// matchStatement matches the statement's unmatched lines, and takes up
// again those a rule left open, by the rules of its company, in line
// order, under the bank's statement lock, once the statement's version
// passes ifMatch (nil: no check, as on import). A line matched in a pass
// takes its payments from the lines before it, so passes repeat until one
// changes nothing: a second run then finds nothing to change. The lines the
// rules matched are then posted (autoPost, T49f), and the bank's
// statements reconciled that now are (T49h). A run that changed a line
// moves the statement's version on.
func (e *Endpoints) matchStatement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement, ifMatch func(int32) error) error {
	if err := lockStatement(ctx, tx, s, ifMatch); err != nil {
		return err
	}
	if err := e.applyRules(ctx, tx, p, s); err != nil {
		return err
	}
	// A run also takes up what changed in the ledger since: a statement
	// whose lines are decided reconciles once the ledger agrees (T49h).
	return e.reconcile(ctx, tx, p, s)
}

// applyRules runs the company's matching rules on the statement and posts
// what they matched.
func (e *Endpoints) applyRules(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement) error {
	co, err := e.statementCompany(ctx, s.LegalEntityID)
	if err != nil || co == nil || len(co.BankStatements.MatchingRules) == 0 {
		return err
	}
	company, err := e.companyCode(ctx, s.LegalEntityID)
	if err != nil {
		return err
	}
	m := matcher{e: e, tx: tx, p: p, s: s, rules: co.BankStatements.MatchingRules, co: co, company: company}
	// A pass after the first changes a line only when the pass before
	// matched one, which takes payments no rule run gives back: the passes
	// end.
	changed := false
	for {
		again, err := m.pass(ctx)
		if err != nil {
			return err
		}
		if !again {
			break
		}
		changed = true
	}
	posted, err := e.autoPost(ctx, tx, p, s)
	if err != nil {
		return err
	}
	if !changed && !posted {
		return nil
	}
	_, err = bumpStatement(ctx, tx, p, s.ID)
	return err
}

// pass runs the rules once over the lines no match holds and those a rule
// left open, in line order, and reports whether it changed one. Matched and
// excluded lines, and an agent's proposals, are left as they are.
func (m matcher) pass(ctx context.Context) (bool, error) {
	rows, _ := m.tx.Query(ctx, `SELECT l.line_no, l.amount, l.is_reversal, l.booking_date, l.transaction_code, l.details,
			(SELECT m.rule || '|open|' || coalesce((SELECT string_agg(t.candidate || ':' || coalesce(t.payment_id::text, '') || ':' ||
				coalesce(t.bank_file_id::text, '') || ':' || coalesce(t.posting_rule, ''), ' ' ORDER BY t.candidate, t.target_no)
				FROM payments.statement_match_target t WHERE t.tenant_id = m.tenant_id AND t.match_id = m.id), '')
			FROM payments.statement_match m WHERE m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no
				AND m.status = 'open' AND m.kind = 'rule')
		FROM payments.bank_statement_line l WHERE l.statement_id = $1 AND (l.status = 'unmatched' OR (l.status = 'proposed'
			AND EXISTS (SELECT FROM payments.statement_match m WHERE m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id
				AND m.line_no = l.line_no AND m.status = 'open' AND m.kind = 'rule')))
		ORDER BY l.line_no FOR UPDATE OF l`, m.s.ID)
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[matchLine])
	if err != nil {
		return false, fmt.Errorf("payments: lines of statement %s to match: %w", m.s.ID, err)
	}
	changed := false
	for _, l := range lines {
		ok, err := m.line(ctx, l)
		if err != nil {
			return false, err
		}
		changed = changed || ok
	}
	return changed, nil
}

// lockStatement takes the bank's statement lock and the statement's row,
// and checks its version against ifMatch, when given.
func lockStatement(ctx context.Context, tx pgx.Tx, s Statement, ifMatch func(int32) error) error {
	if err := lockStatements(ctx, tx, s.HouseBankID); err != nil {
		return err
	}
	var version int32
	if err := tx.QueryRow(ctx, `SELECT version FROM payments.bank_statement WHERE id = $1 FOR UPDATE`, s.ID).Scan(&version); err != nil {
		return fmt.Errorf("payments: lock statement %s: %w", s.ID, err)
	}
	if ifMatch == nil {
		return nil
	}
	return ifMatch(version)
}

// bumpStatement moves the statement's version on: its lines' matches
// changed, so a client's ETag of it no longer holds.
func bumpStatement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID) (int32, error) {
	var version int32
	if err := tx.QueryRow(ctx, `UPDATE payments.bank_statement SET version = version + 1, updated_by = $2 WHERE id = $1 RETURNING version`, id, p.ID).
		Scan(&version); err != nil {
		return 0, fmt.Errorf("payments: the version of statement %s: %w", id, err)
	}
	return version, nil
}

// statementCompany is the configuration of the statement's company; nil
// when the active configuration has none.
func (e *Endpoints) statementCompany(ctx context.Context, legalEntity uuid.UUID) (*config.Company, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, legalEntity) // none: no company of that code below
	if err != nil {
		return nil, fmt.Errorf("payments: legal entity %s of a bank statement: %w", legalEntity, err)
	}
	return doc.Company(le.Code), nil
}

// line tries the rules in order on one line; the first that finds
// candidates records its match.
func (m matcher) line(ctx context.Context, l matchLine) (bool, error) {
	next, ok, err := m.decide(ctx, l)
	if err != nil {
		return false, err
	}
	sig := ""
	if ok {
		sig = signature(next)
	}
	if l.Open != nil {
		if *l.Open == sig {
			return false, nil // the rules find what they found before
		}
		if err := rejectLive(ctx, m.tx, m.p, m.s, m.company, l.LineNo, rejection{}); err != nil {
			return false, err
		}
	}
	if !ok {
		return l.Open != nil, reopenLine(ctx, m.tx, m.p, m.s.ID, l)
	}
	_, err = writeMatch(ctx, m.tx, m.p, m.s, m.company, l.LineNo, next)
	return true, err
}

// decide is the match the first rule that finds candidates makes; false
// when none does.
func (m matcher) decide(ctx context.Context, l matchLine) (newMatch, bool, error) {
	for _, r := range m.rules {
		f, err := m.find(ctx, r, l)
		if err != nil {
			return newMatch{}, false, err
		}
		if len(f.candidates) == 0 {
			continue
		}
		status := MatchOpen
		if len(f.candidates) == 1 && f.settles {
			status = MatchMatched
		}
		rule := r.Kind
		return newMatch{kind: MatchRule, rule: &rule, reason: f.reason, status: status, candidates: f.candidates}, true, nil
	}
	return newMatch{}, false, nil
}

// signature is what a rule's match is compared by when the rules run
// again: its rule, status and targets, as the lines query writes an open
// match's.
func signature(n newMatch) string {
	var ts []string
	for _, c := range n.candidates {
		for _, t := range c {
			rule := ""
			if t.PostingRule != nil {
				rule = *t.PostingRule
			}
			ts = append(ts, fmt.Sprintf("%d:%s:%s:%s", t.Candidate, optID(t.PaymentID), optID(t.BankFileID), rule))
		}
	}
	return *n.rule + "|" + n.status + "|" + strings.Join(ts, " ")
}

func optID(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// reopenLine makes a line whose open match was rejected unmatched again,
// when the rules find nothing for it now; an unmatched line stays so.
func reopenLine(ctx context.Context, tx pgx.Tx, p reqctx.Principal, statement uuid.UUID, l matchLine) error {
	if l.Open == nil {
		return nil
	}
	return unmatchLine(ctx, tx, p, statement, l.LineNo)
}

// unmatchLine makes the line unmatched again: its live match was rejected.
func unmatchLine(ctx context.Context, tx pgx.Tx, p reqctx.Principal, statement uuid.UUID, lineNo int32) error {
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_statement_line SET status = 'unmatched', version = version + 1, updated_by = $3
		WHERE statement_id = $1 AND line_no = $2`, statement, lineNo, p.ID); err != nil {
		return fmt.Errorf("payments: reopen line %d of statement %s: %w", lineNo, statement, err)
	}
	return nil
}

// find runs one rule. The rules on payments read only debits (outgoing
// payments); reversals are left to people.
func (m matcher) find(ctx context.Context, r config.MatchingRule, l matchLine) (found, error) {
	if l.Reversal {
		return found{}, nil
	}
	switch r.Kind {
	case config.MatchTransactionCode:
		return m.byCode(l), nil
	case config.MatchEndToEndID:
		return m.byEndToEnd(ctx, l)
	case config.MatchBatchID:
		return m.byBatch(ctx, l)
	default: // config.MatchAmount, the one kind left after validation
		return m.byAmount(ctx, r, l)
	}
}

// eligible are the payments a line may clear: posted, from the statement's
// house bank and in its currency; taken when a line's match holds them.
// $1 is the house bank, $2 the currency.
const eligible = `WITH eligible AS (
	SELECT p.id, p.end_to_end_id, p.amount, p.bank_account_id, r.run_date, p.payment_run_id, p.house_bank_id, p.payment_method,
		EXISTS (SELECT FROM payments.statement_match_target t JOIN payments.statement_match sm ON sm.tenant_id = t.tenant_id AND sm.id = t.match_id
			WHERE t.tenant_id = p.tenant_id AND t.payment_id = p.id AND sm.status = 'matched') AS taken
	FROM payments.payment p JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
	WHERE coalesce(p.house_bank_id, r.house_bank_id) = $1 AND p.currency = $2 AND p.journal_entry_id IS NOT NULL)
`

// paid is a payment a rule found, with the bank file of a batch, and
// whether another line's match holds it.
type paid struct {
	File       *uuid.UUID
	ID         uuid.UUID
	EndToEndID string
	Amount     money.Minor
	Account    uuid.UUID
	Taken      bool
}

func (m matcher) payments(ctx context.Context, what, q string, args ...any) ([]paid, error) {
	rows, _ := m.tx.Query(ctx, eligible+q, append([]any{m.s.HouseBankID, m.s.Currency}, args...)...)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[paid])
	if err != nil {
		return nil, fmt.Errorf("payments: %s for statement %s: %w", what, m.s.ID, err)
	}
	return out, nil
}

// byEndToEnd finds the payments whose end-to-end ids the line's
// transactions carry (an MT940 owner's reference holds 16 characters, too
// few for ours). The line is settled when every id is a payment no other
// line holds and they pay its amount; a payment another line holds is
// still found, so the booking goes to a person rather than to the rules
// after.
func (m matcher) byEndToEnd(ctx context.Context, l matchLine) (found, error) {
	ids := l.ids(func(d LineDetail) []string { return []string{d.EndToEndID} })
	if l.Amount >= 0 || len(ids) == 0 {
		return found{}, nil
	}
	pays, err := m.payments(ctx, "payments by end-to-end id", `SELECT NULL::uuid, id, end_to_end_id, amount, bank_account_id, taken FROM eligible
		WHERE end_to_end_id = ANY($3) ORDER BY end_to_end_id`, ids)
	if err != nil || len(pays) == 0 {
		return found{}, err
	}
	total := sum(pays)
	return found{candidates: [][]MatchTarget{targets(1, pays)}, settles: len(pays) == len(ids) && total == -l.Amount && !anyTaken(pays),
		reason: fmt.Sprintf("End-to-end ids %s: %d of %d are payments, paying %s of %s.", strings.Join(ids, ", "), len(pays), len(ids),
			total.Plain(m.s.Exponent), (-l.Amount).Plain(m.s.Exponent))}, nil
}

// byBatch finds the house bank's bank files whose message id or payment
// information id the line carries, each a candidate with its payments. The
// line is settled by one file whose payments no other line holds and pay
// its amount.
func (m matcher) byBatch(ctx context.Context, l matchLine) (found, error) {
	ids := l.ids(func(d LineDetail) []string { return []string{d.PaymentInfoID, d.MessageID} })
	if l.Amount >= 0 || len(ids) == 0 {
		return found{}, nil
	}
	pays, err := m.payments(ctx, "bank files by message id", `SELECT f.id, p.id, p.end_to_end_id, p.amount, p.bank_account_id, p.taken
		FROM payments.bank_file f JOIN payments.payment_run r ON r.tenant_id = f.tenant_id AND r.id = f.payment_run_id
		JOIN eligible p ON p.payment_run_id = f.payment_run_id AND p.house_bank_id IS NOT DISTINCT FROM f.house_bank_id
			AND p.payment_method IS NOT DISTINCT FROM f.payment_method
		WHERE (f.message_id = ANY($3) OR f.message_id || '-1' = ANY($3)) AND coalesce(f.house_bank_id, r.house_bank_id) = $1
		ORDER BY f.id, p.end_to_end_id`, ids)
	if err != nil || len(pays) == 0 {
		return found{}, err
	}
	var f found
	for i, p := range pays {
		if i == 0 || *pays[i-1].File != *p.File {
			f.candidates = append(f.candidates, nil)
		}
		n := int16(len(f.candidates))
		f.candidates[n-1] = append(f.candidates[n-1], MatchTarget{Candidate: n, PaymentID: &pays[i].ID, BankFileID: pays[i].File})
	}
	total := sum(pays)
	f.settles = len(f.candidates) == 1 && total == -l.Amount && !anyTaken(pays)
	f.reason = fmt.Sprintf("Batch ids %s: %d bank files, paying %s of %s.", strings.Join(ids, ", "), len(f.candidates),
		total.Plain(m.s.Exponent), (-l.Amount).Plain(m.s.Exponent))
	return f, nil
}

// byCode finds the posting rules for the line's transaction codes, its own
// and its transactions'; one settles it.
func (m matcher) byCode(l matchLine) found {
	codes := l.ids(func(d LineDetail) []string { return []string{d.TransactionCode} })
	codes = append(codes, l.TransactionCode)
	var f found
	for _, r := range m.co.BankStatements.PostingRules {
		if slices.Contains(codes, r.TransactionCode) {
			n := int16(len(f.candidates) + 1)
			f.candidates = append(f.candidates, []MatchTarget{{Candidate: n, PostingRule: &r.TransactionCode}})
			f.reason += fmt.Sprintf(" Transaction code %s: posting rule %q to account %s.", r.TransactionCode, r.Description, r.Account)
		}
	}
	f.settles, f.reason = len(f.candidates) == 1, strings.TrimSpace(f.reason)
	return f
}

// byAmount finds the payments of the line's amount, within the tolerance
// (in the company's currency: a statement in another currency is matched
// exactly), run within the window of the booking date and, when the rule
// asks, paid to the counterparty's IBAN or name. Each is a candidate.
func (m matcher) byAmount(ctx context.Context, r config.MatchingRule, l matchLine) (found, error) {
	if l.Amount >= 0 {
		return found{}, nil
	}
	var tolerance money.Minor
	if m.s.Currency == m.co.Currency && r.AmountTolerance != "" {
		tolerance, _ = money.ParseDecimal(r.AmountTolerance, m.s.Exponent) // validated with the company's digits on activation
	}
	pays, err := m.payments(ctx, "payments by amount", `SELECT NULL::uuid, id, end_to_end_id, amount, bank_account_id, taken FROM eligible
		WHERE NOT taken AND amount BETWEEN $3::bigint - $4::bigint AND $3::bigint + $4::bigint AND run_date BETWEEN $5::date - $6::int AND $5::date + $6::int
		ORDER BY end_to_end_id`, -l.Amount, tolerance, l.BookingDate, r.DateWindowDays)
	if err == nil && r.Counterparty && len(pays) > 0 {
		pays, err = m.toCounterparty(ctx, l, pays)
	}
	if err != nil || len(pays) == 0 {
		return found{}, err
	}
	f := found{settles: len(pays) == 1, reason: fmt.Sprintf("Amount %s within %s, run within %d days of %s: %d payments.",
		(-l.Amount).Plain(m.s.Exponent), tolerance.Plain(m.s.Exponent), r.DateWindowDays, l.BookingDate.Format(time.DateOnly), len(pays))}
	for i := range pays {
		f.candidates = append(f.candidates, targets(int16(i+1), pays[i:i+1]))
	}
	return f, nil
}

// toCounterparty keeps the payments to the IBAN or the holder's name of a
// counterparty of the line.
func (m matcher) toCounterparty(ctx context.Context, l matchLine, pays []paid) ([]paid, error) {
	accounts := make([]uuid.UUID, len(pays))
	for i, p := range pays {
		accounts[i] = p.Account
	}
	details, err := m.e.Payees.BankDetails(ctx, accounts)
	if err != nil {
		return nil, fmt.Errorf("payments: payees' bank details for statement %s: %w", m.s.ID, err)
	}
	return slices.DeleteFunc(pays, func(p paid) bool {
		d := details[p.Account]
		return !slices.ContainsFunc(l.Details, func(c LineDetail) bool {
			return (c.CounterpartyIBAN != "" && fieldcrypt.Normalize(c.CounterpartyIBAN) == fieldcrypt.Normalize(d.Account)) ||
				(c.CounterpartyName != "" && strings.EqualFold(strings.TrimSpace(c.CounterpartyName), strings.TrimSpace(d.Holder)))
		})
	}), nil
}

// ids are the distinct non-empty values of the line's transactions, in
// their order.
func (l matchLine) ids(of func(LineDetail) []string) []string {
	var out []string
	for _, d := range l.Details {
		for _, v := range of(d) {
			if v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func anyTaken(pays []paid) bool {
	return slices.ContainsFunc(pays, func(p paid) bool { return p.Taken })
}

func sum(pays []paid) money.Minor {
	var total money.Minor
	for _, p := range pays {
		total += p.Amount // amounts of one statement's currency, far below the overflow
	}
	return total
}

func targets(candidate int16, pays []paid) []MatchTarget {
	out := make([]MatchTarget, len(pays))
	for i := range pays {
		out[i] = MatchTarget{Candidate: candidate, PaymentID: &pays[i].ID, BankFileID: pays[i].File}
	}
	return out
}

// newMatch is a match to record; an agent's names its session and what
// it proposes.
type newMatch struct {
	kind       string
	rule       *string
	reason     string
	status     string
	candidates [][]MatchTarget
	session    *uuid.UUID
	proposal   *string
}

// writeMatch records the match of the line with its targets, sets the
// line's status by it, audits it and publishes BankStatementLineMatched,
// BankStatementLineExcluded, or for an agent's BankStatementLineProposed,
// naming the statement's company by company (companyCode).
func writeMatch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement, company string, lineNo int32, m newMatch) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by,
			session_id, proposal)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10, $11)`, p.TenantID, id, s.ID, lineNo, m.kind, m.rule, m.reason, m.status, p.ID,
		m.session, m.proposal); err != nil {
		return uuid.Nil, fmt.Errorf("payments: record the match of line %d of statement %s: %w", lineNo, s.ID, err)
	}
	var cands, nos []int16
	var pays, files []*uuid.UUID
	var rules []*string
	var incoming []bool
	for _, c := range m.candidates {
		for i, t := range c {
			cands, nos, pays, files, rules = append(cands, t.Candidate), append(nos, int16(i+1)), append(pays, t.PaymentID), append(files, t.BankFileID), append(rules, t.PostingRule)
			incoming = append(incoming, t.Incoming)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, payment_id, bank_file_id, posting_rule,
			incoming, created_by)
		SELECT $1, $2, c, n, pay, file, rule, inc, $3 FROM unnest($4::smallint[], $5::smallint[], $6::uuid[], $7::uuid[], $8::text[], $9::boolean[])
			AS x(c, n, pay, file, rule, inc)`,
		p.TenantID, id, p.ID, cands, nos, pays, files, rules, incoming); err != nil {
		return uuid.Nil, fmt.Errorf("payments: record the targets of the match of line %d of statement %s: %w", lineNo, s.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_statement_line SET status = $3, version = version + 1, updated_by = $4 WHERE statement_id = $1 AND line_no = $2`,
		s.ID, lineNo, lineStatus[m.status], p.ID); err != nil {
		return uuid.Nil, fmt.Errorf("payments: set the status of line %d of statement %s: %w", lineNo, s.ID, err)
	}
	payload := map[string]any{"statement_match_id": id, "bank_statement_id": s.ID, "line_no": lineNo, "house_bank_id": s.HouseBankID,
		"legal_entity_id": s.LegalEntityID, "kind": m.kind, "rule": m.rule, "status": m.status, "candidates": len(m.candidates)}
	detail := map[string]any{"bank_statement_id": s.ID, "line_no": lineNo, "kind": m.kind, "rule": m.rule, "status": m.status, "reason": m.reason,
		"targets": m.candidates}
	name := map[bool]string{true: "bank_statement_line_excluded", false: "bank_statement_line_matched"}[m.status == MatchExcluded]
	if m.kind == MatchAgent {
		name, payload["session_id"], payload["proposal"], detail["session_id"], detail["proposal"] =
			"bank_statement_line_proposed", m.session, m.proposal, m.session, m.proposal
	}
	return id, record(ctx, "payments.statement_match.create", id, detail,
		outbox.Event{Context: "payments", Name: name, AggregateType: "payments.statement_match", AggregateID: id, Payload: withCompany(payload, company)})
}

// matchSelect reads matches with their targets as JSON.
const matchSelect = `SELECT m.id, m.statement_id, m.line_no, m.kind, m.rule, m.reason, m.status,
	coalesce((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object('candidate', t.candidate, 'payment_id', t.payment_id, 'bank_file_id', t.bank_file_id,
		'posting_rule', t.posting_rule, 'incoming', CASE WHEN t.incoming THEN true END)) ORDER BY t.candidate, t.target_no)
		FROM payments.statement_match_target t WHERE t.tenant_id = m.tenant_id AND t.match_id = m.id), '[]'),
	m.version, m.created_at, m.updated_at, m.created_by, m.updated_by, m.posting_date, m.posting_reason, m.approval_request_id,
	m.journal_entry_year, m.journal_entry_id, m.session_id, m.proposal, m.outcome FROM payments.statement_match m`

func scanMatch(row pgx.CollectableRow) (Match, error) {
	var m Match
	err := row.Scan(&m.ID, &m.StatementID, &m.LineNo, &m.Kind, &m.Rule, &m.Reason, &m.Status, &m.Targets, &m.Version, &m.CreatedAt, &m.UpdatedAt,
		&m.CreatedBy, &m.UpdatedBy, &m.PostingDate, &m.PostingReason, &m.ApprovalRequestID, &m.JournalEntryYear, &m.JournalEntryID,
		&m.SessionID, &m.Proposal, &m.Outcome)
	return m, errs.Wrapf(err, "scan statement match")
}

// statementMatches reads the statement's matches, live and rejected, by
// line and in the order they were made.
func statementMatches(ctx context.Context, tx pgx.Tx, statement uuid.UUID, lineNo int32) ([]Match, error) {
	rows, _ := tx.Query(ctx, matchSelect+` WHERE m.statement_id = $1 AND ($2 = 0 OR m.line_no = $2) ORDER BY m.line_no, m.id`, statement, lineNo)
	out, err := pgx.CollectRows(rows, scanMatch)
	if err != nil {
		return nil, fmt.Errorf("payments: matches of statement %s: %w", statement, err)
	}
	return append([]Match{}, out...), nil
}
