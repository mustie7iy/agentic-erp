package payables_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Expense reports (T87a, ADR-0074): an accountant enters an employee's
// expenses, gross amounts in the company's currency whose tax the lines'
// codes take out, and posts the report once, the employee's open item
// due by the employee's payment term.

var expensePerms = append(slices.Clone(perms), "payables.expense.read", "payables.expense.write", "payables.expense.post",
	"audit.event.read")

// expenseTypes gives company 2000 the expense types TRAVEL on 520000 and
// MEALS on 530000.
func expenseTypes(doc *config.Config) {
	doc.Company("2000").ExpenseTypes = []config.ExpenseType{{Code: "TRAVEL", Name: "Travel", Account: "520000"},
		{Code: "MEALS", Name: "Meals", Account: "530000"}}
}

type expenseEnv struct {
	env
	meals uuid.UUID // account 530000 of company 2000
	today string    // company 2000's date, the report date
}

func setupExpenses(t *testing.T) expenseEnv {
	t.Helper()
	f := fixture.NewWith(t, nil, expenseTypes, expensePerms...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	return expenseEnv{env: e, meals: f.Account(t, "2000", "530000"), today: time.Now().UTC().Format(time.DateOnly)}
}

func (e expenseEnv) in(t *testing.T) expenseEnv {
	t.Helper()
	e.env = e.env.in(t)
	return e
}

// expenseLine is a line body: extra holds further fields, from a comma.
func expenseLine(date, expenseType, amount, extra string) string {
	return fmt.Sprintf(`{"expense_date": %q, "expense_type": %q, "amount": {"amount": %q, "currency": "EUR"}%s}`, date, expenseType, amount, extra)
}

// report is a report body of the employee in company 2000.
func (e expenseEnv) report(employee uuid.UUID, lines ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "employee_id": %q, "description": "Trip to Hamburg", "lines": [%s]}`, e.DE, employee, strings.Join(lines, ", "))
}

// trip is the walkthrough's report: a train ticket of 119.00 with input
// tax V1 (19%) and a dinner of 50.00 without tax.
func (e expenseEnv) trip() string {
	return e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "119.00", `, "tax_code": "V1", "description": "Train ticket"`),
		expenseLine(e.today, "MEALS", "50.00", ""))
}

// entryLines are an entry's lines as account amount tax code cost center
// partner, "-" for none, sorted.
func (e expenseEnv) entryLines(t *testing.T, entry any) []string {
	t.Helper()
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", entry), "", "")
	var out []string
	for _, l := range je["lines"].([]any) {
		m := l.(map[string]any)
		s := fmt.Sprintf("%v %v", m["account_id"], m["amount"].(map[string]any)["amount"])
		for _, f := range []string{"tax_code", "cost_center_id", "partner_id"} {
			v, ok := m[f].(string)
			if !ok {
				v = "-"
			}
			s += " " + v
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// TestExpenseReportEnteredAndPosted: a report is entered with its lines'
// tax taken out of their gross, read back with what it names, posted
// once to each line's expense type's account, the input tax to the
// code's account and the gross to the employee's open item, due by the
// employee's term; it is listed by employee and status, audited, and
// published at each change.
func TestExpenseReportEnteredAndPosted(t *testing.T) {
	e := setupExpenses(t)
	r, tag := e.Must(201, "POST", "/v1/expense-reports", e.trip(), "")
	path := "/v1/expense-reports/" + id(r)
	for _, c := range []struct{ name, got, want string }{
		{"status and version", fmt.Sprintf("%v %v %v", r["status"], r["version"], tag), `entered 1 "1"`},
		{"number", fmt.Sprint(strings.HasPrefix(r["document_number"].(string), "ER")), "true"},
		{"company", fmt.Sprintf("%v %v", r["company_code"], r["company_name"]), "2000 Acme GmbH"},
		{"employee", fmt.Sprintf("%v %v", r["employee_id"], r["employee_number"]), e.Supplier.String() + " BP90000001"},
		{"report date", fmt.Sprint(r["report_date"]), e.today},
		{"totals", fmt.Sprint(r["gross_amount"], r["tax_amount"], r["net_amount"]), "map[amount:169.00 currency:EUR] map[amount:19.00 currency:EUR] map[amount:150.00 currency:EUR]"},
		{"lines", expenseLines(r), "1 TRAVEL Travel 520000 Office Supplies 119.00 V1 19.00 100.00 Train ticket, 2 MEALS Meals 530000 IT Services 50.00 - 0.00 50.00 -"},
		{"not posted", fmt.Sprint(r["journal_entry_id"], r["posting_date"], r["net_due_date"]), "<nil> <nil> <nil>"},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, c.got, c.want)
		}
	}
	posted, tag := e.Must(200, "POST", path+":post", "", tag)
	if got := fmt.Sprintf("%v %v %v %v", posted["status"], posted["version"], posted["posting_date"], posted["payment_term_code"]); got != "posted 2 "+e.today+" NT30" {
		t.Errorf("posted report: got %s, want posted 2 %s NT30", got, e.today)
	}
	due := date(e.today).AddDate(0, 0, 30).Format(time.DateOnly)
	if posted["net_due_date"] != due {
		t.Errorf("net due date: got %v, want %s", posted["net_due_date"], due)
	}
	want := []string{fmt.Sprintf("%s -169.00 - - %s", e.ap, e.Supplier), fmt.Sprintf("%s 100.00 V1 - -", e.expense), fmt.Sprintf("%s 19.00 V1 - -", e.tax),
		fmt.Sprintf("%s 50.00 - - -", e.meals)}
	slices.Sort(want)
	if got := e.entryLines(t, posted["journal_entry_id"]); !slices.Equal(got, want) {
		t.Errorf("entry lines: got %v, want %v", got, want)
	}
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Supplier.String(), "", "")
	items := open["items"].([]any)
	if len(items) != 1 || terms(items[0].(map[string]any)["payment_terms"]) != fmt.Sprintf("NT30 %s %s []", e.today, due) {
		t.Errorf("the employee's open items: got %v, want one due by NT30 from %s", items, e.today)
	}
	e.Problem(409, "payables.invalid_status", "POST", path+":post", "", tag)
	// A second report, of another employee, with two lines of one tax code.
	second, _ := e.Must(201, "POST", "/v1/expense-reports", e.report(e.Vendor, expenseLine(e.today, "TRAVEL", "11.90", `, "tax_code": "V1"`),
		expenseLine(e.today, "MEALS", "23.80", `, "tax_code": "V1"`)), "")
	if got := fmt.Sprint(second["tax_amount"]); got != "map[amount:5.70 currency:EUR]" {
		t.Errorf("second report's tax: got %s, want 5.70 EUR", got)
	}
	for query, want := range map[string]int{"employee_id=" + e.Supplier.String() + "&status=posted": 1, "employee_id=" + e.Vendor.String(): 1,
		"status=entered": 1, "legal_entity_id=" + e.DE.String(): 2, "document_number=" + posted["document_number"].(string): 1,
		"order_by=document_number": 2, "order_by=document_number&page_size=1": 1, "page_size=1": 1} {
		page, _ := e.Must(200, "GET", "/v1/expense-reports?"+query, "", "")
		if got := len(page["items"].([]any)); got != want {
			t.Errorf("list %s: got %d, want %d", query, got, want)
		}
	}
	var changed []map[string]any
	for _, ev := range events(t, e.Tenant, "expense_report_changed") {
		if ev["expense_report_id"] == id(r) {
			changed = append(changed, ev)
		}
	}
	if len(changed) != 2 || changed[0]["status"] != "entered" || changed[0]["status_before"] != nil || changed[1]["status"] != "posted" ||
		changed[1]["status_before"] != "entered" || changed[1]["journal_entry_id"] != posted["journal_entry_id"] || changed[1]["net_due_date"] != due {
		t.Errorf("ExpenseReportChanged: got %v, want entered then posted with its entry", changed)
	}
	if _, ok := changed[1]["employee_name"]; ok {
		t.Errorf("ExpenseReportChanged: got the employee's name, want only the number")
	}
	audit, _ := e.Must(200, "GET", "/v1/audit-events?object_type=payables.expense_report&object_id="+id(r), "", "")
	var actions []string
	for _, a := range audit["items"].([]any) {
		actions = append(actions, a.(map[string]any)["action"].(string))
	}
	slices.Sort(actions)
	if got := strings.Join(actions, " "); got != "payables.expense_report.create payables.expense_report.post" {
		t.Errorf("audit trail: got %s", got)
	}
}

// expenseLines are a report's lines as number, type, type name, account
// number and name, gross, tax code, tax, net and text.
func expenseLines(r map[string]any) string {
	var out []string
	for _, l := range r["lines"].([]any) {
		m := l.(map[string]any)
		or := func(f string) any {
			if v, ok := m[f]; ok {
				return v
			}
			return "-"
		}
		amount := func(f string) any { return m[f].(map[string]any)["amount"] }
		out = append(out, fmt.Sprintf("%v %v %v %v %v %v %v %v %v %v", m["line_no"], m["expense_type"], or("expense_type_name"), m["account_number"],
			m["account_name"], amount("amount"), or("tax_code"), amount("tax_amount"), amount("net_amount"), or("description")))
	}
	return strings.Join(out, ", ")
}

// TestExpenseReportRefused: what entering a report refuses names its
// field; nothing is entered.
func TestExpenseReportRefused(t *testing.T) {
	e := setupExpenses(t)
	blocked := e.NewSupplier(t, "BP90000003")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, blocked)
	tomorrow := date(e.today).AddDate(0, 0, 1).Format(time.DateOnly)
	ok := expenseLine(e.today, "TRAVEL", "10.00", "")
	for name, c := range map[string]struct{ body, code, field string }{
		"an expense after today": {e.report(e.Supplier, ok, expenseLine(tomorrow, "TRAVEL", "10.00", "")), "payables.expense_date_in_future",
			"lines[1].expense_date"},
		"an unknown expense type": {e.report(e.Supplier, expenseLine(e.today, "GIFTS", "10.00", "")), "payables.unknown_expense_type", "lines[0].expense_type"},
		"an unknown tax code": {e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "10.00", `, "tax_code": "XX"`)), "payables.unknown_tax_code",
			"lines[0].tax_code"},
		"another currency":    {e.report(e.Supplier, strings.Replace(ok, "EUR", "USD", 1)), "payables.currency_mismatch", "lines[0].amount.currency"},
		"zero":                {e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "0.00", "")), "payables.invalid_amount", "lines[0].amount.amount"},
		"a blank text":        {e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "1.00", `, "description": " "`)), "request.validation_failed", "lines[0].description"},
		"a blocked employee":  {e.report(blocked, ok), "payables.supplier_not_usable", "employee_id"},
		"no such employee":    {e.report(uuid.New(), ok), "request.reference_not_found", "employee_id"},
		"a blank header text": {strings.Replace(e.report(e.Supplier, ok), "Trip to Hamburg", " ", 1), "request.validation_failed", "description"},
		"more than can be posted": {e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "90000000000000000.00", ""),
			expenseLine(e.today, "TRAVEL", "90000000000000000.00", "")), "payables.amount_too_large", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(422, c.code, "POST", "/v1/expense-reports", c.body, "")
			if c.field == "" {
				return
			}
			errs, _ := p["errors"].([]any)
			if len(errs) == 0 || errs[0].(map[string]any)["field"] != c.field {
				t.Errorf("refused field: got %v, want %s", p["errors"], c.field)
			}
		})
	}
	page, _ := e.Must(200, "GET", "/v1/expense-reports", "", "")
	if n := len(page["items"].([]any)); n != 0 {
		t.Errorf("reports entered: got %d, want 0", n)
	}
}

// TestExpenseReportAccounts: an expense type whose account is not set up
// in the company, or is blocked there, is refused naming the line's type.
func TestExpenseReportAccounts(t *testing.T) {
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		expenseTypes(doc)
		doc.Company("2000").ExpenseTypes = append(doc.Company("2000").ExpenseTypes, config.ExpenseType{Code: "FREIGHT", Name: "Freight", Account: "540000"})
	}, expensePerms...)
	e := expenseEnv{env: env{Env: f}, today: time.Now().UTC().Format(time.DateOnly)}
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE legal_entity_id = $1
		AND account_id = (SELECT a.id FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.id = a.chart_id WHERE c.tenant_id = $2 AND c.code = '2000' AND a.number = '540000')`, e.DE, e.Tenant.ID)
	pgtest.Exec(t, `DELETE FROM ledger.company_account WHERE legal_entity_id = $1
		AND account_id = (SELECT a.id FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.id = a.chart_id WHERE c.tenant_id = $2 AND c.code = '2000' AND a.number = '530000')`, e.DE, e.Tenant.ID)
	for name, line := range map[string]string{"blocked": expenseLine(e.today, "FREIGHT", "1.00", ""), "not set up": expenseLine(e.today, "MEALS", "1.00", "")} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(422, "payables.account_not_postable", "POST", "/v1/expense-reports", e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "1.00", ""), line), "")
			if got := p["errors"].([]any)[0].(map[string]any)["field"]; got != "lines[1].expense_type" {
				t.Errorf("refused field: got %v, want lines[1].expense_type", got)
			}
		})
	}
}

