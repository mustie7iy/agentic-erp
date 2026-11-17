package payables

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Expense report statuses (ADR-0074): entered, then posted once.
const (
	ExpenseEntered = "entered"
	ExpensePosted  = "posted"
)

// ExpenseNumbers is the number range object expense reports draw their
// numbers from (configuration number_ranges).
const ExpenseNumbers = "expense_report"

// ExpenseLedger is what payables reads of the ledger for expense reports
// (ledger.AssetReads): the ids of the company's accounts an expense
// type's number names, and the numbers and names of the accounts and
// cost centers a report's lines name.
type ExpenseLedger interface {
	Accounts(ctx context.Context, legalEntity uuid.UUID, numbers []string) (map[string]uuid.UUID, error)
	AccountNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.AccountRef, error)
	CenterNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.CenterName, error)
}

// PartnerNames reads business partners' numbers and names (partner
// context, partner.Payees): an expense report's employee.
type PartnerNames interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// ExpenseLine is a line of an expense report: an expense of one day and
// type, its gross amount with the tax its code takes out of it at the
// line's own date (product owner 2026-11-13), on the type's expense
// account as it was when the report was entered.
type ExpenseLine struct {
	LineNo       int32        `json:"line_no"`
	ExpenseDate  partner.Date `json:"expense_date"`
	ExpenseType  string       `json:"expense_type"`
	AccountID    uuid.UUID    `json:"account_id"`
	Amount       money.Minor  `json:"amount"`
	TaxCode      *string      `json:"tax_code"`
	TaxAmount    money.Minor  `json:"tax_amount"`
	CostCenterID *uuid.UUID   `json:"cost_center_id"`
	Description  *string      `json:"description"`
}

// ExpenseReport is a row of payables.expense_report with its lines
// (ADR-0074). Amounts are minor units of Currency, the company's, whose
// minor-unit digits are Exponent.
type ExpenseReport struct {
	ID               uuid.UUID
	DocumentNumber   string
	LegalEntityID    uuid.UUID
	EmployeeID       uuid.UUID
	ReportDate       time.Time
	Currency         string
	Exponent         int
	Description      *string
	Status           string
	PostingDate      *time.Time
	JournalEntryYear *int16
	JournalEntryID   *uuid.UUID
	PaymentTermCode  *string
	NetDueDate       *time.Time
	Lines            []ExpenseLine
	Version          int32
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
}

// Totals are the report's gross amount, the tax its lines hold and its
// net amount. The plain additions cannot overflow: entering the report
// checked its lines' amounts add up within range (sumLines, money.Sum),
// and each line's tax is less than its amount (expense_report_line_tax,
// 000329), so the tax adds up below the gross amount.
func (r ExpenseReport) Totals() (gross, tax, net money.Minor) {
	for _, l := range r.Lines {
		gross += l.Amount
		tax += l.TaxAmount
	}
	return gross, tax, gross - tax
}

const expenseSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.employee_id, r.report_date, r.currency, c.exponent, r.description, r.status,
		r.posting_date, r.journal_entry_year, r.journal_entry_id, r.payment_term_code, r.net_due_date,
		(SELECT json_agg(json_build_object('line_no', l.line_no, 'expense_date', l.expense_date, 'expense_type', l.expense_type, 'account_id', l.account_id,
			'amount', l.amount, 'tax_code', l.tax_code, 'tax_amount', l.tax_amount, 'cost_center_id', l.cost_center_id, 'description', l.description)
			ORDER BY l.line_no)
		 FROM payables.expense_report_line l WHERE l.tenant_id = r.tenant_id AND l.expense_report_id = r.id),
		r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM payables.expense_report r JOIN ref.currency c ON c.code = r.currency`

func scanExpense(row pgx.Row) (ExpenseReport, error) {
	var r ExpenseReport
	err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.EmployeeID, &r.ReportDate, &r.Currency, &r.Exponent, &r.Description, &r.Status,
		&r.PostingDate, &r.JournalEntryYear, &r.JournalEntryID, &r.PaymentTermCode, &r.NetDueDate, &r.Lines, &r.Version, &r.CreatedAt, &r.CreatedBy,
		&r.UpdatedAt, &r.UpdatedBy)
	return r, errs.Wrapf(err, "scan expense report")
}

// expenseReport reads an expense report of a legal entity the caller may
// act in, lock under its row lock (ADR-0008); another's is not found, so
// ids cannot be probed (04 §4.9).
func expenseReport(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (ExpenseReport, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return ExpenseReport{}, err
	}
	q := expenseSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanExpense(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("expense report"), "payables: read expense report %s", id); err != nil {
		return ExpenseReport{}, err
	}
	return r, nil
}

// GetExpenseReport returns an expense report.
func GetExpenseReport(ctx context.Context, id uuid.UUID) (ExpenseReport, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return ExpenseReport{}, err
	}
	return expenseReport(ctx, tx, id, false)
}

// ExpenseLineInput is an expense as a client sends it.
type ExpenseLineInput struct {
	ExpenseDate  time.Time
	ExpenseType  string
	Amount       Amount
	TaxCode      *string
	CostCenterID *uuid.UUID
	Description  *string
}

// ExpenseInput is an expense report as a client sends it.
type ExpenseInput struct {
	LegalEntityID uuid.UUID
	EmployeeID    uuid.UUID
	Description   *string
	Lines         []ExpenseLineInput
}

// expenseEntry is what entering a report has established: its company
// and the company's configuration (nil without one), its date, currency
// and minor-unit digits, and its lines as they are stored.
type expenseEntry struct {
	le    tenancy.Entity
	co    *config.Company
	day   time.Time
	exp   int
	lines []ExpenseLine
}

// EnterExpenseReport enters an employee's expense report (ADR-0074): the
// employee a supplier usable in the company, as a supplier invoice's;
// each line dated not after the company's date today, of an expense type
// of the company whose account is postable there, a gross amount above
// zero in the company's currency, an input tax code of the company on
// the line's date, whose tax is taken out of the gross at that date's
// rates (expenseTax), and
// a cost center the ledger accepts on the report date. The report is
// entered, dated the company's date today and numbered from the
// company's expense_report range of that date's fiscal year; its audit
// record and ExpenseReportChanged are written in the request's
// transaction. Nothing is posted.
func (e *Endpoints) EnterExpenseReport(ctx context.Context, in ExpenseInput) (ExpenseReport, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ExpenseReport{}, err
	}
	if in.Description != nil {
		if err := problem.RequireText("description", *in.Description); err != nil {
			return ExpenseReport{}, err
		}
	}
	x, err := e.checkExpenses(ctx, tx, in)
	if err != nil {
		return ExpenseReport{}, err
	}
	year, err := e.Calendar.FiscalYear(ctx, x.le.ID, x.day)
	if err != nil {
		return ExpenseReport{}, err
	}
	number, err := e.Numbers.Next(ctx, x.le.ID, ExpenseNumbers, year)
	if err != nil {
		return ExpenseReport{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := insertExpense(ctx, tx, p, id, number, in, x); err != nil {
		return ExpenseReport{}, err
	}
	r, err := expenseReport(ctx, tx, id, false)
	if err != nil {
		return ExpenseReport{}, err
	}
	return r, e.recordExpense(ctx, "payables.expense_report.create", r, x.le.Code, nil)
}

// checkExpenses checks a report's parties and lines (EnterExpenseReport).
func (e *Endpoints) checkExpenses(ctx context.Context, tx pgx.Tx, in ExpenseInput) (expenseEntry, error) {
	le, _, err := e.parties(ctx, in.LegalEntityID, in.EmployeeID, "employee_id", "The employee cannot be paid: ")
	if err != nil {
		return expenseEntry{}, err
	}
	co, err := e.company(ctx, le.Code)
	if err != nil {
		return expenseEntry{}, err
	}
	day, err := e.companyDay(ctx, le.ID)
	if err != nil {
		return expenseEntry{}, err
	}
	exp, err := currencyExponent(ctx, tx, le.LocalCurrency, "lines[0].amount.currency")
	if err != nil {
		return expenseEntry{}, err
	}
	x := expenseEntry{le: le, co: co, day: day, exp: exp}
	v := &invoiceCheck{e: e, ctx: ctx, tx: tx, le: le, exp: exp, c: &checked{currency: le.LocalCurrency}}
	numbers := make([]string, len(in.Lines))
	for i, l := range in.Lines {
		line, number, err := x.line(v, i, l)
		if err != nil {
			return expenseEntry{}, err
		}
		x.lines, numbers[i] = append(x.lines, line), number
	}
	if err := e.expenseAccounts(ctx, le.ID, numbers, x.lines); err != nil {
		return expenseEntry{}, err
	}
	if err := e.expenseAssignments(ctx, le.ID, day, x.lines); err != nil {
		return expenseEntry{}, err
	}
	if _, err := sumLines(x.lines); err != nil {
		return expenseEntry{}, err
	}
	return x, e.expenseTax(ctx, x.report(), x.lines)
}

// report is the entry as a stored report reads, for the tax calculation.
func (x expenseEntry) report() ExpenseReport {
	return ExpenseReport{LegalEntityID: x.le.ID, ReportDate: x.day, Currency: x.le.LocalCurrency, Exponent: x.exp, Lines: x.lines}
}

// line checks one line and returns it with the number of its expense
// type's account, which expenseAccounts resolves.
func (x expenseEntry) line(v *invoiceCheck, i int, in ExpenseLineInput) (ExpenseLine, string, error) {
	field := fmt.Sprintf("lines[%d]", i)
	if in.ExpenseDate.After(x.day) {
		return ExpenseLine{}, "", problem.Invalid("payables.expense_date_in_future", field+".expense_date",
			"An expense is dated on or before the company's date today, "+x.day.Format(time.DateOnly)+".")
	}
	if in.Description != nil {
		if err := problem.RequireText(field+".description", *in.Description); err != nil {
			return ExpenseLine{}, "", err
		}
	}
	var t config.ExpenseType
	ok := false
	if x.co != nil {
		t, ok = x.co.ExpenseType(in.ExpenseType)
	}
	if !ok {
		return ExpenseLine{}, "", problem.Invalid("payables.unknown_expense_type", field+".expense_type",
			"Expense type "+in.ExpenseType+" is not configured for company "+x.le.Code+" (companies[].expense_types).")
	}
	amount, err := v.parse(in.Amount, field+".amount")
	if err != nil {
		return ExpenseLine{}, "", err
	}
	if amount <= 0 {
		return ExpenseLine{}, "", problem.Unprocessable("payables.invalid_amount", "An expense is more than zero.",
			problem.FieldError{Field: field + ".amount.amount", Code: "not_positive", Message: "an amount above 0"})
	}
	v.taxDate = in.ExpenseDate // a line's tax is its expense's date's (product owner 2026-11-13)
	if err := v.taxCode(field, in.TaxCode); err != nil {
		return ExpenseLine{}, "", err
	}
	return ExpenseLine{LineNo: int32(i + 1), ExpenseDate: partner.Date{Time: in.ExpenseDate}, ExpenseType: t.Code, Amount: amount,
		TaxCode: in.TaxCode, CostCenterID: in.CostCenterID, Description: in.Description}, t.Account, nil
}

// expenseAccounts sets each line's account: its expense type's, by
// number, which must be set up and postable in the company (422
// payables.account_not_postable naming the line's expense type); the
// configuration checked it is an expense account of the chart.
func (e *Endpoints) expenseAccounts(ctx context.Context, le uuid.UUID, numbers []string, lines []ExpenseLine) error {
	ids, err := e.ExpenseLedger.Accounts(ctx, le, numbers)
	if err != nil {
		return err
	}
	for i := range lines {
		id, ok := ids[numbers[i]]
		if ok {
			if ok, err = e.Accounts.IsPostable(ctx, le, id); err != nil {
				return err
			}
		}
		if !ok {
			return problem.Invalid("payables.account_not_postable", fmt.Sprintf("lines[%d].expense_type", i),
				"Account "+numbers[i]+" of expense type "+lines[i].ExpenseType+" cannot be posted in this legal entity.")
		}
		lines[i].AccountID = id
	}
	return nil
}

// expenseAssignments checks the lines' cost centers as the posting will
// on the report date (ledger.CheckAssignments, ADR-0050 §6): a line
// without one takes its account's default. The ledger's refusal names
// items[i]; a report's are lines[i].
func (e *Endpoints) expenseAssignments(ctx context.Context, le uuid.UUID, day time.Time, lines []ExpenseLine) error {
	items := make([]ledger.ItemAssignment, len(lines))
	for i, l := range lines {
		items[i] = ledger.ItemAssignment{Index: i, AccountID: l.AccountID, CostCenterID: l.CostCenterID}
	}
	err := e.Accounts.CheckAssignments(ctx, le, day, items, true)
	var p *problem.Problem
	if errors.As(err, &p) {
		for k := range p.Errors {
			if rest, ok := strings.CutPrefix(p.Errors[k].Field, "items["); ok {
				p.Errors[k].Field = "lines[" + rest
			}
		}
		if rest, ok := strings.CutPrefix(p.Detail, "Item "); ok {
			p.Detail = "Line " + rest
		}
	}
	return err
}

// sumLines is the lines' gross amounts added up; out of range, 422
// payables.amount_too_large.
func sumLines(lines []ExpenseLine) (money.Minor, error) {
	amounts := make([]money.Minor, len(lines))
	for i, l := range lines {
		amounts[i] = l.Amount
	}
	total, err := money.Sum(amounts...)
	if err != nil {
		return 0, problem.Unprocessable("payables.amount_too_large", "The expense report's lines add up to more than can be posted.")
	}
	return total, nil
}

// insertExpense writes an entered report and its lines.
func insertExpense(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, number string, in ExpenseInput, x expenseEntry) error {
	if _, err := tx.Exec(ctx, `INSERT INTO payables.expense_report (tenant_id, id, document_number, legal_entity_id, employee_id, report_date, currency,
			description, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		p.TenantID, id, number, x.le.ID, in.EmployeeID, x.day, x.le.LocalCurrency, in.Description, p.ID); err != nil {
		return fmt.Errorf("payables: insert expense report %s: %w", number, err)
	}
	n := len(x.lines)
	nos, dates, types, accounts := make([]int32, n), make([]time.Time, n), make([]string, n), make([]uuid.UUID, n)
	amounts, codes, taxes, centers, texts := make([]money.Minor, n), make([]*string, n), make([]money.Minor, n), make([]*uuid.UUID, n), make([]*string, n)
	for i, l := range x.lines {
		nos[i], dates[i], types[i], accounts[i] = l.LineNo, l.ExpenseDate.Time, l.ExpenseType, l.AccountID
		amounts[i], codes[i], taxes[i], centers[i], texts[i] = l.Amount, l.TaxCode, l.TaxAmount, l.CostCenterID, l.Description
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.expense_report_line (tenant_id, expense_report_id, line_no, expense_date, expense_type, account_id,
			amount, tax_code, tax_amount, cost_center_id, description)
		SELECT $1, $2, n, d, t, a, m, c, x, cc, s
		FROM unnest($3::int[], $4::date[], $5::text[], $6::uuid[], $7::bigint[], $8::text[], $9::bigint[], $10::uuid[], $11::text[])
			AS u(n, d, t, a, m, c, x, cc, s)`,
		p.TenantID, id, nos, dates, types, accounts, amounts, codes, taxes, centers, texts); err != nil {
		return fmt.Errorf("payables: insert the lines of expense report %s: %w", number, err)
	}
	return nil
}

// recordExpense audits a report's change and publishes
// ExpenseReportChanged with its totals, naming the employee by id and
// number, never by name (product owner 2026-11-05); before is its status
// before, nil when it was entered.
func (e *Endpoints) recordExpense(ctx context.Context, action string, r ExpenseReport, companyCode string, before *string) error {
	names, err := e.Partners.Names(ctx, []uuid.UUID{r.EmployeeID})
	if err != nil {
		return fmt.Errorf("payables: employee of expense report %s: %w", r.ID, err)
	}
	gross, tax, _ := r.Totals()
	date := func(d *time.Time) *string {
		if d == nil {
			return nil
		}
		s := d.Format(time.DateOnly)
		return &s
	}
	payload := map[string]any{"expense_report_id": r.ID, "document_number": r.DocumentNumber, "legal_entity_id": r.LegalEntityID,
		"legal_entity_code": companyCode, "employee_id": r.EmployeeID, "employee_number": names[r.EmployeeID].Number,
		"report_date": r.ReportDate.Format(time.DateOnly), "gross_amount": money.EventAmount(gross, r.Currency, r.Exponent),
		"tax_amount": money.EventAmount(tax, r.Currency, r.Exponent), "posting_date": date(r.PostingDate), "journal_entry_id": r.JournalEntryID,
		"journal_entry_year": r.JournalEntryYear, "net_due_date": date(r.NetDueDate), "status": r.Status, "status_before": before, "version": r.Version}
	detail := map[string]any{"document_number": r.DocumentNumber, "status": r.Status, "gross_amount": gross.Plain(r.Exponent), "currency": r.Currency}
	if r.JournalEntryID != nil {
		detail["journal_entry_id"] = *r.JournalEntryID
	}
	return audit.Record(ctx, action, "payables.expense_report", r.ID, detail,
		outbox.Event{Context: "payables", Name: "expense_report_changed", AggregateType: "payables.expense_report", AggregateID: r.ID, Payload: payload})
}
