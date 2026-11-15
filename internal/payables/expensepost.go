package payables

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// expenseCalculation is a report's tax as the calculator gives it: the
// coded lines' indexes in the report's lines, each line's tax in that
// order, and the items of each of the lines' dates.
type expenseCalculation struct {
	coded []int
	lines []tax.LineTax
	days  []expenseDay
}

// expenseDay is the tax items of the coded lines of one expense date,
// the date their rates apply on and they are reported in.
type expenseDay struct {
	day   time.Time
	items []tax.ComponentTax
}

// calculateExpenses calculates the tax the gross amounts of a report's
// coded lines hold (tax.Document.Inclusive), each line at its own date,
// its expense's (product owner 2026-11-13): one calculation per date, at
// its codes' rates then and the company's rounding, with its local
// amounts at a rate of 1, a report being in the company's currency. A
// code that is no input tax code of the company on its line's date is
// refused naming the line, as entering it is.
func (e *Endpoints) calculateExpenses(ctx context.Context, r ExpenseReport) (expenseCalculation, error) {
	var out expenseCalculation
	byDay := map[string][]int{} // a date's coded lines, by index
	for i, l := range r.Lines {
		if l.TaxCode != nil {
			out.coded = append(out.coded, i)
			d := l.ExpenseDate.Format(time.DateOnly)
			byDay[d] = append(byDay[d], i)
		}
	}
	if len(out.coded) == 0 {
		return out, nil
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a report's legal entity exists
	if err != nil {
		return out, err
	}
	co, err := e.company(ctx, le.Code)
	if err != nil {
		return out, err
	}
	lines := map[int]tax.LineTax{}
	for _, d := range slices.Sorted(maps.Keys(byDay)) { // ISO dates sort as the days
		day := r.Lines[byDay[d][0]].ExpenseDate.Time
		res, err := e.calculateExpenseDay(ctx, r, day, byDay[d], settingsOf(co))
		if err != nil {
			return out, err
		}
		for k, i := range byDay[d] {
			lines[i] = res.Lines[k]
		}
		out.days = append(out.days, expenseDay{day: day, items: res.Items})
	}
	for _, i := range out.coded {
		out.lines = append(out.lines, lines[i])
	}
	return out, nil
}

// calculateExpenseDay calculates the tax of a report's coded lines dated
// day, given by their indexes, at their codes' rates on that day.
func (e *Endpoints) calculateExpenseDay(ctx context.Context, r ExpenseReport, day time.Time, coded []int, settings config.TaxSettings) (tax.Result, error) {
	doc := tax.Document{TaxDate: day, Currency: r.Currency, Inclusive: true, Codes: map[string]tax.Code{},
		Rounding: tax.Rounding(settings.Rounding), Local: &tax.Conversion{Rate: "1", FromExp: r.Exponent, ToExp: r.Exponent}}
	for _, i := range coded {
		code := *r.Lines[i].TaxCode
		doc.Lines = append(doc.Lines, tax.Line{Amount: r.Lines[i].Amount, Code: code})
		if _, ok := doc.Codes[code]; ok {
			continue
		}
		t, ok, err := e.Accounts.TaxCode(ctx, r.LegalEntityID, code, day)
		if err != nil {
			return tax.Result{}, err
		}
		if !ok || t.Direction != config.TaxInput {
			return tax.Result{}, invalid("payables.unknown_tax_code", fmt.Sprintf("lines[%d].tax_code", i), "Tax code "+code+
				" is not an input tax code of the legal entity on the line's date "+day.Format(time.DateOnly)+".")
		}
		doc.Codes[code] = t.Calculable()
	}
	res, err := tax.Calculate(doc)
	if err != nil {
		return tax.Result{}, calculationFailed(r.ID, err)
	}
	return res, nil
}

// expenseTax sets each line's tax: what its code charges in its gross
// (tax.LineTax.Charged, the self-assessed aside), none without a code.
func (e *Endpoints) expenseTax(ctx context.Context, r ExpenseReport, lines []ExpenseLine) error {
	c, err := e.calculateExpenses(ctx, r)
	if err != nil {
		return err
	}
	for j, i := range c.coded {
		// Within the line's gross, which is in range: the calculator
		// checked its sums.
		lines[i].TaxAmount, _ = c.lines[j].Charged()
	}
	return nil
}

// PostExpense posts an entered report, once (ADR-0074): Dr each
// line's expense account with its net amount and the tax its code does
// not let the company recover, on its cost center; Dr the recovered
// input tax on its codes' accounts, as a supplier invoice's (taxPostings);
// Cr the employee's reconciliation account with the gross, the
// employee's open item with the employee's payment term counted from the
// report date. The report is read FOR UPDATE (ADR-0008), so it posts
// once. The tax is calculated again, each line at its own date; a line
// whose code no longer gives the tax it was entered with is refused (409
// payables.tax_changed). Its audit record and ExpenseReportChanged are
// written in the posting's transaction.
func (e *Endpoints) PostExpense(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (ExpenseReport, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ExpenseReport{}, err
	}
	r, err := expenseReport(ctx, tx, id, true)
	if err != nil {
		return ExpenseReport{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return ExpenseReport{}, err
	}
	if r.Status != ExpenseEntered {
		return ExpenseReport{}, problem.Conflict("payables.invalid_status", "Expense report "+r.DocumentNumber+" is "+r.Status+"; post requires status [\"entered\"].")
	}
	if postingDate == nil {
		d, err := e.companyDay(ctx, r.LegalEntityID)
		if err != nil {
			return ExpenseReport{}, err
		}
		postingDate = &d
	}
	sup, err := e.postableSupplier(ctx, Invoice{SupplierID: r.EmployeeID, LegalEntityID: r.LegalEntityID})
	if err != nil {
		return ExpenseReport{}, err
	}
	in, err := e.expensePosting(ctx, r, sup, *postingDate)
	if err != nil {
		return ExpenseReport{}, err
	}
	entry, err := e.Postings.Post(ctx, in)
	if err != nil {
		return ExpenseReport{}, err
	}
	if err := markExpensePosted(ctx, tx, p, r, entry, *postingDate, in.Terms); err != nil {
		return ExpenseReport{}, err
	}
	posted, err := expenseReport(ctx, tx, id, false)
	if err != nil {
		return ExpenseReport{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a report's legal entity exists
	if err != nil {
		return ExpenseReport{}, err
	}
	before := ExpenseEntered
	return posted, e.recordExpense(ctx, "payables.expense_report.post", posted, le.Code, &before)
}

// expensePosting is the entry a report posts (PostExpenseReport).
func (e *Endpoints) expensePosting(ctx context.Context, r ExpenseReport, sup partner.Supplier, date time.Time) (ledger.Posting, error) {
	c, err := e.calculateExpenses(ctx, r)
	if err != nil {
		return ledger.Posting{}, err
	}
	if err := requireSameTax(r, c); err != nil {
		return ledger.Posting{}, err
	}
	t, err := e.expenseTaxPostings(ctx, r, c)
	if err != nil {
		return ledger.Posting{}, err
	}
	terms, err := e.termsOf(ctx, sup.PaymentTermCode, r.ReportDate, "The employee's")
	if err != nil {
		return ledger.Posting{}, err
	}
	gross, _, _ := r.Totals()
	lines := append(expenseLines(r, c), t.lines...)
	lines = append(lines, ledger.PostingLine{AccountID: sup.ReconciliationID, Amount: -gross, PartnerID: &r.EmployeeID})
	text := "Expense report " + r.DocumentNumber
	if r.Description != nil {
		text += ": " + *r.Description
	}
	return ledger.Posting{LegalEntityID: r.LegalEntityID, DocumentType: DocumentType, DocumentDate: r.ReportDate, PostingDate: date,
		Currency: r.Currency, Reference: &r.DocumentNumber, HeaderText: &text, SourceContext: "payables", SourceDocumentID: r.ID,
		Lines: lines, Terms: terms, TaxDate: &r.ReportDate, TaxItems: t.items}, nil
}

// expenseTaxPostings are a report's tax lines and items, as a supplier
// invoice's (taxPostings), each date's items dated on it
// (ledger.PostingTaxItem.TaxDate): their tax is reported in the period of
// the expense's date, not the report's.
func (e *Endpoints) expenseTaxPostings(ctx context.Context, r ExpenseReport, c expenseCalculation) (invoiceTax, error) {
	v := Invoice{ID: r.ID, LegalEntityID: r.LegalEntityID, Currency: r.Currency, Exponent: r.Exponent, Kind: KindInvoice}
	var t invoiceTax
	for _, d := range c.days {
		from := len(t.items)
		if err := e.taxPostings(ctx, v, d.day, d.items, &t); err != nil {
			return invoiceTax{}, err
		}
		for k := from; k < len(t.items); k++ {
			t.items[k].TaxDate = &d.day
		}
	}
	return t, nil
}

// requireSameTax refuses a report a line of which its code now gives
// another tax than it was entered with (409 payables.tax_changed): the
// rates on the line's date changed since, and the report, which is not
// changed, is entered again.
func requireSameTax(r ExpenseReport, c expenseCalculation) error {
	for j, i := range c.coded {
		l := r.Lines[i]
		now, _ := c.lines[j].Charged() // within the line's gross, as expenseTax took it
		if now != l.TaxAmount {
			return problem.Conflict("payables.tax_changed", fmt.Sprintf("Line %d's tax code %s now gives %s %s of tax on its date %s; it was "+
				"entered with %s. Enter the report again.", l.LineNo, *l.TaxCode, now.Plain(r.Exponent), r.Currency, l.ExpenseDate.Format(time.DateOnly),
				l.TaxAmount.Plain(r.Exponent)))
		}
	}
	return nil
}

// expenseLines are the report's debits: each line's net amount (its
// gross less the tax charged in it, the base) and the tax its code does
// not let the company recover (a non-deductible component's, the part
// of a component's not recovered), on its account and cost center.
func expenseLines(r ExpenseReport, c expenseCalculation) []ledger.PostingLine {
	debit := make([]money.Minor, len(r.Lines))
	for i, l := range r.Lines {
		debit[i] = l.Amount
	}
	for j, i := range c.coded {
		debit[i] = c.lines[j].Base
		for _, comp := range c.lines[j].Components {
			if comp.AccountKey == tax.KeyNonDeductible || comp.AccountKey == tax.KeyInput || comp.AccountKey == tax.KeySelfInput {
				debit[i] += comp.NonRecoverable // within the line's gross
			}
		}
	}
	out := make([]ledger.PostingLine, len(r.Lines))
	for i, l := range r.Lines {
		text := l.Description
		if text == nil {
			text = &l.ExpenseType
		}
		out[i] = ledger.PostingLine{AccountID: l.AccountID, Amount: debit[i], TaxCode: l.TaxCode, Text: text, CostCenterID: l.CostCenterID}
	}
	return out
}

// markExpensePosted records a report as posted with its entry, its
// posting date and its open item's terms.
func markExpensePosted(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r ExpenseReport, entry ledger.Entry, date time.Time, terms *ledger.ItemTerms) error {
	if _, err := tx.Exec(ctx, `UPDATE payables.expense_report SET status = 'posted', posting_date = $2, journal_entry_year = $3, journal_entry_id = $4,
		payment_term_code = $5, net_due_date = $6, version = version + 1, updated_by = $7 WHERE id = $1`,
		r.ID, date, entry.FiscalYear, entry.ID, terms.PaymentTermCode, terms.NetDueDate(), p.ID); err != nil {
		return fmt.Errorf("payables: mark expense report %s posted: %w", r.ID, err)
	}
	return nil
}