// TestExpenseReportCostCenters: a line's cost center is checked as the
// posting will check it, a refusal naming the line; one the ledger
// accepts is read back by code and name and posted on the line.
func TestExpenseReportCostCenters(t *testing.T) {
	f := fixture.NewWith(t, nil, expenseTypes, expensePerms...)
	e := expenseEnv{env: env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")},
		meals: f.Account(t, "2000", "530000"), today: time.Now().UTC().Format(time.DateOnly)}
	c := e.CostCenters(t, func(doc *config.Config) { fixture.GRIRAccounts(doc); expenseTypes(doc) })
	center := func(id uuid.UUID) string { return fmt.Sprintf(`, "cost_center_id": %q`, id) }
	p := e.Problem(422, "ledger.cost_center_invalid", "POST", "/v1/expense-reports",
		e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "1.00", ""), expenseLine(e.today, "TRAVEL", "1.00", center(c.Locked))), "")
	if got := p["errors"].([]any)[0].(map[string]any)["field"]; got != "lines[1].cost_center_id" || !strings.HasPrefix(p["detail"].(string), "Line 2") {
		t.Errorf("refusal: got %v %v, want lines[1].cost_center_id and Line 2", got, p["detail"])
	}
	r, tag := e.Must(201, "POST", "/v1/expense-reports", e.report(e.Supplier, expenseLine(e.today, "TRAVEL", "10.00", center(c.Sales))), "")
	line := r["lines"].([]any)[0].(map[string]any)
	if got := fmt.Sprintf("%v %v %v", line["cost_center_id"], line["cost_center_code"], line["cost_center_name"]); got != c.Sales.String()+" 2000 CC 2000" {
		t.Errorf("line's cost center: got %s, want %s 2000 CC 2000", got, c.Sales)
	}
	posted, _ := e.Must(200, "POST", "/v1/expense-reports/"+id(r)+":post", "", tag)
	if got := e.entryLines(t, posted["journal_entry_id"]); !slices.Contains(got, fmt.Sprintf("%s 10.00 - %s -", e.expense, c.Sales)) {
		t.Errorf("entry lines: got %v, want the expense on cost center %s", got, c.Sales)
	}
}

