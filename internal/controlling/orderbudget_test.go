package controlling_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// An order's budget against its actual costs (T60c, ADR-0064 §3).

func budgetBody(amount, currency string) string {
	return fmt.Sprintf(`{"budget": {"amount": %q, "currency": %q}}`, amount, currency)
}

// budgetLine is a budget response as "version budget actual remaining
// exceeded", each amount with its currency and "-" for one that is absent.
func budgetLine(m map[string]any) string {
	amount := func(k string) string {
		a, ok := m[k].(map[string]any)
		if !ok {
			return "-"
		}
		return fmt.Sprint(a["amount"], " ", a["currency"])
	}
	return fmt.Sprintf("v%v budget %s actual %s remaining %s exceeded %v", m["version"], amount("budget"), amount("actual"), amount("remaining"), m["exceeded"])
}

func TestInternalOrderBudget(t *testing.T) {
	p := setup(t)
	id := p.releasedOrder(t, "FAIR-26")
	path := "/v1/internal-orders/" + id + "/budget"

	t.Run("an order whose budget was never set", func(t *testing.T) {
		got, tag := p.in(t).Must(200, "GET", path, "", "")
		want(t, "budget", budgetLine(got), "v0 budget - actual 0.00 EUR remaining - exceeded false")
		want(t, "etag", tag, `"0"`)
		want(t, "the order named", fmt.Sprint(got["internal_order_id"], " ", got["internal_order_code"], " ", got["internal_order_name"], " ", got["legal_entity_id"]),
			fmt.Sprint(id, " FAIR-26 Order FAIR-26 ", p.DE))
		if _, ok := got["updated_at"]; ok {
			t.Errorf("updated_at of a budget never set: got %v, want none", got["updated_at"])
		}
	})
	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(428, "request.precondition_required", "PATCH", path, budgetBody("100.00", "EUR"), "")
		p.Problem(412, "request.version_mismatch", "PATCH", path, budgetBody("100.00", "EUR"), `"1"`)
		p.Problem(404, "request.not_found", "PATCH", "/v1/internal-orders/"+uuid.NewString()+"/budget", budgetBody("100.00", "EUR"), `"0"`)
		p.Problem(404, "request.not_found", "GET", "/v1/internal-orders/"+uuid.NewString()+"/budget", "", "")
		for name, c := range map[string]struct{ amount, currency, want string }{
			"another currency than the company's":       {"100.00", "USD", "budget.currency invalid"},
			"a negative amount":                         {"-0.01", "EUR", "budget.amount invalid"},
			"an amount without the currency's decimals": {"100", "EUR", "budget.amount invalid"},
			"an amount beyond what an amount holds":     {"999999999999999999.00", "EUR", "budget.amount invalid"},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.order_budget_invalid", "PATCH", path, budgetBody(c.amount, c.currency), `"0"`)
				want(t, "field error", reason(t, m), c.want)
			})
		}
	})

	set, tag := p.Must(200, "PATCH", path, budgetBody("1000.00", "EUR"), `"0"`)
	want(t, "the budget first set", budgetLine(set), "v1 budget 1000.00 EUR actual 0.00 EUR remaining 1000.00 EUR exceeded false")
	want(t, "etag of the budget first set", tag, `"1"`)
	want(t, "who set it", set["updated_by"], any(p.Tenant.Human.ID.String()))
	entry, _ := p.charge(t, id, "2026-05-10", p.office, "400.00")
	// The order's actual is every year's: an entry of 2025 counts. The
	// fixture sets up 2026 alone, so the entry is a copy of the one of
	// 2026, over 50.00.
	pgtest.Exec(t, `INSERT INTO ledger.fiscal_period (tenant_id, calendar_id, fiscal_year, period, start_date, end_date)
		SELECT tenant_id, calendar_id, 2025, 12, '2025-12-01', '2025-12-31' FROM ledger.fiscal_period WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 12`,
		p.Tenant.ID)
	pgtest.Exec(t, `INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
		SELECT tenant_id, gen_random_uuid(), legal_entity_id, ledger_id, calendar_id, 2025, 12, 'open', updated_by
		FROM ledger.period_status WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 12`, p.Tenant.ID)
	pgtest.Exec(t, `WITH copy AS (INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
				document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
			SELECT tenant_id, 2025, gen_random_uuid(), legal_entity_id, ledger_id, 12, document_type, 'Y2025', '2025-12-10', '2025-12-10', currency_tc,
				currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, 'Y2025', source_context, posted_by
			FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2 RETURNING id)
		INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date, account_id,
			amount_tc, amount_lc, amount_gc, profit_center_id, segment_id, internal_order_id)
		SELECT l.tenant_id, 2025, copy.id, l.line_no, l.legal_entity_id, l.ledger_id, 12, '2025-12-10', l.account_id,
			CASE WHEN l.internal_order_id IS NULL THEN -5000 ELSE 5000 END, CASE WHEN l.internal_order_id IS NULL THEN -5000 ELSE 5000 END,
			CASE WHEN l.internal_order_id IS NULL THEN -5000 ELSE 5000 END, l.profit_center_id, l.segment_id, l.internal_order_id
		FROM ledger.journal_line l, copy WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, p.Tenant.ID, entry["id"])
	got, tag := p.Must(200, "GET", path, "", "")
	want(t, "what remains after postings", budgetLine(got), "v1 budget 1000.00 EUR actual 450.00 EUR remaining 550.00 EUR exceeded false")
	want(t, "etag read", tag, `"1"`)

	t.Run("a budget below the actual is shown as exceeded and refuses no posting", func(t *testing.T) {
		lowered, tag := p.Must(200, "PATCH", path, budgetBody("300.00", "EUR"), tag)
		want(t, "the lowered budget", budgetLine(lowered), "v2 budget 300.00 EUR actual 450.00 EUR remaining -150.00 EUR exceeded true")
		want(t, "etag", tag, `"2"`)
		p.Problem(412, "request.version_mismatch", "PATCH", path, budgetBody("5000.00", "EUR"), `"1"`)
		p.charge(t, id, "2026-06-10", p.office, "25.00")
		got, _ := p.Must(200, "GET", path, "", "")
		want(t, "after a posting beyond the budget", budgetLine(got), "v2 budget 300.00 EUR actual 475.00 EUR remaining -175.00 EUR exceeded true")
		exact, _ := p.Must(200, "PATCH", path, budgetBody("475.00", "EUR"), tag)
		want(t, "a budget equal to the actual", budgetLine(exact), "v3 budget 475.00 EUR actual 475.00 EUR remaining 0.00 EUR exceeded false")
	})
	t.Run("an order not yet released takes a budget, zero included", func(t *testing.T) {
		p := p.in(t)
		created := "/v1/internal-orders/" + p.order(t, "PLANNED") + "/budget"
		zero, _ := p.Must(200, "PATCH", created, budgetBody("0.00", "EUR"), `"0"`)
		want(t, "a budget of zero", budgetLine(zero), "v1 budget 0.00 EUR actual 0.00 EUR remaining 0.00 EUR exceeded false")
	})

	var changes []string
	for _, ev := range p.events(t, "internal_order_budget_changed") {
		previous := "-"
		if b, ok := ev["previous_budget"].(map[string]any); ok {
			previous = fmt.Sprint(b["amount"], " ", b["currency"])
		}
		b := ev["budget"].(map[string]any)
		changes = append(changes, fmt.Sprintf("%v %v v%v: %s -> %v %v", ev["code"], ev["name"], ev["version"], previous, b["amount"], b["currency"]))
	}
	wantList(t, "events", changes, []string{"FAIR-26 Order FAIR-26 v1: - -> 1000.00 EUR", "FAIR-26 Order FAIR-26 v2: 1000.00 EUR -> 300.00 EUR",
		"FAIR-26 Order FAIR-26 v3: 300.00 EUR -> 475.00 EUR", "PLANNED Order PLANNED v1: - -> 0.00 EUR"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg(coalesce(detail->>'budget_before', '-') || ' -> ' || (detail->>'budget_after') || ' ' || (detail->>'currency'), '; ' ORDER BY seq)
		FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.internal_order.set_budget' AND object_id = $2`, []any{p.Tenant.ID, id}, &audited)
	want(t, "audited amounts before and after", audited, "- -> 1000.00 EUR; 1000.00 -> 300.00 EUR; 300.00 -> 475.00 EUR")
}

