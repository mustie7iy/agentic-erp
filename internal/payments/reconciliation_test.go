package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// reconScenario is a house bank whose statements post (postScenario's
// bank, its incoming payments clearing account 140000) and its first
// statement R1, opening at 100.00 on 16 April: charges of 10.50 the rules
// post at once (line 1), an incoming payment of 40.00 (line 2) and a debit
// of 5.00 nothing explains (line 3), closing at 124.50 on 17 April.
type reconScenario struct {
	matchScenario
}

func reconSetup(t *testing.T) reconScenario {
	t.Helper()
	e := matchSetupWith(t, "payments.statement.post")
	e.statementRules(t, postRules)
	e.bankPosts(t)
	pgtest.Exec(t, `UPDATE payments.house_bank SET incoming_gl_account_id = $2, version = version + 1 WHERE id = $1`, e.bank, e.Account(t, "2000", "140000"))
	file := mtStatement("R1", "1", "100,00", "260416", "124,50", "260417",
		":61:2604170417D10,50NCHGNONREF//R1\n",
		":61:2604170417C40,00NTRFNONREF//R2\n:86:/NAME/CUSTOMER 4711/REMI/AR-2026-118\n",
		":61:2604170417D5,00NMSCNONREF//R3\n")
	s := e.imported(t, "mt940", file)[0].(map[string]any)
	return reconScenario{matchScenario{e: e, statement: id(s)}}
}

// openingEntry posts what the bank held before its first statement to the
// main bank account 101000 on 15 April, as a company's opening balance.
func (sc reconScenario) openingEntry(t *testing.T, amount money.Minor) {
	t.Helper()
	e := sc.e
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	text := "Opening balance"
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date("2026-04-15"), PostingDate: date("2026-04-15"),
			Currency: "EUR", HeaderText: &text, SourceContext: "payments", SourceDocumentID: uuid.Must(uuid.NewV7()),
			Lines: []ledger.PostingLine{{AccountID: e.Account(t, "2000", "101000"), Amount: amount}, {AccountID: e.expense, Amount: -amount}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// report reads the house bank's reconciliation report with the query.
func (sc reconScenario) report(t *testing.T, query string) map[string]any {
	t.Helper()
	r, _ := sc.e.in(t).Must(200, "GET", "/v1/house-banks/"+sc.e.bank.String()+"/reconciliation"+query, "", "")
	return r
}

// amountOf is a Money field's amount.
func amountOf(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m["amount"]
	}
	return v
}

// wantFigures checks figures against "closing ledger excluded in_transit
// posted_later unexplained" ("-" for an absent amount).
func wantFigures(t *testing.T, what string, figures any, want string) {
	t.Helper()
	f := figures.(map[string]any)
	var got []string
	for _, k := range []string{"closing_balance", "ledger_balance", "excluded", "in_transit", "posted_later", "unexplained"} {
		a := amountOf(f[k])
		if a == nil {
			a = "-"
		}
		got = append(got, fmt.Sprint(a))
	}
	if strings.Join(got, " ") != want {
		t.Errorf("%s figures (closing ledger excluded in_transit posted_later unexplained): got %v, want %s", what, got, want)
	}
}

// TestIncomingPayment: a person matches a credit to the house bank's
// incoming payments clearing account, which posts it against the main bank
// account, an open item there until accounts receivable clears it
// (ADR-0047, T49h).
func TestIncomingPayment(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	m, tag := e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "Customer 4711 paid AR-2026-118."}`, sc.tag(t))
	targets := m["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("targets of an incoming payment: got %v, want one", targets)
	}
	if got := targets[0].(map[string]any)["incoming"]; got != true {
		t.Errorf("target: got incoming %v, want true", got)
	}
	posted, _ := e.Must(200, "POST", sc.line(2, "post"), "", tag)
	if posted["journal_entry_id"] == nil {
		t.Fatalf("an incoming payment of 40.00 within the first tier: got %v, want it posted", posted)
	}
	wantPosting(t, "incoming payment", posted["journal_entry_id"].(string), map[string]money.Minor{"101000": 4000, "140000": -4000})
	if got := e.lineStatuses(t, sc.statement)[1]; got != "posted" {
		t.Errorf("line 2: got %s, want posted", got)
	}
}

// TestIncomingPaymentRefused: only a credit that reverses nothing, of a
// bank naming an incoming payments clearing account, is an incoming
// payment; a match names one target; a bank that dropped the account
// since the match does not post it.
func TestIncomingPaymentRefused(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	for name, c := range map[string]struct {
		line       int
		body, code string
	}{
		"a debit":           {3, `{"incoming": true, "reason": "x"}`, "payments.statement_match_target"},
		"a rule and income": {2, `{"incoming": true, "posting_rule": "NCHG", "reason": "x"}`, "request.validation_failed"},
		"nothing":           {2, `{"reason": "x"}`, "request.validation_failed"},
	} {
		t.Run(name, func(t *testing.T) { e.in(t).Problem(422, c.code, "POST", sc.line(c.line, "match"), c.body, sc.tag(t)) })
	}
	file := mtStatement("R2", "2", "124,50", "260418", "114,50", "260418", ":61:2604180418RD10,00NTRFNONREF//R4\n", ":61:2604180418D20,00NTRFNONREF//R5\n")
	reversal := id(e.imported(t, "mt940", file)[0].(map[string]any))
	_, tag := e.Must(200, "GET", "/v1/bank-statements/"+reversal, "", "")
	t.Run("a reversal", func(t *testing.T) {
		e.in(t).Problem(422, "payments.statement_match_target", "POST", "/v1/bank-statements/"+reversal+"/lines/1:match", `{"incoming": true, "reason": "x"}`, tag)
	})
	_, tag = e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "x"}`, sc.tag(t))
	pgtest.Exec(t, `UPDATE payments.house_bank SET incoming_gl_account_id = NULL, version = version + 1 WHERE id = $1`, e.bank)
	t.Run("the account dropped since", func(t *testing.T) {
		e.in(t).Problem(409, "payments.statement_incoming_gone", "POST", sc.line(2, "post"), "", tag)
	})
	t.Run("a bank without the account", func(t *testing.T) {
		e.in(t).Problem(422, "payments.statement_match_target", "POST", sc.line(2, "match"), `{"incoming": true, "reason": "x"}`, sc.tag(t))
	})
}

