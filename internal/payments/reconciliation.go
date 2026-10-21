package payments

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Reconciliation (ADR-0047, T49h) compares a house bank's statements with
// the ledger. On a statement's closing date the bank's closing balance and
// the ledger's balance of the bank's main bank account differ by the lines
// of the bank's statements up to that date that the ledger does not hold
// yet: those excluded, those not posted (in transit) and those a person
// posted on a later date. What these do not explain is unexplained: an
// opening balance the ledger never received, or an entry on the bank
// account no statement line made. A statement becomes reconciled once
// nothing is in transit or unexplained on its closing date, which also
// means every line of it is posted or excluded: once, audited and
// published, and never undone by the server, so its lines are then decided
// for good. Everything is compared in the house bank's currency (ADR-0051
// §3, T53a): the statements' amounts, and the ledger's balance of the main
// bank account in it, its local amounts for a bank in the legal entity's
// currency and the transaction amounts of the entries in the bank's
// currency for a foreign-currency bank, whose exchange differences and
// valuations then never show as unexplained.

// Figures are a closing balance compared with the ledger on its date.
// Ledger is nil when the house bank has no main bank account.
type Figures struct {
	AsOf                             time.Time
	Closing                          money.Minor
	Ledger                           *money.Minor
	Excluded, InTransit, PostedLater money.Minor
}

// Unexplained is the difference between the bank's balance and the
// ledger's that the lines do not explain; nil without a ledger balance.
func (f Figures) Unexplained() *money.Minor {
	if f.Ledger == nil {
		return nil
	}
	u := f.Closing - *f.Ledger - f.Excluded - f.InTransit - f.PostedLater // balances of one account, far below the overflow
	return &u
}

// agrees reports whether the ledger agrees with the bank on the date:
// every line up to it is posted or excluded, and nothing is unexplained.
func (f Figures) agrees() bool {
	u := f.Unexplained()
	return u != nil && *u == 0 && f.InTransit == 0
}

// reconBank is what reconciliation reads of a house bank.
type reconBank struct {
	ID, LegalEntityID uuid.UUID
	Currency          string
	Exponent          int
	Main              *uuid.UUID // the main bank account; nil: the bank's statements never post
}

const reconBankSelect = `SELECT h.id, h.legal_entity_id, h.currency, c.exponent, h.main_gl_account_id
	FROM payments.house_bank h JOIN ref.currency c ON c.code = h.currency WHERE h.id = $1`

func scanReconBank(row pgx.Row) (reconBank, error) {
	var b reconBank
	err := row.Scan(&b.ID, &b.LegalEntityID, &b.Currency, &b.Exponent, &b.Main)
	return b, errs.Wrapf(err, "scan house bank")
}

// figures are the house bank's figures on each date, each with the
// closing balance given for it.
func (e *Endpoints) figures(ctx context.Context, tx pgx.Tx, b reconBank, dates []time.Time, closing []money.Minor) ([]Figures, error) {
	rows, _ := tx.Query(ctx, `WITH per AS (
			SELECT s.closing_date AS c, coalesce(sum(l.amount) FILTER (WHERE l.status = 'excluded'), 0) AS excluded,
				coalesce(sum(l.amount) FILTER (WHERE l.status NOT IN ('posted', 'excluded')), 0) AS in_transit
			FROM payments.bank_statement s
			LEFT JOIN payments.bank_statement_line l ON l.tenant_id = s.tenant_id AND l.statement_id = s.id
			WHERE s.house_bank_id = $1 GROUP BY s.id, s.closing_date),
		late AS (
			SELECT s.closing_date AS c, m.posting_date AS pd, l.amount FROM payments.bank_statement s
			JOIN payments.bank_statement_line l ON l.tenant_id = s.tenant_id AND l.statement_id = s.id AND l.status = 'posted'
			JOIN payments.statement_match m ON m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no AND m.status <> 'rejected'
			WHERE s.house_bank_id = $1 AND m.posting_date > s.closing_date)
		SELECT x.d, x.closing, NULL::bigint, coalesce((SELECT sum(excluded) FROM per WHERE c <= x.d), 0)::bigint,
			coalesce((SELECT sum(in_transit) FROM per WHERE c <= x.d), 0)::bigint, coalesce((SELECT sum(amount) FROM late WHERE c <= x.d AND pd > x.d), 0)::bigint
		FROM unnest($2::date[], $3::bigint[]) WITH ORDINALITY AS x(d, closing, n) ORDER BY x.n`, b.ID, dates, closing)
	figs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Figures])
	if err != nil {
		return nil, fmt.Errorf("payments: reconciliation figures of house bank %s: %w", b.ID, err)
	}
	if b.Main == nil {
		return figs, nil
	}
	ledger, err := e.Accounts.Balances(ctx, b.LegalEntityID, *b.Main, b.Currency, dates)
	if err != nil {
		return nil, fmt.Errorf("payments: ledger balances of house bank %s: %w", b.ID, err)
	}
	for i := range figs {
		figs[i].Ledger = &ledger[i]
	}
	return figs, nil
}