// TestInternalOrderBudgetAccess: the budget is read and set only in the
// companies a person may act in, set only by a person holding
// controlling.order.budget, and read only with the ledger's entry read,
// since the actual is a ledger amount.
func TestInternalOrderBudgetAccess(t *testing.T) {
	p := setup(t)
	path := "/v1/internal-orders/" + p.order(t, "FAIR-26") + "/budget"
	body := budgetBody("100.00", "EUR")
	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "PATCH", path, body, `"0"`)
	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path, "", "")
	agent.Problem(403, "identity.agent_not_allowed", "PATCH", path, body, `"0"`)
	for _, c := range []struct{ permission, method string }{
		{"controlling.order.budget", "PATCH"}, {"ledger.entry.read", "PATCH"}, {"ledger.entry.read", "GET"}, {"controlling.order.read", "GET"},
	} {
		t.Run(c.method+" without "+c.permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, c.permission)
			p.Problem(403, "identity.permission_missing", c.method, path, body, `"0"`)
		})
	}
}

// TestInternalOrderBudgetUpdateOnce: two changes at the version both read
// serialize on the order's row lock; the second finds the budget moved
// and is refused, and one change is published.
func TestInternalOrderBudgetUpdateOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := uuid.MustParse(p.order(t, "FAIR-26"))
	set := func(amount string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.UpdateInternalOrderBudget(ctx, &gen.InternalOrderBudgetUpdate{Budget: gen.Money{Amount: amount, Currency: "EUR"}},
				gen.UpdateInternalOrderBudgetParams{ID: id, IfMatch: `"0"`})
			return err
		}
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, set("100.00"), set("200.00"))
	if first != nil {
		t.Fatalf("first change: %v", first)
	}
	want(t, "second change", problem.From(second).Code, "request.version_mismatch")
	want(t, "changes published", len(p.events(t, "internal_order_budget_changed")), 1)
	got, _ := p.Must(200, "GET", "/v1/internal-orders/"+id.String()+"/budget", "", "")
	want(t, "the budget kept", budgetLine(got), "v1 budget 100.00 EUR actual 0.00 EUR remaining 100.00 EUR exceeded false")
}

// TestInternalOrderBudgetBeyondAnAmount: what remains of a budget that an
// amount cannot hold is an error, never a wrong figure.
func TestInternalOrderBudgetBeyondAnAmount(t *testing.T) {
	p := setup(t)
	id := p.releasedOrder(t, "FAIR-26")
	// A credit to the order alone: its actual is below zero.
	p.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-05-10", "posting_date": "2026-05-10", "currency": "EUR",
		"lines": [{"account_id": %q, "amount": {"amount": "-1.00", "currency": "EUR"}, "internal_order_id": %q},
			{"account_id": %q, "amount": {"amount": "1.00", "currency": "EUR"}}]}`, p.DE, p.office, id, p.bank), "")
	pgtest.Exec(t, `INSERT INTO controlling.order_budget (tenant_id, internal_order_id, amount, created_by, updated_by)
		VALUES ($1, $2, 9223372036854775807, $3, $3)`, p.Tenant.ID, id, p.Tenant.Human.ID)
	p.Problem(500, "platform.internal", "GET", "/v1/internal-orders/"+id+"/budget", "", "")
}
