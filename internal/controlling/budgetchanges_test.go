package controlling_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Budget supplements and returns, and availability control (T60h,
// ADR-0064 §3).

func changeBody(kind, amount, currency, text string) string {
	if text == "" {
		return fmt.Sprintf(`{"kind": %q, "amount": {"amount": %q, "currency": %q}}`, kind, amount, currency)
	}
	return fmt.Sprintf(`{"kind": %q, "amount": {"amount": %q, "currency": %q}, "text": %q}`, kind, amount, currency, text)
}

// changeLine is a budget change as "vN kind before -> after (change)
// limits text".
func changeLine(m map[string]any) string {
	amount := func(k string) string {
		a, ok := m[k].(map[string]any)
		if !ok {
			return "-"
		}
		return fmt.Sprint(a["amount"])
	}
	limits := "-"
	if c, ok := m["availability_control"].(map[string]any); ok {
		limits = fmt.Sprintf("%v/%v", c["warn_percent"], c["refuse_percent"])
	}
	text, _ := m["text"].(string)
	return fmt.Sprintf("v%v %v %s -> %s (%s) %s %s", m["version"], m["kind"], amount("budget_before"), amount("budget"), amount("change"), limits, text)
}

// availabilityLine is a budget's read as "budget actual consumed%
// limits availability".
func availabilityLine(m map[string]any) string {
	limits, state := "-", "-"
	if c, ok := m["availability_control"].(map[string]any); ok {
		limits = fmt.Sprintf("%v/%v", c["warn_percent"], c["refuse_percent"])
		state = fmt.Sprint(m["availability"])
	}
	consumed, ok := m["consumed_percent"]
	if !ok {
		consumed = "-"
	}
	return fmt.Sprintf("budget %v actual %v consumed %v limits %s %s", m["budget"].(map[string]any)["amount"], m["actual"].(map[string]any)["amount"],
		consumed, limits, state)
}

// credit posts the amount in EUR to the order as a credit on the office
// account, against the bank, on the day.
func (p plan) credit(t *testing.T, order, day, amount string) {
	t.Helper()
	p.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR",
		"lines": [{"account_id": %q, "amount": {"amount": "-%s", "currency": "EUR"}, "internal_order_id": %q},
			{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}}]}`, p.DE, day, day, p.office, amount, order, p.bank, amount), "")
}

func TestInternalOrderBudgetChanges(t *testing.T) {
	p := setup(t)
	id := p.releasedOrder(t, "FAIR-26")
	path := "/v1/internal-orders/" + id + "/budget"
	changes := "/v1/internal-orders/" + id + "/budget-changes"
	p.Problem(409, "controlling.order_budget_missing", "POST", changes, changeBody("supplement", "10.00", "EUR", ""), `"0"`)
	_, tag := p.Must(200, "PATCH", path, budgetBody("1000.00", "EUR"), `"0"`)
	p.charge(t, id, "2026-05-10", p.office, "400.00")

	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(428, "request.precondition_required", "POST", changes, changeBody("supplement", "10.00", "EUR", ""), "")
		p.Problem(412, "request.version_mismatch", "POST", changes, changeBody("supplement", "10.00", "EUR", ""), `"0"`)
		p.Problem(404, "request.not_found", "POST", "/v1/internal-orders/"+uuid.NewString()+"/budget-changes", changeBody("supplement", "1.00", "EUR", ""), `"1"`)
		for name, c := range map[string]struct{ kind, amount, currency, want string }{
			"a kind that is none":           {"transfer", "1.00", "EUR", "kind unsupported"},
			"another currency":              {"supplement", "1.00", "USD", "amount.currency invalid"},
			"a zero amount":                 {"supplement", "0.00", "EUR", "amount.amount invalid"},
			"a negative return":             {"return", "-1.00", "EUR", "amount.amount invalid"},
			"an amount without its digits":  {"supplement", "1", "EUR", "amount.amount invalid"},
			"a supplement beyond an amount": {"supplement", "92233720368547758.07", "EUR", "amount.amount invalid"},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.order_budget_invalid", "POST", changes, changeBody(c.kind, c.amount, c.currency, ""), tag)
				want(t, "field error", reason(t, m), c.want)
			})
		}
		m := p.Problem(409, "controlling.order_budget_return_too_large", "POST", changes, changeBody("return", "600.01", "EUR", ""), tag)
		want(t, "what may be returned", m["detail"], any("At most 600.00 EUR of the budget may be returned: the budget less the order's actual costs."))
	})

	supplemented, tag := p.Must(201, "POST", changes, changeBody("supplement", "250.00", "EUR", "Second hall"), tag)
	want(t, "the supplement", changeLine(supplemented), "v2 supplement 1000.00 -> 1250.00 (250.00) - Second hall")
	want(t, "its etag, the budget's version", tag, `"2"`)
	returned, tag := p.Must(201, "POST", changes, changeBody("return", "850.00", "EUR", ""), tag)
	want(t, "a return down to the actual", changeLine(returned), "v3 return 1250.00 -> 400.00 (-850.00) - ")
	got, _ := p.Must(200, "GET", path, "", "")
	want(t, "the budget after them", budgetLine(got), "v3 budget 400.00 EUR actual 400.00 EUR remaining 0.00 EUR exceeded false")
	p.Problem(409, "controlling.order_budget_return_too_large", "POST", changes, changeBody("return", "0.01", "EUR", ""), tag)

	t.Run("a credit balance leaves the whole budget to return", func(t *testing.T) {
		p := p.in(t)
		credited := p.releasedOrder(t, "CREDITED")
		p.Must(200, "PATCH", "/v1/internal-orders/"+credited+"/budget", budgetBody("10.00", "EUR"), `"0"`)
		p.credit(t, credited, "2026-05-10", "5.00")
		all, _ := p.Must(201, "POST", "/v1/internal-orders/"+credited+"/budget-changes", changeBody("return", "10.00", "EUR", ""), `"1"`)
		want(t, "the whole budget returned", changeLine(all), "v2 return 10.00 -> 0.00 (-10.00) - ")
	})

	p.Problem(400, "request.invalid_order_by", "GET", changes+"?order_by=amount", "", "")
	page, _ := p.Must(200, "GET", changes+"?page_size=2", "", "")
	var lines []string
	for _, it := range apitest.List(t, page, "items") {
		lines = append(lines, changeLine(it.(map[string]any)))
	}
	next, _ := p.Must(200, "GET", changes+"?page_size=2&page_token="+page["next_page_token"].(string), "", "")
	for _, it := range apitest.List(t, next, "items") {
		lines = append(lines, changeLine(it.(map[string]any)))
	}
	wantList(t, "the budget's history, latest first", lines, []string{"v3 return 1250.00 -> 400.00 (-850.00) - ", "v2 supplement 1000.00 -> 1250.00 (250.00) - Second hall",
		"v1 set - -> 1000.00 (1000.00) - "})

	var kinds []string
	for _, ev := range p.events(t, "internal_order_budget_changed") {
		if ev["code"] == "FAIR-26" {
			kinds = append(kinds, fmt.Sprintf("v%v %v %v", ev["version"], ev["kind"], ev["text"]))
		}
	}
	wantList(t, "events", kinds, []string{"v1 set <nil>", "v2 supplement Second hall", "v3 return <nil>"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg(action || ' ' || (detail->>'kind') || ' ' || coalesce(detail->>'budget_before', '-') || ' -> ' || (detail->>'budget_after'), '; ' ORDER BY seq)
		FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'controlling.internal_order.%_budget' AND object_id = $2`, []any{p.Tenant.ID, id}, &audited)
	want(t, "audited", audited, "controlling.internal_order.set_budget set - -> 1000.00; controlling.internal_order.supplement_budget supplement 1000.00 -> 1250.00; "+
		"controlling.internal_order.return_budget return 1250.00 -> 400.00")
}