// reconcile reconciles the house bank's statements that now are: imported
// (not in gap), every line posted or excluded, and the ledger agreeing on
// the closing date. Its callers hold the bank's statement lock and have
// just changed a line of s, or run the rules on it.
func (e *Endpoints) reconcile(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s Statement) error {
	rows, _ := tx.Query(ctx, statementSelect+` WHERE s.house_bank_id = $1 AND s.status = 'imported' AND NOT EXISTS (SELECT FROM payments.bank_statement_line l
		WHERE l.tenant_id = s.tenant_id AND l.statement_id = s.id AND l.status NOT IN ('posted', 'excluded')) ORDER BY s.closing_date, s.id`, s.HouseBankID)
	done, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) { return scanStatement(r) })
	if err != nil {
		return fmt.Errorf("payments: statements of house bank %s to reconcile: %w", s.HouseBankID, err)
	}
	if len(done) == 0 {
		return nil
	}
	b, err := scanReconBank(tx.QueryRow(ctx, reconBankSelect, s.HouseBankID))
	if err != nil {
		return fmt.Errorf("payments: house bank %s to reconcile: %w", s.HouseBankID, err)
	}
	dates, closing := make([]time.Time, len(done)), make([]money.Minor, len(done))
	for i, d := range done {
		dates[i], closing[i] = d.ClosingDate, d.Closing
	}
	figs, err := e.figures(ctx, tx, b, dates, closing)
	if err != nil {
		return err
	}
	agreed := map[uuid.UUID]int{}
	for i, d := range done {
		if figs[i].agrees() {
			agreed[d.ID] = i
		}
	}
	return reconcileStatements(ctx, tx, p, done, figs, agreed)
}

// reconcileStatements sets the statements agreed reconciled, audits each
// and publishes BankStatementReconciled, once: the update takes only a
// statement still imported, so a statement reconciled before records
// nothing again.
func reconcileStatements(ctx context.Context, tx pgx.Tx, p reqctx.Principal, done []Statement, figs []Figures, agreed map[uuid.UUID]int) error {
	ids := make([]uuid.UUID, 0, len(agreed))
	for id := range agreed {
		ids = append(ids, id)
	}
	rows, _ := tx.Query(ctx, `UPDATE payments.bank_statement SET status = 'reconciled', reconciled_at = now(), reconciled_by = $2,
		version = version + 1, updated_by = $2 WHERE id = ANY($1) AND status = 'imported' RETURNING id`, ids, p.ID)
	set, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("payments: reconcile statements %v: %w", ids, err)
	}
	for _, id := range set {
		i := agreed[id]
		if err := recordReconciled(ctx, done[i], figs[i]); err != nil {
			return err
		}
	}
	return nil
}

// recordReconciled audits the statement's reconciliation and publishes
// BankStatementReconciled; agreed figures have a ledger balance.
func recordReconciled(ctx context.Context, s Statement, f Figures) error {
	amount := func(m money.Minor) map[string]string { return money.EventAmount(m, s.Currency, s.Exponent) }
	day := s.ClosingDate.Format(time.DateOnly)
	return record(ctx, "payments.bank_statement.reconcile", s.ID, map[string]any{"closing_date": day, "closing_balance": amount(s.Closing),
		"ledger_balance": amount(*f.Ledger), "excluded": amount(f.Excluded), "posted_later": amount(f.PostedLater)},
		outbox.Event{Context: "payments", Name: "bank_statement_reconciled", AggregateType: "payments.bank_statement", AggregateID: s.ID,
			Payload: map[string]any{"bank_statement_id": s.ID, "house_bank_id": s.HouseBankID, "legal_entity_id": s.LegalEntityID,
				"closing_date": day, "closing_balance": amount(s.Closing), "ledger_balance": amount(*f.Ledger), "excluded": amount(f.Excluded),
				"posted_later": amount(f.PostedLater), "line_count": s.LineCount, "status": StatementReconciled}})
}