// TestReconcileStatement: a statement becomes reconciled once every line
// is posted or excluded and the ledger agrees with its closing balance
// less what was excluded; until the opening balance reaches the ledger,
// the difference is unexplained and it stays imported. A run of the rules
// takes the ledger's change up. Reconciled once (one event, one audit
// entry), its lines are decided for good (ADR-0047, T49h).
func TestReconcileStatement(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	_, tag := e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "Customer 4711."}`, sc.tag(t))
	_, tag = e.Must(200, "POST", sc.line(2, "post"), "", tag)
	e.Must(201, "POST", sc.line(3, "exclude"), `{"reason": "The bank's error; it reverses it tomorrow."}`, tag)
	s, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	if s["status"] != "imported" {
		t.Fatalf("status with the opening balance missing from the ledger: got %v, want imported", s["status"])
	}
	wantFigures(t, "before the opening entry", sc.report(t, "")["items"].([]any)[0].(map[string]any)["figures"], "124.50 29.50 -5.00 0.00 0.00 100.00")
	sc.openingEntry(t, 10000)
	if s, _ = e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", ""); s["status"] != "imported" {
		t.Fatalf("status before anything runs: got %v, want imported", s["status"])
	}
	s, _ = e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	if s["status"] != "reconciled" {
		t.Fatalf("status after a run with the ledger agreeing: got %v, want reconciled", s["status"])
	}
	if s["reconciled_by"] != e.Tenant.Human.ID.String() {
		t.Errorf("reconciled by: got %v, want %s", s["reconciled_by"], e.Tenant.Human.ID)
	}
	if s["reconciled_at"] == nil {
		t.Error("reconciled at: got none, want when")
	}
	e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	evs := events(t, e.Tenant, "bank_statement_reconciled")
	if len(evs) != 1 {
		t.Fatalf("reconciled events after two runs: got %d, want 1", len(evs))
	}
	if got := amountOf(evs[0]["ledger_balance"]); got != "129.50" {
		t.Errorf("event's ledger balance: got %v, want 129.50", got)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE object_id = $1 AND action = 'payments.bank_statement.reconcile'`, []any{sc.statement}, &audited)
	if audited != 1 {
		t.Errorf("audited reconciliations: got %d, want 1", audited)
	}
	e.Problem(409, "payments.statement_reconciled", "POST", sc.line(3, "match"), `{"posting_rule": "NCHG", "reason": "x"}`, sc.tag(t))
}