// TestInternalOrderBudgetChangesAccess: changes are made only by a person
// holding controlling.order.budget and the ledger's entry read, in the
// companies the person may act in, and read with controlling.order.read.
func TestInternalOrderBudgetChangesAccess(t *testing.T) {
	p := setup(t)
	id := p.order(t, "FAIR-26")
	p.Must(200, "PATCH", "/v1/internal-orders/"+id+"/budget", budgetBody("100.00", "EUR"), `"0"`)
	path := "/v1/internal-orders/" + id + "/budget-changes"
	body := changeBody("supplement", "1.00", "EUR", "")
	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "POST", path, body, `"1"`)
	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path, "", "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", path, body, `"1"`)
	for _, c := range []struct{ permission, method string }{
		{"controlling.order.budget", "POST"}, {"ledger.entry.read", "POST"}, {"controlling.order.read", "GET"},
	} {
		t.Run(c.method+" without "+c.permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, c.permission)
			p.Problem(403, "identity.permission_missing", c.method, path, body, `"1"`)
		})
	}
}

func TestInternalOrderAvailabilityControl(t *testing.T) {
	p := setup(t)
	id := p.releasedOrder(t, "FAIR-26")
	path := "/v1/internal-orders/" + id + "/budget"
	limits := func(warn, refuse string) string {
		return fmt.Sprintf(`{"availability_control": {"warn_percent": %s, "refuse_percent": %s}}`, warn, refuse)
	}
	p.Problem(422, "controlling.order_budget_invalid", "PATCH", path, limits("80", "100"), `"0"`)
	_, tag := p.Must(200, "PATCH", path, budgetBody("1000.00", "EUR"), `"0"`)

	t.Run("refusals of the limits", func(t *testing.T) {
		p := p.in(t)
		for name, c := range map[string]struct{ body, want string }{
			"neither field":               {`{}`, "budget invalid"},
			"a warning at the refusal":    {limits("100", "100"), "availability_control.warn_percent invalid"},
			"a warning above the refusal": {`{"budget": {"amount": "5.00", "currency": "EUR"}, "availability_control": {"warn_percent": 101, "refuse_percent": 100}}`, "availability_control.warn_percent invalid"},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.order_budget_invalid", "PATCH", path, c.body, tag)
				want(t, "field error", reason(t, m), c.want)
			})
		}
		p.Problem(400, "request.invalid_body", "PATCH", path, limits("0", "100"), tag)
		p.Problem(400, "request.invalid_body", "PATCH", path, `{"availability_control": {}}`, tag)
		p.Problem(400, "request.invalid_body", "PATCH", path, limits("80", "1000"), tag)
	})

	set, tag := p.Must(200, "PATCH", path, limits("50", "100"), tag)
	want(t, "the limits set", availabilityLine(set), "budget 1000.00 actual 0.00 consumed 0.00 limits 50/100 within")
	p.charge(t, id, "2026-05-10", p.office, "500.00")
	got, _ := p.Must(200, "GET", path, "", "")
	want(t, "at the warning limit", availabilityLine(got), "budget 1000.00 actual 500.00 consumed 50.00 limits 50/100 within")
	want(t, "no warning at the limit", len(p.events(t, "internal_order_budget_warned")), 0)
	above, _ := p.charge(t, id, "2026-05-11", p.office, "100.00")
	got, _ = p.Must(200, "GET", path, "", "")
	want(t, "above the warning limit", availabilityLine(got), "budget 1000.00 actual 600.00 consumed 60.00 limits 50/100 above_warning")
	warned := p.events(t, "internal_order_budget_warned")
	if len(warned) != 1 {
		t.Fatalf("warnings: got %d, want 1", len(warned))
	}
	w := warned[0]
	want(t, "the warning", fmt.Sprintf("%v %v %v %v %v %v", w["code"], w["budget"].(map[string]any)["amount"], w["consumption"].(map[string]any)["amount"],
		w["posted"].(map[string]any)["amount"], w["consumed_percent"], w["warn_percent"]), "FAIR-26 1000.00 600.00 100.00 60.00 50")
	want(t, "the warning names the entry", w["journal_entry_id"], above["id"])

	m := p.Problem(422, "ledger.internal_order_invalid", "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-05-12",
		"posting_date": "2026-05-12", "currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "-400.01", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "400.01", "currency": "EUR"}, "internal_order_id": %q}]}`, p.DE, p.bank, p.office, id), "")
	want(t, "a posting above the refusal limit", reason(t, m), "lines[1].internal_order_id budget_exceeded")
	p.charge(t, id, "2026-05-12", p.office, "400.00")
	got, tag = p.Must(200, "GET", path, "", "")
	want(t, "up to the refusal limit", availabilityLine(got), "budget 1000.00 actual 1000.00 consumed 100.00 limits 50/100 above_warning")

	t.Run("a credit is never refused", func(t *testing.T) {
		p := p.in(t)
		p.credit(t, id, "2026-05-13", "10.00")
		p.charge(t, id, "2026-05-13", p.office, "10.00")
	})
	tolerated, tag := p.Must(200, "PATCH", path, limits("90", "110"), tag)
	want(t, "a refusal limit above the budget tolerates more", availabilityLine(tolerated), "budget 1000.00 actual 1000.00 consumed 100.00 limits 90/110 above_warning")
	p.charge(t, id, "2026-05-14", p.office, "100.00")
	lowered, tag := p.Must(200, "PATCH", path, budgetBody("500.00", "EUR"), tag)
	want(t, "a budget lowered below the consumption keeps its limits", availabilityLine(lowered), "budget 500.00 actual 1100.00 consumed 220.00 limits 90/110 above_refusal")
	refuseOnly, tag := p.Must(200, "PATCH", path, `{"budget": {"amount": "5000.00", "currency": "EUR"}, "availability_control": {"refuse_percent": 50}}`, tag)
	want(t, "a refusal limit alone", availabilityLine(refuseOnly), "budget 5000.00 actual 1100.00 consumed 22.00 limits <nil>/50 within")
	warnOnly, tag := p.Must(200, "PATCH", path, `{"availability_control": {"warn_percent": 10}}`, tag)
	want(t, "a warning limit alone", availabilityLine(warnOnly), "budget 5000.00 actual 1100.00 consumed 22.00 limits 10/<nil> above_warning")
	removed, tag := p.Must(200, "PATCH", path, `{"availability_control": null}`, tag)
	want(t, "the control removed", availabilityLine(removed), "budget 5000.00 actual 1100.00 consumed 22.00 limits - -")
	p.charge(t, id, "2026-05-15", p.office, "4000.00")
	zero, _ := p.Must(200, "PATCH", path, `{"budget": {"amount": "0.00", "currency": "EUR"}, "availability_control": {"warn_percent": 1, "refuse_percent": 999}}`, tag)
	want(t, "a budget of zero has no percent", availabilityLine(zero), "budget 0.00 actual 5100.00 consumed - limits 1/999 above_refusal")

	page, _ := p.Must(200, "GET", "/v1/internal-orders/"+id+"/budget-changes?page_size=3", "", "")
	var lines []string
	for _, it := range apitest.List(t, page, "items") {
		lines = append(lines, changeLine(it.(map[string]any)))
	}
	wantList(t, "the limits' history", lines, []string{"v8 set 5000.00 -> 0.00 (-5000.00) 1/999 ", "v7 set 5000.00 -> 5000.00 (0.00) - ",
		"v6 set 5000.00 -> 5000.00 (0.00) 10/<nil> "})
}