// lineStatuses are the statuses of statement lines.
var lineStatuses = []string{"unmatched", "proposed", MatchMatched, MatchExcluded, "pending_approval", "posted"}

// LineCounts are lines by status.
type LineCounts map[string]int32

// ReconciledLine is a line of the report with its live match and the
// outcome of the latest agent proposal a person decided.
type ReconciledLine struct {
	StatementID       uuid.UUID
	LineNo            int32
	Amount            money.Minor
	Reversal          bool
	BookingDate       time.Time
	TransactionCode   string
	Status            string
	MatchID           *uuid.UUID
	MatchKind         *string
	MatchRule         *string
	MatchStatus       *string
	Incoming          bool
	PostingDate       *time.Time
	JournalEntryYear  *int16
	JournalEntryID    *uuid.UUID
	ApprovalRequestID *uuid.UUID
	ProposalOutcome   *string
}

// ReconciledStatement is a statement of the report with its figures, its
// lines' counts and its lines (of the status asked for).
type ReconciledStatement struct {
	Statement Statement
	Figures   Figures
	Counts    LineCounts
	Lines     []ReconciledLine
}

// ReconciliationSummary is the period's: its statements, the first one's
// opening balance, the figures on its last closing date (nil without
// statements) and the lines by status.
type ReconciliationSummary struct {
	Statements  int32
	Opening     *money.Minor
	OpeningDate *time.Time
	Figures     *Figures
	Counts      LineCounts
}

// Reconciliation is a page of a house bank's reconciliation report.
type Reconciliation struct {
	HouseBankID, LegalEntityID uuid.UUID
	Currency                   string
	Exponent                   int
	From, To                   *time.Time
	Summary                    ReconciliationSummary
	Statements                 []ReconciledStatement
	NextPageToken              string
}

// ReconciliationQuery is what a report covers: the statements closing
// from From to To (nil: open), the lines of LineStatus ("" all), the page.
type ReconciliationQuery struct {
	From, To   *time.Time
	LineStatus string
	Page       *pagination.Keyset
}

// BankReconciliation is the reconciliation report of a house bank of a
// legal entity the caller may access: one page of its statements closing
// in the period, by closing date, with the period's summary.
func (e *Endpoints) BankReconciliation(ctx context.Context, houseBank uuid.UUID, q ReconciliationQuery) (Reconciliation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Reconciliation{}, err
	}
	if q.LineStatus != "" && !slices.Contains(lineStatuses, q.LineStatus) {
		return Reconciliation{}, problem.Unprocessable("request.validation_failed", "line_status is not a line status.",
			problem.FieldError{Field: "line_status", Code: "invalid", Message: strings.Join(lineStatuses, ", ")})
	}
	if q.From != nil && q.To != nil && q.To.Before(*q.From) {
		return Reconciliation{}, problem.Unprocessable("request.validation_failed", "The period ends before it starts.",
			problem.FieldError{Field: "closing_date_to", Code: "invalid", Message: "on or after closing_date_from"})
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Reconciliation{}, err
	}
	b, err := scanReconBank(tx.QueryRow(ctx, reconBankSelect, houseBank))
	if err = authz.Visible(err, access, b.LegalEntityID, problem.NotFoundOf("house bank"), "payments: house bank %s to reconcile", houseBank); err != nil {
		return Reconciliation{}, err
	}
	r := Reconciliation{HouseBankID: b.ID, LegalEntityID: b.LegalEntityID, Currency: b.Currency, Exponent: b.Exponent, From: q.From, To: q.To}
	if r.Summary, err = e.periodSummary(ctx, tx, b, q); err != nil {
		return Reconciliation{}, err
	}
	statements, next, err := periodStatements(ctx, tx, b, q)
	if err != nil {
		return Reconciliation{}, err
	}
	r.NextPageToken = next
	r.Statements, err = e.reconciledStatements(ctx, tx, b, statements, q.LineStatus)
	return r, err
}

// periodFilter is the statements of the bank ($1) closing from $2 to $3.
const periodFilter = ` WHERE s.house_bank_id = $1 AND ($2::date IS NULL OR s.closing_date >= $2) AND ($3::date IS NULL OR s.closing_date <= $3)`