// TestReconcileOnLastDecision: the last line's exclusion, a person's
// posting and an approval each reconcile a statement the ledger agrees
// with; a statement in gap is never reconciled.
func TestReconcileOnLastDecision(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	sc.openingEntry(t, 10000)
	_, tag := e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "Customer 4711."}`, sc.tag(t))
	_, tag = e.Must(201, "POST", sc.line(3, "exclude"), `{"reason": "The bank's error."}`, tag)
	e.Must(200, "POST", sc.line(2, "post"), "", tag)
	if s, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", ""); s["status"] != "reconciled" {
		t.Errorf("after the last line's posting: got %v, want reconciled", s["status"])
	}
	file := mtStatement("R2", "2", "124,50", "260418", "64,50", "260418", ":61:2604180418D60,00NTRFNONREF//R6\n")
	r2 := id(e.imported(t, "mt940", file)[0].(map[string]any))
	_, tag = e.Must(200, "GET", "/v1/bank-statements/"+r2, "", "")
	_, tag = e.As(e.Approver2).Must(201, "POST", "/v1/bank-statements/"+r2+"/lines/1:match", `{"posting_rule": "NCHG", "reason": "Charges."}`, tag)
	m, _ := e.Must(200, "POST", "/v1/bank-statements/"+r2+"/lines/1:post", "", tag)
	e.approve(t, e.Approver, id(m))
	if s, _ := e.Must(200, "GET", "/v1/bank-statements/"+r2, "", ""); s["status"] != "reconciled" {
		t.Errorf("after the approval of the last line's posting: got %v, want reconciled", s["status"])
	}
	gap := mtStatement("R4", "4", "1,00", "260420", "0,00", "260420", ":61:2604200420D1,00NTRFNONREF//R7\n")
	r4 := e.imported(t, "mt940", gap)[0].(map[string]any)
	if r4["status"] != "gap" {
		t.Fatalf("a statement after a missing one: got %v, want gap", r4["status"])
	}
	_, tag = e.Must(200, "GET", "/v1/bank-statements/"+id(r4), "", "")
	e.Must(201, "POST", "/v1/bank-statements/"+id(r4)+"/lines/1:exclude", `{"reason": "x"}`, tag)
	if s, _ := e.Must(200, "GET", "/v1/bank-statements/"+id(r4), "", ""); s["status"] != "gap" {
		t.Errorf("a statement in gap with its lines decided: got %v, want gap", s["status"])
	}
}

// TestReconciliationReport: the report pages the period's statements by
// closing date with their figures, their lines' counts and their lines
// (of one status when asked), the period's summary on its last closing
// date, and is read by agents too; a period that ends before it starts is
// refused, and another company's bank is not found.
func TestReconciliationReport(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	sc.openingEntry(t, 10000)
	e.Must(201, "POST", sc.line(3, "exclude"), `{"reason": "The bank's error."}`, sc.tag(t))
	file := mtStatement("R2", "2", "124,50", "260418", "104,50", "260418", ":61:2604180418D20,00NTRFNONREF//R8\n")
	e.imported(t, "mt940", file)
	_, tag := e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "Customer 4711."}`, sc.tag(t))
	for _, status := range []string{"closed", "locked"} {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND period = 4`, e.Tenant.ID, e.DE, status)
	}
	e.Must(200, "POST", sc.line(2, "post"), `{"posting_date": "2026-05-04", "reason": "April is locked."}`, tag)
	r := sc.report(t, "?page_size=1")
	sum := r["summary"].(map[string]any)
	if sum["statements"] != float64(2) {
		t.Errorf("statements in the report: got %v, want 2", sum["statements"])
	}
	if got := amountOf(sum["opening_balance"]); got != "100.00" {
		t.Errorf("opening balance: got %v, want the first statement's 100.00", got)
	}
	wantFigures(t, "summary", sum["figures"], "104.50 89.50 -5.00 -20.00 40.00 0.00")
	if got := fmt.Sprint(sum["lines"]); got != "map[excluded:1 matched:0 pending_approval:0 posted:2 proposed:0 unmatched:1]" {
		t.Errorf("summary's lines: got %s", got)
	}
	first := r["items"].([]any)[0].(map[string]any)
	wantFigures(t, "R1 on its closing date", first["figures"], "124.50 89.50 -5.00 0.00 40.00 0.00")
	if first["status"] != "reconciled" {
		t.Errorf("R1 with its lines decided, one posted after its closing date: got %v, want reconciled", first["status"])
	}
	lines := first["lines"].([]any)
	if len(lines) != 3 {
		t.Fatalf("R1's lines: got %d, want 3", len(lines))
	}
	incoming := lines[1].(map[string]any)
	if incoming["incoming"] != true || incoming["match_kind"] != "manual" || incoming["posting_date"] != "2026-05-04" {
		t.Errorf("R1 line 2: got %v, want the incoming payment matched by hand, posted on 4 May", incoming)
	}
	if lines[0].(map[string]any)["match_rule"] != "transaction_code" {
		t.Errorf("R1 line 1: got %v, want matched by the transaction code rule", lines[0])
	}
	tok, ok := r["next_page_token"].(string)
	if !ok {
		t.Fatal("page 1 of 2: got no next page token")
	}
	second := sc.report(t, "?page_size=1&page_token="+tok)["items"].([]any)
	if len(second) != 1 || second[0].(map[string]any)["statement_ref"] != "R2" {
		t.Errorf("page 2: got %v, want R2", second)
	}
	open := sc.report(t, "?line_status=unmatched&closing_date_from=2026-04-18")["items"].([]any)
	if len(open) != 1 || len(open[0].(map[string]any)["lines"].([]any)) != 1 {
		t.Errorf("R2's unmatched lines: got %v, want its one line", open)
	}
	empty := sc.report(t, "?closing_date_to=2026-04-01")
	if s := empty["summary"].(map[string]any); s["statements"] != float64(0) || s["figures"] != nil {
		t.Errorf("a period without statements: got %v, want none and no figures", s)
	}
	t.Run("agents", func(t *testing.T) {
		e.Tenant.AllCompanies(t, e.Tenant.Agent)
		e.agent().in(t).Must(200, "GET", "/v1/house-banks/"+e.bank.String()+"/reconciliation", "", "")
	})
	t.Run("a period ending first", func(t *testing.T) {
		e.in(t).Problem(422, "request.validation_failed", "GET", "/v1/house-banks/"+e.bank.String()+"/reconciliation?closing_date_from=2026-04-18&closing_date_to=2026-04-17", "", "")
	})
	t.Run("another bank", func(t *testing.T) {
		e.in(t).Problem(404, "request.not_found", "GET", "/v1/house-banks/"+uuid.NewString()+"/reconciliation", "", "")
	})
}

// TestReconciliationWithoutMainAccount: a bank whose statements never post
// has no ledger balance and nothing unexplained.
func TestReconciliationWithoutMainAccount(t *testing.T) {
	sc := reconSetup(t)
	pgtest.Exec(t, `UPDATE payments.house_bank SET main_gl_account_id = NULL, incoming_gl_account_id = NULL, version = version + 1 WHERE id = $1`, sc.e.bank)
	wantFigures(t, "a bank that never posts", sc.report(t, "")["summary"].(map[string]any)["figures"], "124.50 - 0.00 35.00 0.00 -")
}

// TestReconciliationDatabaseFailures: every statement of reconciling and
// of the report fails in turn, and none runs outside a unit of work.
func TestReconciliationDatabaseFailures(t *testing.T) {
	sc := reconSetup(t)
	e := sc.e
	sc.openingEntry(t, 10000)
	e.Must(201, "POST", sc.line(2, "match"), `{"incoming": true, "reason": "Customer 4711."}`, sc.tag(t))
	h := endpoints(nil)
	st := uuidOf(sc.statement)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	ks, err := pages.Keyset(pagination.Sort{Default: "closing_date", Fields: []pagination.Field{{Name: "closing_date", Column: "s.closing_date", Type: "date"}}}, "t", pagination.Request{})
	if err != nil {
		t.Fatal(err)
	}
	report := func(ctx context.Context) error {
		_, err := h.BankReconciliation(ctx, e.bank, payments.ReconciliationQuery{Page: ks})
		return err
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"report": report,
		"handler": func(ctx context.Context) error {
			_, err := h.GetBankReconciliation(ctx, gen.GetBankReconciliationParams{ID: e.bank})
			return err
		},
		"post and reconcile": func(ctx context.Context) error {
			if _, err := h.ExcludeLine(ctx, st, 3, anyVersion, "x"); err != nil {
				return err
			}
			_, err := h.PostLine(ctx, st, 2, anyVersion, payments.LinePosting{})
			return err
		},
		"match incoming": func(ctx context.Context) error {
			_, err := h.MatchLine(ctx, st, 2, anyVersion, payments.ManualMatch{Incoming: true, Reason: "x"})
			return err
		},
		"balances": func(ctx context.Context) error {
			_, err := ledger.Accounts{}.Balances(ctx, e.DE, e.Account(t, "2000", "101000"), []time.Time{date("2026-04-17")})
			return err
		},
	} {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
	}
	t.Run("outside a unit of work", func(t *testing.T) { pgtest.WantErr(t, report(context.Background()), "") })
}