// TestInternalOrderAvailabilityZeroBudget: a budget of zero with a
// warning limit warns of any cost, and its warning has no percent.
func TestInternalOrderAvailabilityZeroBudget(t *testing.T) {
	p := setup(t)
	id := p.releasedOrder(t, "FAIR-26")
	p.Must(200, "PATCH", "/v1/internal-orders/"+id+"/budget", `{"budget": {"amount": "0.00", "currency": "EUR"}, "availability_control": {"warn_percent": 50}}`, `"0"`)
	p.charge(t, id, "2026-05-10", p.office, "1.00")
	warned := p.events(t, "internal_order_budget_warned")
	if len(warned) != 1 {
		t.Fatalf("warnings: got %d, want 1", len(warned))
	}
	if _, ok := warned[0]["consumed_percent"]; ok {
		t.Errorf("consumed_percent of a budget of zero: got %v, want none", warned[0]["consumed_percent"])
	}
}

// TestInternalOrderAvailabilitySerialized: two postings to one order at
// once, each within the refusal limit alone and above it together, are
// checked one after the other on the budget's row lock: the second reads
// the first's cost and is refused.
func TestInternalOrderAvailabilitySerialized(t *testing.T) {
	p := setup(t)
	id := p.releasedOrder(t, "FAIR-26")
	p.Must(200, "PATCH", "/v1/internal-orders/"+id+"/budget", `{"budget": {"amount": "100.00", "currency": "EUR"}, "availability_control": {"refuse_percent": 100}}`, `"0"`)
	post := p.chargeOrder(uuid.MustParse(id), "2026-05-10", money.Minor(6000))
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, post, post)
	if first != nil {
		t.Fatalf("first posting: %v", first)
	}
	want(t, "second posting", problem.From(second).Code, "ledger.internal_order_invalid")
	if errs := problem.From(second).Errors; len(errs) != 1 || errs[0].Code != "budget_exceeded" {
		t.Errorf("why the second posting is refused: got %v, want budget_exceeded", errs)
	}
	got, _ := p.Must(200, "GET", "/v1/internal-orders/"+id+"/budget", "", "")
	want(t, "the order's consumption", availabilityLine(got), "budget 100.00 actual 60.00 consumed 60.00 limits <nil>/100 within")
}