// TestExpenseReportPostRefused: posting needs an open period and the tax
// the lines were entered with; a refused report stays entered.
func TestExpenseReportPostRefused(t *testing.T) {
	e := setupExpenses(t)
	r, tag := e.Must(201, "POST", "/v1/expense-reports", e.trip(), "")
	path := "/v1/expense-reports/" + id(r) + ":post"
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`,
		e.Tenant.ID)
	e.Problem(409, "ledger.period_not_open", "POST", path, `{"posting_date": "2026-02-10"}`, tag)
	pgtest.Exec(t, `UPDATE payables.expense_report_line SET tax_amount = tax_amount + 1 WHERE expense_report_id = $1 AND line_no = 1`, id(r))
	p := e.Problem(409, "payables.tax_changed", "POST", path, "", tag)
	if !strings.Contains(p["detail"].(string), "Line 1's tax code V1 now gives 19.00 EUR") {
		t.Errorf("detail: got %v, want line 1's tax named", p["detail"])
	}
	pgtest.Exec(t, `UPDATE payables.expense_report_line SET tax_code = 'XX' WHERE expense_report_id = $1 AND line_no = 1`, id(r))
	p = e.Problem(422, "payables.unknown_tax_code", "POST", path, "", tag)
	if got := p["errors"].([]any)[0].(map[string]any)["field"]; got != "lines[0].tax_code" {
		t.Errorf("refused field: got %v, want lines[0].tax_code", got)
	}
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	e.Problem(422, "payables.supplier_not_usable", "POST", path, "", tag)
	e.Problem(412, "request.version_mismatch", "POST", path, "", `"9"`)
	if got, _ := e.Must(200, "GET", "/v1/expense-reports/"+id(r), "", ""); got["status"] != "entered" || got["version"] != float64(1) {
		t.Errorf("refused report: got %v %v, want entered 1", got["status"], got["version"])
	}
	// A reverse charge pair whose halves are at different rates: an input
	// code the calculator refuses.
	doc := fixture.Document(t)
	doc.Company("2000").ChartOfAccounts = append(doc.Company("2000").ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability})
	pair := halfPair("RX")
	pair.Components[0].Rates = []config.TaxRate{rate("7", "2026-01-01", "")}
	fixture.TaxCodes(t, e.Tenant, doc, pair)
	e.Problem(422, "payables.invalid_tax_code", "POST", "/v1/expense-reports", e.report(e.Vendor, expenseLine(e.today, "TRAVEL", "1.00", `, "tax_code": "RX"`)), "")
	other := e.Reader(t, "outsider", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", "/v1/expense-reports/"+id(r), "", "")
}

// TestExpenseDatabaseFailures calls every expense report handler outside
// a unit of work and with each of its database statements failing in
// turn.
func TestExpenseDatabaseFailures(t *testing.T) {
	e := setupExpenses(t)
	h := endpoints(nil)
	entered, _ := e.Must(201, "POST", "/v1/expense-reports", e.trip(), "")
	u := uuid.MustParse(id(entered))
	lines := []gen.ExpenseReportLineCreate{{ExpenseDate: date(e.today), ExpenseType: "TRAVEL", Amount: gen.Money{Amount: "119.00", Currency: "EUR"},
		TaxCode: gen.NewOptString("V1")}, {ExpenseDate: date(e.today), ExpenseType: "MEALS", Amount: gen.Money{Amount: "5.00", Currency: "EUR"}}}
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateExpenseReport(ctx, &gen.ExpenseReportCreate{LegalEntityID: e.DE, EmployeeID: e.Supplier, Lines: lines}, gen.CreateExpenseReportParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetExpenseReport(ctx, gen.GetExpenseReportParams{ID: u})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListExpenseReports(ctx, gen.ListExpenseReportsParams{})
			return err
		},
		"post": func(ctx context.Context) error {
			_, err := h.PostExpenseReport(ctx, gen.OptExpenseReportPost{}, gen.PostExpenseReportParams{ID: u, IfMatch: "*"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"a page size of 0": func(ctx context.Context) error {
			_, err := h.ListExpenseReports(ctx, gen.ListExpenseReportsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"no such report": func(ctx context.Context) error {
			_, err := h.GetExpenseReport(ctx, gen.GetExpenseReportParams{ID: uuid.New()})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); err == nil {
				t.Error("got nil error, want a refusal")
			}
		})
	}
}
