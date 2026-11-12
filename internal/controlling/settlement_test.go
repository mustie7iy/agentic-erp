package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An order settled and closed (T60e, ADR-0064 §3).

func settleBody(year, period int) string {
	return fmt.Sprintf(`{"fiscal_year": %d, "fiscal_period": %d}`, year, period)
}

// settlementLine is a settlement as "year/period on date to receiver:
// amount currency on n account(s)".
func settlementLine(v any) string {
	s := v.(map[string]any)
	a := s["amount"].(map[string]any)
	return fmt.Sprintf("%v/%v on %v to %v: %v %v on %v account(s)", s["fiscal_year"], s["fiscal_period"], s["posting_date"], s["receiver_cost_center_code"],
		a["amount"], a["currency"], s["account_count"])
}

// settledLine is what a settlement answered: the settlement posted, or
// "nothing".
func settledLine(m map[string]any) string {
	if m["settled"] != true {
		if _, ok := m["settlement"]; ok {
			return "a settlement although nothing was settled"
		}
		return "nothing"
	}
	return settlementLine(m["settlement"])
}

// ruledOrder is a released order of company 2000 under cost center 1000
// whose settlement rule names the receiver, and its path.
func (p plan) ruledOrder(t *testing.T, code string, receiver uuid.UUID) (id, path string) {
	t.Helper()
	id = p.releasedOrder(t, code)
	path = "/v1/internal-orders/" + id
	p.Must(200, "PATCH", path+"/settlement-rule", ruleBody(receiver), `"2"`)
	return id, path
}

// entryLines reads a journal entry's lines as "account amount on order
// code" or "account amount on center code", in line order.
func (p plan) entryLines(t *testing.T, id any) []string {
	t.Helper()
	entry, _ := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", id), "", "")
	want(t, "the entry's source", fmt.Sprint(entry["source_context"], " ", entry["document_type"], " ", entry["currency"]), "controlling SA EUR")
	var out []string
	for _, l := range apitest.List(t, entry, "lines") {
		m := l.(map[string]any)
		var number string
		pgtest.Scan(t, `SELECT number FROM ledger.gl_account WHERE tenant_id = $1 AND id = $2`, []any{p.Tenant.ID, m["account_id"]}, &number)
		object := "nothing"
		if code, ok := m["internal_order_code"]; ok {
			object = fmt.Sprint("order ", code)
		} else if cc, ok := m["cost_center_id"].(string); ok {
			object = "center " + p.code(cc)
		}
		out = append(out, fmt.Sprintf("%s %v on %s", number, m["amount"].(map[string]any)["amount"], object))
	}
	return out
}

// centerCosts are the 2026 actuals of a cost center of company 2000 as
// "account amount", by account.
func (p plan) centerCosts(t *testing.T, center uuid.UUID) []string {
	t.Helper()
	var rows []ledger.AssignmentActual
	if err := apitest.InRequest(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) (err error) {
		rows, err = ledger.AssignmentBalances{}.CostCenterActuals(ctx, ledger.ActualsQuery{LegalEntityID: p.DE, FiscalYear: 2026, PeriodFrom: 1, PeriodTo: 16,
			Centers: []uuid.UUID{center}, Limit: 100})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		var number string
		pgtest.Scan(t, `SELECT number FROM ledger.gl_account WHERE tenant_id = $1 AND id = $2`, []any{p.Tenant.ID, r.AccountID}, &number)
		out = append(out, fmt.Sprintf("%s %d", number, r.Amount))
	}
	return out
}

