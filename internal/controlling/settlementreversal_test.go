package controlling_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A settlement reversed (T60i, ADR-0064 §3).

// reversedLine is a settlement as settlementLine gives it, then
// "reversed by <number>" or "in force".
func reversedLine(v any) string {
	s := v.(map[string]any)
	if n, ok := s["reversal_journal_entry_number"]; ok {
		return settlementLine(v) + " reversed by " + fmt.Sprint(n)
	}
	return settlementLine(v) + " in force"
}

// settlements lists the order's settlements, latest first.
func (p plan) settlements(t *testing.T, path string) []string {
	t.Helper()
	page, _ := p.Must(200, "GET", path+"/settlements", "", "")
	var out []string
	for _, s := range apitest.List(t, page, "items") {
		out = append(out, reversedLine(s))
	}
	return out
}

// settledFor settles the order up to the period and returns the
// settlement's id.
func (p plan) settledFor(t *testing.T, path string, period int) string {
	t.Helper()
	res, _ := p.Must(200, "POST", path+":settle", settleBody(2026, period), `"3"`)
	return apitest.Object(t, res, "settlement")["id"].(string)
}

func TestInternalOrderSettlementReversed(t *testing.T) {
	p := setup(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	may := p.settledFor(t, path, 5)
	p.charge(t, id, "2026-06-10", p.office, "50.00")
	june := p.settledFor(t, path, 6)
	reverse := func(settlement string) string { return path + "/settlements/" + settlement + ":reverse" }

	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(428, "request.precondition_required", "POST", reverse(june), "", "")
		p.Problem(412, "request.version_mismatch", "POST", reverse(june), "", `"2"`)
		p.Problem(404, "request.not_found", "POST", reverse(uuid.NewString()), "", `"3"`)
		other, otherPath := p.ruledOrder(t, "OTHER", p.c.Sales)
		p.charge(t, other, "2026-05-10", p.office, "1.00")
		p.Problem(404, "request.not_found", "POST", otherPath+"/settlements/"+june+":reverse", "", `"3"`)
		p.Problem(404, "request.not_found", "POST", "/v1/internal-orders/"+uuid.NewString()+"/settlements/"+june+":reverse", "", `"3"`)
		p.Problem(409, "controlling.settlement_not_latest", "POST", reverse(may), "", `"3"`)
	})

	reversed, tag := p.Must(200, "POST", reverse(june), "", `"3"`)
	want(t, "the reversed settlement", reversedLine(reversed), fmt.Sprintf("2026/6 on 2026-06-30 to 2000: 50.00 EUR on 1 account(s) reversed by %v",
		reversed["reversal_journal_entry_number"]))
	want(t, "the order's version, unchanged", tag, `"3"`)
	want(t, "who reversed it", reversed["reversed_by"], any(p.Tenant.Human.ID.String()))
	want(t, "the reversal's entry", fmt.Sprint(p.entryLines(t, reversed["reversal_journal_entry_id"])), "[520000 50.00 on order FAIR-26 520000 -50.00 on center 2000]")
	entry, _ := p.Must(200, "GET", fmt.Sprint("/v1/journal-entries/", reversed["reversal_journal_entry_id"]), "", "")
	want(t, "the reversal's date", entry["posting_date"], any("2026-06-30"))
	got, _ := balances(t, p.Client, id, "")
	want(t, "the order holds June's cost again", got[len(got)-1], "totals 200.00 150.00 50.00 EUR")
	p.Problem(409, "controlling.settlement_reversed", "POST", reverse(june), "", `"3"`)
	p.Problem(409, "ledger.entry_owned_elsewhere", "POST", fmt.Sprint("/v1/journal-entries/", reversed["journal_entry_id"], ":reverse"), "", "*")

	t.Run("May's settlement is the latest in force now, and is reversed in turn", func(t *testing.T) {
		p.Must(200, "POST", reverse(may), "", `"3"`)
		got, _ := balances(t, p.Client, id, "")
		want(t, "the order holds both costs", got[len(got)-1], "totals 300.00 150.00 150.00 EUR")
	})
	t.Run("a reversed settlement no longer bounds the next", func(t *testing.T) {
		res, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 5), `"3"`)
		want(t, "May settled again", settledLine(res), "2026/5 on 2026-05-31 to 2000: 100.00 EUR on 1 account(s)")
		res, _ = p.Must(200, "POST", path+":settle", settleBody(2026, 6), `"3"`)
		want(t, "June settled again", settledLine(res), "2026/6 on 2026-06-30 to 2000: 50.00 EUR on 1 account(s)")
	})
	lines := p.settlements(t, path)
	want(t, "settlements kept", len(lines), 4)
	var events []string
	for _, ev := range p.events(t, "internal_order_settlement_reversed") {
		events = append(events, fmt.Sprintf("%v %v/%v %v %v", ev["code"], ev["fiscal_year"], ev["fiscal_period"], ev["amount"].(map[string]any)["amount"],
			ev["receiver_cost_center_code"]))
	}
	wantList(t, "events", events, []string{"FAIR-26 2026/6 50.00 2000", "FAIR-26 2026/5 100.00 2000"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg(detail->>'fiscal_period' || ' ' || (detail->>'amount'), '; ' ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'controlling.internal_order.reverse_settlement' AND object_id = $2`, []any{p.Tenant.ID, id}, &audited)
	want(t, "audited", audited, "6 50.00; 5 100.00")

	t.Run("a closed order's settlement is not reversed", func(t *testing.T) {
		p.Must(200, "POST", path+":close", "", `"3"`)
		p.Problem(409, "controlling.internal_order_closed", "POST", reverse(june), "", `"4"`)
	})
}

// TestInternalOrderSettlementReversalAccess: a person holding
// controlling.order.settle and the ledger's entry read reverses, in the
// companies the person may act in; agents never do.
func TestInternalOrderSettlementReversalAccess(t *testing.T) {
	p := setup(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	p.charge(t, id, "2026-05-10", p.office, "1.00")
	reverse := path + "/settlements/" + p.settledFor(t, path, 5) + ":reverse"
	p.as(p.Reader(t, "Reader of 1000", p.US)).Problem(404, "request.not_found", "POST", reverse, "", `"3"`)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	p.As(p.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", reverse, "", `"3"`)
	for _, permission := range []string{"controlling.order.settle", "ledger.entry.read"} {
		t.Run("without "+permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, permission)
			p.Problem(403, "identity.permission_missing", "POST", reverse, "", `"3"`)
		})
	}
}

// TestInternalOrderSettlementReversedOnce: two reversals of one
// settlement at once serialize; the second finds it reversed.
func TestInternalOrderSettlementReversedOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	settlement := uuid.MustParse(p.settledFor(t, path, 5))
	reverse := func(ctx context.Context) error {
		_, err := h.ReverseInternalOrderSettlement(ctx, gen.ReverseInternalOrderSettlementParams{ID: uuid.MustParse(id), SettlementID: settlement, IfMatch: `"3"`})
		return err
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, reverse, reverse)
	if first != nil {
		t.Fatalf("first reversal: %v", first)
	}
	want(t, "second reversal", problem.From(second).Code, "controlling.settlement_reversed")
	got, _ := balances(t, p.Client, id, "")
	want(t, "the order's totals", got[len(got)-1], "totals 200.00 100.00 100.00 EUR")
}

// TestInternalOrderSettlementReversalLockOrder: a reversal takes the
// settlement entry's lock before the order (ADR-0064 As built, T60i), as
// the ledger's own reversal of an entry naming the order does. While it
// waits for the entry's lock it holds no lock on the order: a posting to
// the order goes through.
func TestInternalOrderSettlementReversalLockOrder(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id, path := p.ruledOrder(t, "FAIR-26", p.c.Sales)
	order := uuid.MustParse(id)
	p.charge(t, id, "2026-05-10", p.office, "100.00")
	settled, _ := p.Must(200, "POST", path+":settle", settleBody(2026, 5), `"3"`)
	s := apitest.Object(t, settled, "settlement")
	entry, settlement := uuid.MustParse(s["journal_entry_id"].(string)), uuid.MustParse(s["id"].(string))
	release := make(chan struct{})
	goOn := sync.OnceFunc(func() { close(release) })
	defer goOn()
	locked := make(chan struct{})
	holderPID, holder := apitest.Started(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		if err := (ledger.Postings{}).LockEntries(ctx, []uuid.UUID{entry}); err != nil {
			return err
		}
		close(locked)
		<-release
		return nil
	})
	holding := <-holderPID
	<-locked
	revPID, reversal := apitest.Started(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ReverseInternalOrderSettlement(ctx, gen.ReverseInternalOrderSettlementParams{ID: order, SettlementID: settlement, IfMatch: `"3"`})
		return err
	})
	apitest.WaitBlocked(t, <-revPID, holding, reversal, "the reversal")
	if err := apitest.Committed(t, p.Tenant, p.Tenant.Human, p.chargeOrder(order, "2026-05-20", 2000)); err != nil {
		t.Fatalf("a posting to the order while the reversal waits: %v", err)
	}
	goOn()
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if err := <-reversal; err != nil {
		t.Fatalf("the reversal: %v", err)
	}
	got, _ := balances(t, p.Client, id, "")
	want(t, "the order's totals", got[len(got)-1], "totals 220.00 100.00 120.00 EUR")
}
