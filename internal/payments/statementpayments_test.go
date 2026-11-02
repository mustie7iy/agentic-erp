package payments_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// e2es are the end-to-end ids of a page of payments, in its order.
func e2es(page map[string]any) string {
	var out []string
	for _, it := range page["items"].([]any) {
		out = append(out, it.(map[string]any)["end_to_end_id"].(string))
	}
	return strings.Join(out, ",")
}

// namesItsBank makes a posted payment name its house bank, as a run
// proposed without one writes it (000093); the scenario's run was proposed
// for the bank. A posted payment does not change (payment_posted_once),
// so the trigger is skipped for this one test row.
func namesItsBank(t *testing.T, payment string, bank uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE payments.payment SET house_bank_id = '`+bank.String()+`' WHERE id = '`+uuidOf(payment).String()+`'; COMMIT`)
}

// TestSearchPaymentsToMatch: the payments a person may match a line to by
// hand are those the rules may clear and no line holds, of the statement's
// bank and currency, whether the payment or its run names the bank,
// latest first, narrowed by amount and run date; the one chosen is matched
// through matchBankStatementLine and leaves the search (T73g2).
func TestSearchPaymentsToMatch(t *testing.T) {
	sc := matchStatementSetup(t)
	e, pays := sc.e, sc.pays
	namesItsBank(t, pays[3].payment, e.bank)
	search := "/v1/bank-statements/" + sc.statement + "/payments"
	// Lines 1 and 2 hold payments 0 and 1; line 4's open match only names
	// payments 2 and 3, which stay free to choose.
	free := pays[3].e2e + "," + pays[2].e2e
	page, _ := e.Must(200, "GET", search, "", "")
	if got := e2es(page); got != free {
		t.Errorf("payments to match: got %s, want %s", got, free)
	}
	wantPaymentRefs(t, page)
	for name, c := range map[string]struct{ query, want string }{
		"from a run date":        {"?run_date_from=2026-04-18", pays[3].e2e},
		"to a run date":          {"?run_date_to=2026-04-17", pays[2].e2e},
		"within the amount":      {"?amount_min=100.00&amount_max=100.00", free},
		"above the amount":       {"?amount_min=100.01", ""},
		"below the amount":       {"?amount_max=99.99", ""},
		"the first of two pages": {"?page_size=1", pays[3].e2e},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := e.in(t).Must(200, "GET", search+c.query, "", "")
			if e2es(got) != c.want {
				t.Errorf("got %s, want %s", e2es(got), c.want)
			}
		})
	}
	first, _ := e.Must(200, "GET", search+"?page_size=1", "", "")
	next, _ := e.Must(200, "GET", search+"?page_size=1&page_token="+first["next_page_token"].(string), "", "")
	if got := e2es(next); got != pays[2].e2e {
		t.Errorf("second page: got %s, want %s", got, pays[2].e2e)
	}
	if got := next["next_page_token"]; got != nil {
		t.Errorf("second page's next token: got %v, want none", got)
	}
	_, tag := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	e.Must(201, "POST", "/v1/bank-statements/"+sc.statement+"/lines/5:match", `{"payment_ids": ["`+pays[3].payment+`"], "reason": "Remittance found."}`, tag)
	if got, _ := e.Must(200, "GET", search, "", ""); e2es(got) != pays[2].e2e {
		t.Errorf("after line 5 matched by hand: got %s, want %s", e2es(got), pays[2].e2e)
	}
}

// TestSearchPaymentsRefused: amounts without the currency's digits, a
// statement not found, an agent and a person who may not match lines are
// refused.
func TestSearchPaymentsRefused(t *testing.T) {
	sc := matchStatementSetup(t)
	e := sc.e
	search := "/v1/bank-statements/" + sc.statement + "/payments"
	readOnly, _ := statementSetup(t)
	for name, c := range map[string]struct {
		as     env
		status int
		code   string
		path   string
	}{
		"too few decimals":      {e, 400, "request.invalid_parameter", search + "?amount_min=100.0"},
		"no decimals":           {e, 400, "request.invalid_parameter", search + "?amount_max=100"},
		"not an amount":         {e, 400, "request.invalid_parameter", search + "?amount_max=1e3"},
		"a page token of other": {e, 400, "request.invalid_page_token", search + "?page_token=x"},
		"no such statement":     {e, 404, "request.not_found", "/v1/bank-statements/" + uuid.NewString() + "/payments"},
		"an agent":              {e.agent(), 403, "identity.agent_not_allowed", search},
		"a person who may read": {readOnly, 403, "identity.permission_missing", search},
	} {
		t.Run(name, func(t *testing.T) {
			c.as.in(t).Problem(c.status, c.code, "GET", c.path, "", "")
		})
	}
}

// TestSearchPaymentsDatabaseFailures: every statement of the search failing
// fails the call.
func TestSearchPaymentsDatabaseFailures(t *testing.T) {
	sc := matchStatementSetup(t)
	e := sc.e
	h := endpoints(nil)
	st := uuidOf(sc.statement)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "search", func(ctx context.Context) error {
		_, err := h.ListBankStatementPayments(ctx, gen.ListBankStatementPaymentsParams{ID: st, AmountMin: gen.NewOptString("1.00")})
		return err
	})
	_, _, err := payments.StatementPayments(context.Background(), nil, st, payments.PaymentSearch{})
	pgtest.WantErr(t, err, "")
}

// TestSearchPaymentsBudget checks 01-requirements' list budget (p95 150 ms)
// for the search over 20,000 posted payments of the bank, half naming the
// bank and half their run's: the first page, and a search that finds
// nothing, which reads every posted payment of the bank.
func TestSearchPaymentsBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	sc := matchStatementSetup(t)
	e, pay := sc.e, uuidOf(sc.pays[0].payment)
	// The payments are written straight in, each of a run of its own, as a
	// copy of a posted payment, without the row checks a release runs.
	pgtest.Exec(t, `BEGIN; SET LOCAL session_replication_role = replica;
		WITH n AS (SELECT n, gen_random_uuid() AS run FROM generate_series(1, 20000) n),
		r AS (INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount,
				status, proposed_by, decided_by, decided_at, created_by, updated_by)
			SELECT r.tenant_id, n.run, 'B' || n.n, r.legal_entity_id, CASE WHEN n.n % 2 = 0 THEN r.house_bank_id END, date '2025-01-01' + n.n / 100,
				date '2025-01-01' + n.n / 100, r.currency, 10000, 'released', r.proposed_by, r.decided_by, r.decided_at, r.created_by, r.updated_by
			FROM n, payments.payment p JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id WHERE p.id = '`+pay.String()+`')
		INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, journal_entry_year,
			journal_entry_id, created_by, house_bank_id)
		SELECT p.tenant_id, gen_random_uuid(), n.run, p.supplier_id, p.bank_account_id, 10000 + n.n, p.currency, 'B-' || lpad(n.n::text, 8, '0'),
			p.journal_entry_year, p.journal_entry_id, p.created_by, CASE WHEN n.n % 2 = 1 THEN r.house_bank_id END
		FROM n, payments.payment p JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id WHERE p.id = '`+pay.String()+`';
		COMMIT; ANALYZE payments.payment, payments.payment_run`)
	search := "/v1/bank-statements/" + sc.statement + "/payments"
	for name, c := range map[string]struct {
		query string
		items int
	}{
		"the first page":  {"?page_size=25", 25},
		"nothing matches": {"?amount_min=999999.00", 0},
	} {
		t.Run(name, func(t *testing.T) {
			var slowest time.Duration
			for range 5 {
				start := time.Now()
				page, _ := e.in(t).Must(200, "GET", search+c.query, "", "")
				slowest = max(slowest, time.Since(start))
				if got := len(page["items"].([]any)); got != c.items {
					t.Fatalf("items: got %d, want %d", got, c.items)
				}
			}
			t.Logf("search of 20,000 payments, %s: slowest of five %v", name, slowest)
			if slowest > 150*time.Millisecond {
				t.Errorf("search took %v; the budget is 150 ms", slowest)
			}
		})
	}
}