// TestInternalOrderAvailabilityNotOnSettlement: a settlement credits the
// order and is never refused for its budget, even one above its refusal
// limit.
func TestInternalOrderAvailabilityNotOnSettlement(t *testing.T) {
	p := setup(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	p.Must(200, "PATCH", path+"/budget", `{"budget": {"amount": "10.00", "currency": "EUR"}, "availability_control": {"refuse_percent": 100}}`, `"0"`)
	res, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 5), `"3"`)
	want(t, "settled", res["settled"], any(true))
}

// TestInternalOrderConsumptionBeyondAnAmount: what a posting adds to an
// order that an amount cannot hold is an error, never a wrong figure.
func TestInternalOrderConsumptionBeyondAnAmount(t *testing.T) {
	p := setup(t)
	order := uuid.MustParse(p.releasedOrder(t, "FAIR-26"))
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	err := apitest.InRequest(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: p.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "EUR",
			SourceContext: "controlling", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: p.office, Amount: math.MaxInt64, InternalOrderID: &order}, {AccountID: p.office, Amount: math.MaxInt64, InternalOrderID: &order},
				{AccountID: p.bank, Amount: -math.MaxInt64}, {AccountID: p.bank, Amount: -math.MaxInt64}}})
		return err
	})
	if err == nil || problem.From(err).Status != 500 {
		t.Errorf("a posting beyond an amount: got %v, want an internal error", err)
	}
}