func TestInternalOrderSettlement(t *testing.T) {
	p := setup(t)
	it := p.Account(t, "2000", "530000")
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	p.charge(t, id, "2026-06-15", p.office, "7.00")
	p.charge(t, id, "2026-06-16", it, "50.00")

	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		created := "/v1/internal-orders/" + p.order(t, "CREATED")
		p.Problem(409, "controlling.internal_order_not_released", "POST", created+":settle", settleBody(2026, 5), `"1"`)
		p.Problem(409, "controlling.internal_order_not_released", "POST", created+":close", "", `"1"`)
		p.Problem(409, "controlling.settlement_rule_missing", "POST", "/v1/internal-orders/"+p.releasedOrder(t, "NO-RULE")+":settle", settleBody(2026, 5), `"2"`)
		p.Problem(404, "request.not_found", "POST", "/v1/internal-orders/"+uuid.NewString()+":settle", settleBody(2026, 5), `"3"`)
		p.Problem(404, "request.not_found", "POST", "/v1/internal-orders/"+uuid.NewString()+":close", "", `"1"`)
		p.Problem(404, "request.not_found", "GET", "/v1/internal-orders/"+uuid.NewString()+"/settlements", "", "")
		p.Problem(422, "ledger.no_fiscal_period", "POST", path+":settle", settleBody(1999, 5), `"3"`)
		m := p.Problem(422, "controlling.period_not_regular", "POST", path+":settle", settleBody(2026, 13), `"3"`)
		want(t, "field error of a special period", reason(t, m), "fiscal_period not_regular")
		p.Problem(400, "request.invalid_body", "POST", path+":settle", settleBody(2026, 17), `"3"`)
		p.Problem(428, "request.precondition_required", "POST", path+":close", "", "")
		p.Problem(428, "request.precondition_required", "POST", path+":settle", settleBody(2026, 5), "")
		// The version before the rule was set: the receiver is not the one read.
		p.Problem(412, "request.version_mismatch", "POST", path+":settle", settleBody(2026, 5), `"2"`)
		p.Problem(412, "request.version_mismatch", "POST", path+":close", "", `"9"`)
		m = p.Problem(409, "controlling.internal_order_not_settled", "POST", path+":close", "", `"3"`)
		want(t, "why the order is not closed", m["detail"], any("The internal order still holds 107.00 EUR on account 520000 and a balance on 2 account(s) in all; settle it first."))
		page, _ := p.Must(200, "GET", path+"/settlements", "", "")
		want(t, "settlements after refusals", len(apitest.List(t, page, "items")), 0)
	})

	first, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 5), `"3"`)
	t.Run("the unsettled balance up to the period moves to the receiver", func(t *testing.T) {
		want(t, "settled up to May", settledLine(first), "2026/5 on 2026-05-31 to 2000: 100.00 EUR on 1 account(s)")
		want(t, "the order named", fmt.Sprint(first["internal_order_id"], " ", first["internal_order_code"], " ", first["internal_order_name"], " ",
			first["legal_entity_id"], " ", first["fiscal_year"], "/", first["fiscal_period"]), fmt.Sprint(id, " FAIR-26 Order FAIR-26 ", p.DE, " 2026/5"))
		s := apitest.Object(t, first, "settlement")
		want(t, "the settlement's order and receiver", fmt.Sprint(s["internal_order_id"], " ", s["receiver_cost_center_id"], " ", s["created_by"]),
			fmt.Sprint(id, " ", p.c.Sales, " ", p.Tenant.Human.ID))
		wantList(t, "the entry's lines", p.entryLines(t, s["journal_entry_id"]), []string{"520000 -100.00 on order FAIR-26", "520000 100.00 on center 2000"})
		entry, _ := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", s["journal_entry_id"]), "", "")
		want(t, "the entry's number and date", fmt.Sprint(entry["document_number"], " ", entry["posting_date"], " ", entry["fiscal_period"]),
			fmt.Sprint(s["journal_entry_number"], " 2026-05-31 5"))
		got, _ := balances(t, p.Client, id, "&fiscal_period_to=5")
		wantList(t, "the order up to May", got, []string{"520000 " + p.accountName(t, p.office) + "/5 100.00 100.00 0.00 EUR", "totals 100.00 100.00 0.00 EUR"})
		wantList(t, "the receiver's costs", p.centerCosts(t, p.c.Sales), []string{"520000 10000"})
	})
	t.Run("settling again with nothing new posts nothing", func(t *testing.T) {
		again, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 5), `"3"`)
		want(t, "settled again", settledLine(again), "nothing")
		want(t, "the period it named", fmt.Sprint(again["fiscal_year"], "/", again["fiscal_period"]), "2026/5")
		earlier, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 4), `"3"`)
		want(t, "settled up to April, before any cost", settledLine(earlier), "nothing")
		want(t, "settlements published", len(p.events(t, "internal_order_settled")), 1)
	})
	t.Run("each account's balance moves on its own account", func(t *testing.T) {
		// A cost posted into May after May was settled is unsettled again.
		p.charge(t, id, "2026-05-20", p.office, "20.00")
		june, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 6), `"3"`)
		want(t, "settled up to June", settledLine(june), "2026/6 on 2026-06-30 to 2000: 77.00 EUR on 2 account(s)")
		wantList(t, "the entry's lines", p.entryLines(t, apitest.Object(t, june, "settlement")["journal_entry_id"]), []string{
			"520000 -27.00 on order FAIR-26", "520000 27.00 on center 2000", "530000 -50.00 on order FAIR-26", "530000 50.00 on center 2000"})
		got, _ := balances(t, p.Client, id, "")
		want(t, "the order's totals", got[len(got)-1], "totals 177.00 177.00 0.00 EUR")
		wantList(t, "the receiver's costs", p.centerCosts(t, p.c.Sales), []string{"520000 12700", "530000 5000"})
	})
	t.Run("a credit balance is settled as one", func(t *testing.T) {
		if err := apitest.Committed(t, p.Tenant, p.Tenant.Human, p.chargeOrder(uuid.MustParse(id), "2026-07-01", -500)); err != nil {
			t.Fatal(err)
		}
		july, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 7), `"3"`)
		want(t, "settled up to July", settledLine(july), "2026/7 on 2026-07-31 to 2000: -5.00 EUR on 1 account(s)")
	})
	t.Run("the settlements listed, latest first", func(t *testing.T) {
		p := p.in(t)
		list := func(query string) ([]string, string) {
			page, _ := p.Must(200, "GET", path+"/settlements"+query, "", "")
			var out []string
			for _, s := range apitest.List(t, page, "items") {
				out = append(out, settlementLine(s))
			}
			token, _ := page["next_page_token"].(string)
			return out, token
		}
		all := []string{"2026/7 on 2026-07-31 to 2000: -5.00 EUR on 1 account(s)", "2026/6 on 2026-06-30 to 2000: 77.00 EUR on 2 account(s)",
			"2026/5 on 2026-05-31 to 2000: 100.00 EUR on 1 account(s)"}
		got, _ := list("")
		wantList(t, "every settlement", got, all)
		got, token := list("?page_size=2")
		wantList(t, "the first page", got, all[:2])
		got, token = list("?page_size=2&page_token=" + token)
		wantList(t, "the second page", got, all[2:])
		want(t, "a page after the last", token, "")
		got, _ = list("?order_by=posting_date%20asc&page_size=1")
		wantList(t, "the earliest first", got, all[2:])
		p.Problem(400, "request.invalid_order_by", "GET", path+"/settlements?order_by=amount", "", "")
		other, _ := p.Must(200, "GET", "/v1/internal-orders/"+p.order(t, "OTHER")+"/settlements", "", "")
		want(t, "settlements of another order", len(apitest.List(t, other, "items")), 0)
	})

	_, tag := p.Must(200, "GET", path, "", "")
	closed, closedTag := p.Must(200, "POST", path+":close", "", tag)
	t.Run("a settled order is closed, for good", func(t *testing.T) {
		p := p.in(t)
		want(t, "the closed order", fmt.Sprint(closed["status"], " ", closed["responsible_cost_center_code"], " ", closedTag), `closed 1000 "4"`)
		p.Problem(409, "controlling.internal_order_closed", "POST", path+":close", "", closedTag)
		p.Problem(409, "controlling.internal_order_closed", "POST", path+":settle", settleBody(2026, 7), closedTag)
		p.Problem(409, "controlling.internal_order_released", "POST", path+":release", "", closedTag)
		m := p.Problem(422, "ledger.internal_order_invalid", "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-07-02",
			"posting_date": "2026-07-02", "currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "1.00", "currency": "EUR"}, "internal_order_id": %q},
			{"account_id": %q, "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, p.DE, p.office, id, p.bank), "")
		want(t, "why a closed order takes no posting", reason(t, m), "lines[0].internal_order_id closed")
		wantList(t, "closed orders listed", orders(t, p.Client, "?status=closed"), []string{"FAIR-26 closed 1000"})
	})

	t.Run("published and audited", func(t *testing.T) {
		var got []string
		for _, ev := range p.events(t, "internal_order_settled") {
			got = append(got, fmt.Sprint(ev["code"], " ", ev["fiscal_year"], "/", ev["fiscal_period"], " ", ev["posting_date"], " ", ev["receiver_cost_center_code"], " ",
				ev["amount"].(map[string]any)["amount"], " ", ev["amount"].(map[string]any)["currency"], " ", ev["account_count"]))
		}
		wantList(t, "settlements published", got, []string{"FAIR-26 2026/5 2026-05-31 2000 100.00 EUR 1", "FAIR-26 2026/6 2026-06-30 2000 77.00 EUR 2",
			"FAIR-26 2026/7 2026-07-31 2000 -5.00 EUR 1"})
		last := p.events(t, "internal_order_settled")[2]
		var settlement, entry, number string
		pgtest.Scan(t, `SELECT id::text, journal_entry_id::text, document_number FROM controlling.order_settlement WHERE tenant_id = $1 AND fiscal_period = 7`,
			[]any{p.Tenant.ID}, &settlement, &entry, &number)
		want(t, "the event's settlement and entry", fmt.Sprint(last["internal_order_id"], " ", last["settlement_id"], " ", last["journal_entry_id"], " ",
			last["journal_entry_number"], " ", last["receiver_cost_center_id"]), fmt.Sprint(id, " ", settlement, " ", entry, " ", number, " ", p.c.Sales))
		var changes []string
		for _, ev := range p.events(t, "internal_order_changed") {
			if ev["code"] == "FAIR-26" {
				changes = append(changes, fmt.Sprint(ev["change"], " ", ev["status"]))
			}
		}
		wantList(t, "the order's changes published", changes, []string{"created created", "released released", "closed closed"})
		var audited string
		pgtest.Scan(t, `SELECT string_agg(replace(action, 'controlling.internal_order.', '') || coalesce(' ' || (detail->>'amount') || ' ' || (detail->>'currency')
				|| ' ' || (detail->>'journal_entry_number'), ''), '; ' ORDER BY seq)
			FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action IN ('controlling.internal_order.settle', 'controlling.internal_order.close')`,
			[]any{p.Tenant.ID, id}, &audited)
		var numbers []string
		for _, s := range []int{5, 6, 7} {
			var n string
			pgtest.Scan(t, `SELECT document_number FROM controlling.order_settlement WHERE tenant_id = $1 AND fiscal_period = $2`, []any{p.Tenant.ID, s}, &n)
			numbers = append(numbers, n)
		}
		want(t, "audited", audited, fmt.Sprintf("settle 100.00 EUR %s; settle 77.00 EUR %s; settle -5.00 EUR %s; close", numbers[0], numbers[1], numbers[2]))
	})
}

// TestInternalOrderSettlementReceiver: the receiver is checked on the
// period's last day before anything is posted, and what the ledger
// refuses of the entry is the settlement's refusal; nothing is kept.
func TestInternalOrderSettlementReceiver(t *testing.T) {
	p := setup(t)
	created, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "9300", "name": "From June", "responsible": "J",
		"category": "ADMIN", "valid_from": "2026-06-01", "reason": "r"}`, p.DE), "")
	p.Approve(t, created["pending_change_id"].(string))
	id, path := p.ruledOrder(t, "FAIR-26", uuid.MustParse(created["id"].(string)))
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	m := p.Problem(409, "controlling.settlement_receiver_not_valid", "POST", path+":settle", settleBody(2026, 5), `"3"`)
	want(t, "why the receiver is refused", m["detail"],
		any("The receiving cost center 9300 is not valid on 2026-05-31, the period's last day; replace the order's settlement rule."))
	t.Run("a receiver locked for costs is the ledger's refusal", func(t *testing.T) {
		locked, lockedPath := p.ruledOrder(t, "LOCKED", p.c.Locked)
		p.charge(t, locked, "2026-05-10", p.office, "10.00")
		p.Problem(422, "ledger.cost_center_invalid", "POST", lockedPath+":settle", settleBody(2026, 5), `"3"`)
	})
	var kept int
	pgtest.Scan(t, `SELECT count(*) FROM controlling.order_settlement WHERE tenant_id = $1`, []any{p.Tenant.ID}, &kept)
	want(t, "settlements kept", kept, 0)
	june, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 6), `"3"`)
	want(t, "settled once the receiver is valid", settledLine(june), "2026/6 on 2026-06-30 to 9300: 100.00 EUR on 1 account(s)")
}

// TestInternalOrderSettlementAccess: an order is settled and closed, and
// its settlements read, only in the companies a person may act in; to
// settle and close needs controlling.order.settle and is a person's, and
// the amounts need ledger.entry.read beside.
func TestInternalOrderSettlementAccess(t *testing.T) {
	p := setup(t)
	_, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	other.Problem(404, "request.not_found", "GET", path+"/settlements", "", "")
	other.Problem(404, "request.not_found", "POST", path+":settle", settleBody(2026, 5), `"3"`)
	other.Problem(404, "request.not_found", "POST", path+":close", "", `"3"`)
	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path+"/settlements", "", "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":settle", settleBody(2026, 5), `"3"`)
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":close", "", `"3"`)
	for _, c := range []struct{ permission, method, path, body string }{
		{"controlling.order.settle", "POST", path + ":settle", settleBody(2026, 5)}, {"controlling.order.settle", "POST", path + ":close", ""},
		{"ledger.entry.read", "POST", path + ":settle", settleBody(2026, 5)}, {"ledger.entry.read", "GET", path + "/settlements", ""},
		{"controlling.order.read", "GET", path + "/settlements", ""},
	} {
		t.Run(c.method+" "+strings.TrimPrefix(c.path, path)+" without "+c.permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, c.permission)
			p.Problem(403, "identity.permission_missing", c.method, c.path, c.body, `"3"`)
		})
	}
}

func (p plan) settle(h interface {
	SettleInternalOrder(context.Context, *gen.InternalOrderSettle, gen.SettleInternalOrderParams) (gen.SettleInternalOrderRes, error)
}, order uuid.UUID, period int32, settled *[]bool) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		res, err := h.SettleInternalOrder(ctx, &gen.InternalOrderSettle{FiscalYear: 2026, FiscalPeriod: period}, gen.SettleInternalOrderParams{ID: order, IfMatch: `"3"`})
		if err == nil {
			*settled = append(*settled, res.(*gen.InternalOrderSettlementResultHeaders).Response.Settled)
		}
		return err
	}
}

// chargeOrder posts the amount to the order on the office account against
// the bank on the day, as a context posting through the ledger's port.
func (p plan) chargeOrder(order uuid.UUID, day string, amount money.Minor) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		d, err := time.Parse(time.DateOnly, day)
		if err != nil {
			return err
		}
		_, err = postings().Post(ctx, ledger.Posting{LegalEntityID: p.DE, DocumentType: "SA", DocumentDate: d, PostingDate: d, Currency: "EUR",
			SourceContext: "controlling", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: p.office, Amount: amount, InternalOrderID: &order}, {AccountID: p.bank, Amount: -amount}}})
		return err
	}
}

// TestInternalOrderSettledOnce: two settlements of one order at once
// serialize on the order's row lock; the second finds nothing unsettled
// and posts nothing.
func TestInternalOrderSettledOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	var settled []bool
	settle := p.settle(h, uuid.MustParse(id), 5, &settled)
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, settle, settle)
	if first != nil || second != nil {
		t.Fatalf("settlements: got %v and %v, want both answered", first, second)
	}
	want(t, "first and second settled", fmt.Sprint(settled), "[true false]")
	page, _ := p.Must(200, "GET", path+"/settlements", "", "")
	want(t, "settlements kept", len(apitest.List(t, page, "items")), 1)
	got, _ := balances(t, p.Client, id, "")
	want(t, "the order's totals", got[len(got)-1], "totals 100.00 100.00 0.00 EUR")
}

// TestInternalOrderSettlementLockOrder: a settlement holds its order FOR
// UPDATE and then posts, while a posting takes its period, then its
// orders FOR SHARE (ADR-0064 As built, T60e: the lock order). A posting
// to the order that is waiting for the order while the settlement posts
// holds nothing the settlement's entry waits for: the settlement
// finishes, the posting follows, and its cost is unsettled. The other
// way round, a settlement waits for a posting under way and moves its
// cost. And a close waits for a posting under way and then refuses.
func TestInternalOrderSettlementLockOrder(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	order := uuid.MustParse(id)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	var settled []bool

	t.Run("a posting waiting for the order does not stop the settlement's entry", func(t *testing.T) {
		locked, post := make(chan struct{}), make(chan struct{})
		settlePID, settlement := apitest.Started(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			// The settlement's first lock, taken ahead of it so that the
			// posting is seen waiting before the settlement posts.
			if _, err := tx.Exec(ctx, `SELECT FROM controlling.internal_order WHERE id = $1 FOR UPDATE`, order); err != nil {
				return err
			}
			close(locked)
			<-post
			return p.settle(h, order, 5, &settled)(ctx)
		})
		holder := <-settlePID
		select {
		case <-locked:
		case err := <-settlement:
			t.Fatalf("the settlement ended before it held the order: %v", err)
		}
		postPID, posting := apitest.Started(t, p.Tenant, p.Tenant.Human, p.chargeOrder(order, "2026-05-20", 2000))
		apitest.WaitBlocked(t, <-postPID, holder, posting, "the posting to the order")
		close(post)
		if err := <-settlement; err != nil {
			t.Fatalf("the settlement: %v", err)
		}
		if err := <-posting; err != nil {
			t.Fatalf("the posting: %v", err)
		}
		want(t, "settled", fmt.Sprint(settled), "[true]")
		page, _ := p.Must(200, "GET", path+"/settlements", "", "")
		want(t, "what the settlement moved", settlementLine(apitest.List(t, page, "items")[0]), "2026/5 on 2026-05-31 to 2000: 100.00 EUR on 1 account(s)")
		got, _ := balances(t, p.Client, id, "")
		want(t, "the posting is unsettled", got[len(got)-1], "totals 120.00 100.00 20.00 EUR")
	})
	t.Run("a settlement waits for a posting under way and moves its cost", func(t *testing.T) {
		settled = nil
		posting, settlement := apitest.Serialized(t, p.Tenant, p.Tenant.Human, p.chargeOrder(order, "2026-05-21", 300), p.settle(h, order, 5, &settled))
		if posting != nil || settlement != nil {
			t.Fatalf("posting and settlement: got %v and %v, want both done", posting, settlement)
		}
		got, _ := balances(t, p.Client, id, "")
		want(t, "everything is settled", got[len(got)-1], "totals 123.00 123.00 0.00 EUR")
	})
	t.Run("a close waits for a posting under way and refuses its balance", func(t *testing.T) {
		posting, closing := apitest.Serialized(t, p.Tenant, p.Tenant.Human, p.chargeOrder(order, "2026-05-22", 100), func(ctx context.Context) error {
			_, err := h.CloseInternalOrder(ctx, gen.CloseInternalOrderParams{ID: order, IfMatch: `"3"`})
			return err
		})
		if posting != nil {
			t.Fatalf("the posting: %v", posting)
		}
		want(t, "the close", problem.From(closing).Code, "controlling.internal_order_not_settled")
	})
	t.Run("a posting waits for a close under way and finds the order closed", func(t *testing.T) {
		if err := apitest.Committed(t, p.Tenant, p.Tenant.Human, p.settle(h, order, 5, &settled)); err != nil {
			t.Fatal(err)
		}
		closing, posting := apitest.Serialized(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
			_, err := h.CloseInternalOrder(ctx, gen.CloseInternalOrderParams{ID: order, IfMatch: `"3"`})
			return err
		}, p.chargeOrder(order, "2026-05-23", 100))
		if closing != nil {
			t.Fatalf("the close: %v", closing)
		}
		got := problem.From(posting)
		if got.Code != "ledger.internal_order_invalid" || len(got.Errors) != 1 || got.Errors[0].Code != "closed" {
			t.Errorf("the posting: got %s %v, want ledger.internal_order_invalid closed", got.Code, got.Errors)
		}
	})
}