// periodSummary is the period's summary: its statements, its first
// opening balance, its figures on its last closing date and its lines.
func (e *Endpoints) periodSummary(ctx context.Context, tx pgx.Tx, b reconBank, q ReconciliationQuery) (ReconciliationSummary, error) {
	out := ReconciliationSummary{Counts: LineCounts{}}
	var last *time.Time
	var closing *money.Minor
	if err := tx.QueryRow(ctx, `SELECT count(*)::int, (array_agg(s.opening_balance ORDER BY s.closing_date, s.id))[1],
			(array_agg(s.opening_date ORDER BY s.closing_date, s.id))[1], (array_agg(s.closing_balance ORDER BY s.closing_date DESC, s.id DESC))[1],
			max(s.closing_date) FROM payments.bank_statement s`+periodFilter, b.ID, q.From, q.To).
		Scan(&out.Statements, &out.Opening, &out.OpeningDate, &closing, &last); err != nil {
		return out, fmt.Errorf("payments: reconciliation period of house bank %s: %w", b.ID, err)
	}
	rows, _ := tx.Query(ctx, `SELECT l.status, count(*)::int FROM payments.bank_statement s
		JOIN payments.bank_statement_line l ON l.tenant_id = s.tenant_id AND l.statement_id = s.id`+periodFilter+` GROUP BY l.status`, b.ID, q.From, q.To)
	var status string
	var n int32
	if _, err := pgx.ForEachRow(rows, []any{&status, &n}, func() error { out.Counts[status] = n; return nil }); err != nil {
		return out, fmt.Errorf("payments: reconciliation lines of house bank %s: %w", b.ID, err)
	}
	if last == nil {
		return out, nil
	}
	figs, err := e.figures(ctx, tx, b, []time.Time{*last}, []money.Minor{*closing})
	if err != nil {
		return out, err
	}
	out.Figures = &figs[0]
	return out, nil
}

// periodStatements is the page of the period's statements, by closing
// date, and the token of the next page.
func periodStatements(ctx context.Context, tx pgx.Tx, b reconBank, q ReconciliationQuery) ([]Statement, string, error) {
	sql, args := q.Page.SQL(statementSelect+periodFilter, []any{b.ID, q.From, q.To})
	rows, _ := tx.Query(ctx, sql, args...)
	statements, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) { return scanStatement(r) })
	if err != nil {
		return nil, "", fmt.Errorf("payments: reconciliation statements of house bank %s: %w", b.ID, err)
	}
	statements, next := pagination.Page(q.Page, statements, func(s Statement) (string, uuid.UUID) { return s.ClosingDate.Format(time.DateOnly), s.ID })
	return statements, next, nil
}

// reconciledStatements are the statements with their figures, their
// lines' counts and their lines of the status ("" all).
func (e *Endpoints) reconciledStatements(ctx context.Context, tx pgx.Tx, b reconBank, statements []Statement, lineStatus string) ([]ReconciledStatement, error) {
	ids, dates, closing := make([]uuid.UUID, len(statements)), make([]time.Time, len(statements)), make([]money.Minor, len(statements))
	for i, s := range statements {
		ids[i], dates[i], closing[i] = s.ID, s.ClosingDate, s.Closing
	}
	figs, err := e.figures(ctx, tx, b, dates, closing)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.statement_id, l.line_no, l.amount, l.is_reversal, l.booking_date, l.transaction_code, l.status,
			m.id, m.kind, m.rule, m.status, coalesce((SELECT bool_or(t.incoming) FROM payments.statement_match_target t
				WHERE t.tenant_id = m.tenant_id AND t.match_id = m.id), false),
			m.posting_date, m.journal_entry_year, m.journal_entry_id, m.approval_request_id,
			(SELECT a.outcome FROM payments.statement_match a WHERE a.tenant_id = l.tenant_id AND a.statement_id = l.statement_id
				AND a.line_no = l.line_no AND a.outcome IS NOT NULL ORDER BY a.updated_at DESC, a.id DESC LIMIT 1)
		FROM payments.bank_statement_line l
		LEFT JOIN payments.statement_match m ON m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no AND m.status <> 'rejected'
		WHERE l.statement_id = ANY($1) ORDER BY l.statement_id, l.line_no`, ids)
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ReconciledLine])
	if err != nil {
		return nil, fmt.Errorf("payments: reconciliation lines of house bank %s: %w", b.ID, err)
	}
	out := make([]ReconciledStatement, len(statements))
	at := make(map[uuid.UUID]int, len(statements))
	for i, s := range statements {
		out[i] = ReconciledStatement{Statement: s, Figures: figs[i], Counts: LineCounts{}, Lines: []ReconciledLine{}}
		at[s.ID] = i
	}
	for _, l := range lines {
		r := &out[at[l.StatementID]]
		r.Counts[l.Status]++
		if lineStatus == "" || l.Status == lineStatus {
			r.Lines = append(r.Lines, l)
		}
	}
	return out, nil
}
