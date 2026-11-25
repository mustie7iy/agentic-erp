package revenue_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/revenue"
)

// changeOrder changes the order at its current version with the body of
// a sales order change.
func (e env) changeOrder(t *testing.T, order, body string) {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	e.Must(200, "POST", "/v1/sales-orders/"+order+":change", body, tag)
}

// versionOf is the order's current version as an If-Match value.
func (e env) versionOf(t *testing.T, order string) string {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	return tag
}

// cancelOrder cancels the order at its current version.
func (e env) cancelOrder(t *testing.T, order string) {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	e.Must(200, "POST", "/v1/sales-orders/"+order+":cancel", `{"reason": "Withdrawn by the customer"}`, tag)
}

// itemChange is the JSON of a sales order change of one item: its new
// quantity in EA and its new unit price in EUR, either empty for none.
func itemChange(no int, quantity, price string) string {
	ch := fmt.Sprintf(`{"item_no": %d`, no)
	if quantity != "" {
		ch += fmt.Sprintf(`, "quantity": {"value": %q, "unit": "EA"}`, quantity)
	}
	if price != "" {
		ch += fmt.Sprintf(`, "unit_price": {"amount": %q, "currency": "EUR"}`, price)
	}
	return fmt.Sprintf(`{"items": [%s}]}`, ch)
}

// handled reads the order's latest SalesOrderChanged with the consumer.
func (e env) handled(t *testing.T, c *revenue.Consumer, order string) {
	t.Helper()
	if err := c.Handle(context.Background(), e.busEvent(t, order)); err != nil {
		t.Fatalf("the order's event: got %v, want nil", err)
	}
}

// netAmounts are the obligations' order values.
func netAmounts(t *testing.T, c map[string]any) []string {
	t.Helper()
	var out []string
	for _, o := range obligations(t, c) {
		out = append(out, amount(t, o, "net_amount"))
	}
	return out
}

// follows counts the contract's kept follows of the cause.
func (e env) follows(t *testing.T, id, cause string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM revenue.contract_change WHERE tenant_id = $1 AND contract_id = $2 AND cause = $3`, id, cause)
}

// TestContractFollowsOrderChange: a draft contract follows a change of
// its confirmed order's price: the obligation's order value and the
// transaction price are the order's, the stand-alone selling prices stay
// as they were; the follow is kept with its values before and after,
// audited and published. Reading the event again, an older event, or one
// of a change that leaves the order's values as they were changes
// nothing (T81e acceptance).
func TestContractFollowsOrderChange(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support())
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	confirmation := e.busEvent(t, order)
	e.changeOrder(t, order, itemChange(1, "", "13.00"))
	e.handled(t, c, order)
	got := e.contractOf(t, order)
	fields(t, got, map[string]any{"status": "draft", "version": float64(2), "changes": float64(1), "updated_by": revenue.BuilderActor.String()})
	if a := amount(t, got, "transaction_price"); a != "2760.00 EUR" {
		t.Errorf("transaction price: got %s, want 2760.00 EUR, the changed order's net value", a)
	}
	if n := netAmounts(t, got); !slices.Equal(n, []string{"1560.00 EUR", "1200.00 EUR"}) {
		t.Errorf("order values: got %v, want 1560.00 EUR and 1200.00 EUR", n)
	}
	if p := amount(t, obligations(t, got)[0], "standalone_selling_price"); p != "1500.00 EUR" {
		t.Errorf("the changed item's stand-alone selling price: got %s, want 1500.00 EUR, as it was", p)
	}
	if a := allocated(t, got); !slices.Equal(a, []string{"-", "-"}) {
		t.Errorf("a draft's allocated amounts: got %v, want none", a)
	}
	if _, ok := got["ended_at"]; ok {
		t.Errorf("ended_at: got %v, want none", got["ended_at"])
	}
	kept := `[{"obligation_no": 1, "net_amount_before": 150000, "net_amount_after": 156000, "allocated_amount_before": null, "allocated_amount_after": null,
		"ended": false, "added": false}]`
	if n := e.count(t, `SELECT count(*) FROM revenue.contract_change WHERE tenant_id = $1 AND contract_id = $2 AND change_no = 1 AND cause = 'sales_order_changed'
		AND sales_order_changes = 1 AND transaction_price_before = 270000 AND transaction_price_after = 276000 AND obligations = $3::jsonb`, id, kept); n != 1 {
		t.Errorf("the kept follow: got %d with its prices 270000 > 276000 and its obligations %s, want 1", n, kept)
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'revenue.contract.follow' AND object_id = $2
		AND detail->>'source' = 'sales_order_changed' AND detail->>'change_no' = '1' AND actor_kind = 'system'`, id); n != 1 {
		t.Errorf("audit records of the follow: got %d, want 1", n)
	}
	events := e.events(t, order)
	if len(events) != 2 {
		t.Fatalf("RevenueContractChanged events: got %d, want 2 (created, followed)", len(events))
	}
	fields(t, events[1], map[string]any{"status": "draft", "status_before": "draft", "changes": float64(1), "ended": false, "version": float64(2)})
	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"the event read again", func(t *testing.T) { t.Helper(); e.handled(t, c, order) }},
		{"the confirmation's event read after it", func(t *testing.T) {
			t.Helper()
			if err := c.Handle(context.Background(), again(confirmation)); err != nil {
				t.Fatalf("got %v, want nil", err)
			}
		}},
		{"a change of the requested delivery date alone", func(t *testing.T) {
			t.Helper()
			e.changeOrder(t, order, `{"requested_delivery_date": "2027-01-15"}`)
			e.handled(t, c, order)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t)
			if a, b := mustJSON(t, e.contractOf(t, order)), mustJSON(t, got); a != b {
				t.Errorf("the contract: got %s, want it as it was %s", a, b)
			}
			if n := len(e.events(t, order)); n != 2 {
				t.Errorf("RevenueContractChanged events: got %d, want 2", n)
			}
		})
	}
}

// TestActiveContractReallocated: an active contract follows a change of
// its order's quantity: the new transaction price is allocated by the
// stand-alone selling prices a person set, which stay, with the timing
// and service period; the shares add up to it exactly and its schedule
// reads them (T81e acceptance).
func TestActiveContractReallocated(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support())
	e.Must(200, "PATCH", contracts+"/"+id, set(overTime(2, "2026-01-01", "2026-12-31", "1300.00")), `"1"`)
	e.activated(t, id)
	var log pgtest.SyncBuffer
	e.changeOrder(t, order, itemChange(1, "100", ""))
	e.handled(t, consumer(t, &log), order)
	got := e.contractOf(t, order)
	fields(t, got, map[string]any{"status": "active", "version": float64(4), "changes": float64(1)})
	if a := amount(t, got, "transaction_price"); a != "2450.00 EUR" {
		t.Errorf("transaction price: got %s, want 2450.00 EUR", a)
	}
	// 2450.00 × 1500 ÷ 2800 is 1312.50 and × 1300 ÷ 2800 is 1137.50, exactly.
	if a := allocated(t, got); !slices.Equal(a, []string{"1312.50 EUR", "1137.50 EUR"}) {
		t.Errorf("allocated amounts: got %v, want 1312.50 EUR and 1137.50 EUR", a)
	}
	o := obligations(t, got)[1]
	fields(t, o, map[string]any{"timing": "over_time", "service_from": "2026-01-01", "service_to": "2026-12-31"})
	if p := amount(t, o, "standalone_selling_price"); p != "1300.00 EUR" {
		t.Errorf("the support's stand-alone selling price: got %s, want 1300.00 EUR, as Nora set it", p)
	}
	s := e.mustSchedule(t, id, "2026-03-31")
	if s.TransactionPrice.Amount != "2450.00" {
		t.Errorf("the schedule's transaction price: got %s, want 2450.00", s.TransactionPrice.Amount)
	}
	if n := e.count(t, `SELECT count(*) FROM revenue.contract k WHERE k.tenant_id = $1 AND k.id = $2 AND k.transaction_price =
		(SELECT sum(o.allocated_amount) FROM revenue.performance_obligation o WHERE o.tenant_id = k.tenant_id AND o.contract_id = k.id)`, id); n != 1 {
		t.Error("the allocated amounts do not add up to the transaction price in minor units")
	}
}

// TestRunAfterFollow: what a recognition run posted stands after a
// follow; the next run posts the change in its own period, and once
// billed what was recognized is what was billed (T81e acceptance: no
// period already run changes).
func TestRunAfterFollow(t *testing.T) {
	e := setup(t)
	_, order := e.active(t, e.goods())
	e.delivered(t, order, 1, "60", "2026-01-15")
	january := e.ran(t, 1)
	var log pgtest.SyncBuffer
	e.changeOrder(t, order, itemChange(1, "", "15.00"))
	e.handled(t, consumer(t, &log), order)
	february := e.ran(t, 2)
	if got, want := moved(t, postings(t, february)[0]), "1 400000: 900.00 EUR - 0.00 EUR, 150.00 EUR"; got != want {
		t.Errorf("February's posting: got %s, want %s, the change's difference", got, want)
	}
	read, _ := e.Must(200, "GET", runs+"/"+january["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, january); a != b {
		t.Errorf("January's run: got %s, want it as it was %s", a, b)
	}
	e.billed(t, order, "2026-03-10")
	e.ran(t, 3)
	if got := e.balance(t, "125000"); got != 0 {
		t.Errorf("contract assets once billed: got %d, want 0", got)
	}
	if got := e.balance(t, "400000"); got != -90000 {
		t.Errorf("revenue: got %d, want -90000, the 60 delivered at the changed price", got)
	}
}

// TestRunBeforeTheEvent: a recognition run, and a schedule read, after an
// order's change and before the consumer reads its event take the
// contract as the follow makes it: the run follows it in its own
// transaction, and the event read afterwards adds nothing (T81e, review
// round 1).
func TestRunBeforeTheEvent(t *testing.T) {
	e := setup(t)
	id, order := e.active(t, e.goods())
	e.delivered(t, order, 1, "100", "2026-01-15")
	e.changeOrder(t, order, itemChange(1, "150", ""))
	s := e.mustSchedule(t, id, "2026-01-31")
	if got := s.RecognizedAmount.Amount; got != "1250.00" {
		t.Errorf("the schedule before the follow: got %s recognized, want 1250.00, 100 of 150 at the order's 1875.00 (not 1000.00 of the old 1500.00)", got)
	}
	january := e.ran(t, 1)
	if got, want := moved(t, postings(t, january)[0]), "1 400000: 1250.00 EUR - 0.00 EUR, 1250.00 EUR"; got != want {
		t.Errorf("January's posting: got %s, want %s", got, want)
	}
	if n := e.follows(t, id, "sales_order_changed"); n != 1 {
		t.Errorf("follows kept by the run: got %d, want 1", n)
	}
	events := len(e.events(t, order))
	var log pgtest.SyncBuffer
	e.handled(t, consumer(t, &log), order)
	if n := len(e.events(t, order)); n != events {
		t.Errorf("RevenueContractChanged after the event: got %d, want %d, nothing left to follow", n, events)
	}
}

// TestCancelledOrderEndsContract: an order's cancellation ends its
// contract: every obligation ends with nothing allocated, the transaction
// price is zero, and the next run takes off what earlier runs recognized
// over time; an ended contract is not changed or activated, and its
// events read again change nothing; a cancelled order without a contract
// gets none (T81e acceptance).
func TestCancelledOrderEndsContract(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.serviceItem())
	e.Must(200, "PATCH", contracts+"/"+id, set(overTime(2, "2026-01-01", "2026-12-31", "1200.00")), `"1"`)
	e.activated(t, id)
	e.ran(t, 1)
	if got := e.balance(t, "125000"); got <= 0 {
		t.Fatalf("contract assets after January: got %d, want the support recognized over January", got)
	}
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	e.cancelOrder(t, order)
	e.handled(t, c, order)
	got := e.contractOf(t, order)
	fields(t, got, map[string]any{"status": "active", "changes": float64(1)})
	if _, ok := got["ended_at"]; !ok {
		t.Error("ended_at: got none, want when it ended")
	}
	if a := amount(t, got, "transaction_price"); a != "0.00 EUR" {
		t.Errorf("transaction price: got %s, want 0.00 EUR", a)
	}
	if a := allocated(t, got); !slices.Equal(a, []string{"0.00 EUR", "0.00 EUR"}) {
		t.Errorf("allocated amounts: got %v, want nothing", a)
	}
	for i, o := range obligations(t, got) {
		if _, ok := o["ended_at"]; !ok {
			t.Errorf("obligation %d's ended_at: got none, want when it ended", i+1)
		}
	}
	if n := e.follows(t, id, "sales_order_cancelled"); n != 1 {
		t.Errorf("follows of the cancellation: got %d, want 1", n)
	}
	if events := e.events(t, order); events[len(events)-1]["ended"] != true {
		t.Errorf("the last RevenueContractChanged: got ended %v, want true", events[len(events)-1]["ended"])
	}
	ended := e.mustSchedule(t, id, "2026-03-31")
	if got := ended.TransactionPrice.Amount; got != "0.00" {
		t.Errorf("the ended contract's scheduled price: got %s, want 0.00", got)
	}
	if got := ended.RecognizedAmount.Amount; got != "0.00" {
		t.Errorf("the ended contract's recognized amount: got %s, want 0.00", got)
	}
	e.ran(t, 2)
	if got := e.balance(t, "125000"); got != 0 {
		t.Errorf("contract assets after February: got %d, want 0, the support taken off", got)
	}
	if got := e.balance(t, "410000"); got != 0 {
		t.Errorf("service revenue: got %d, want 0", got)
	}
	_, tag := e.Must(200, "GET", contracts+"/"+id, "", "")
	e.Problem(409, "revenue.contract_ended", "PATCH", contracts+"/"+id, set(overTime(2, "2026-01-01", "2026-06-30", "1200.00")), tag)
	e.Problem(409, "revenue.contract_ended", "POST", contracts+"/"+id+":activate", "", tag)
	if err := c.Handle(context.Background(), again(e.busEvent(t, order))); err != nil {
		t.Fatalf("the cancellation published again: got %v, want nil", err)
	}
	if a, b := mustJSON(t, e.contractOf(t, order)), mustJSON(t, got); a != b {
		t.Errorf("the ended contract after its event published again: got %s, want it as it was %s", a, b)
	}
	t.Run("a draft's", func(t *testing.T) {
		draft, other := e.contract(t, e.goods())
		e.cancelOrder(t, other)
		e.handled(t, c, other)
		got := e.contractOf(t, other)
		fields(t, got, map[string]any{"status": "draft", "changes": float64(1)})
		if a := allocated(t, got); !slices.Equal(a, []string{"0.00 EUR"}) {
			t.Errorf("allocated amounts: got %v, want nothing for the ended obligation", a)
		}
		_, tag := e.In(t).Must(200, "GET", contracts+"/"+draft, "", "")
		e.In(t).Problem(409, "revenue.contract_ended", "POST", contracts+"/"+draft+":activate", "", tag)
	})
	t.Run("an order cancelled without a contract", func(t *testing.T) {
		other := e.draft(t, "EUR", e.goods())["id"].(string)
		e.cancelOrder(t, other)
		var log pgtest.SyncBuffer
		e.handled(t, consumer(t, &log), other)
		if n := e.contractsOf(t, other); n != 0 {
			t.Errorf("contracts of the order: got %d, want none", n)
		}
		if log.String() != "" {
			t.Errorf("logged %q, want nothing", log.String())
		}
	})
}

// TestRunReadsEndedContractsUntilTakenOff: an ended contract is read by
// the run after its end, which takes its accruals off, and by no run
// after that; one that ended with nothing posted is never read. So the
// contracts a run reads do not grow with the contracts that ended: with
// a bound of one, the runs go on however many ended (review:batch79).
func TestRunReadsEndedContractsUntilTakenOff(t *testing.T) {
	e := setup(t)
	accrued, order := e.contract(t, e.goods(), e.serviceItem())
	e.Must(200, "PATCH", contracts+"/"+accrued, set(overTime(2, "2026-01-01", "2026-12-31", "1200.00")), `"1"`)
	e.activated(t, accrued)
	_, live := e.active(t, e.goods())
	e.delivered(t, live, 1, "10", "2026-01-15")
	fields(t, e.ran(t, 1), map[string]any{"contract_count": float64(2), "posting_count": float64(2)})
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	for range 3 {
		_, never := e.active(t, e.goods())
		e.cancelOrder(t, never)
		e.handled(t, c, never)
	}
	e.cancelOrder(t, order)
	e.handled(t, c, order)
	february := e.ran(t, 2)
	fields(t, february, map[string]any{"contract_count": float64(2), "posting_count": float64(1)})
	if got := e.balance(t, "125000"); got != 12500 {
		t.Errorf("contract assets after February: got %d, want 12500, the live order's delivery alone", got)
	}
	h := endpoints(t)
	h.MaxContracts = 1
	var march gen.CreateRevenueRecognitionRunRes
	err := e.inRequest(t, func(ctx context.Context) (err error) {
		march, err = h.CreateRevenueRecognitionRun(ctx, &gen.RevenueRecognitionRunCreate{LegalEntityID: e.DE, FiscalYear: 2026, FiscalPeriod: 3},
			gen.CreateRevenueRecognitionRunParams{})
		return err
	})
	if err != nil {
		t.Fatalf("March's run with a bound of one contract: got %v, want it run", err)
	}
	if run, ok := march.(*gen.RevenueRecognitionRunHeaders); !ok || run.Response.ContractCount != 1 {
		t.Errorf("March's run: got %+v, want it read 1 contract, the live order's", march)
	}
}

// TestFollowRacingChange: a follow and a change of its order at once wait
// for one another on the order's row; read after both, the change's event
// leaves the contract at the order's values (T81e, ADR-0008).
func TestFollowRacingChange(t *testing.T) {
	e := setup(t)
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	for i := range 4 {
		_, order := e.contract(t, e.goods())
		e.changeOrder(t, order, itemChange(1, "110", ""))
		m := e.busEvent(t, order)
		_, tag := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
		var handled error
		var code int
		var wg sync.WaitGroup
		wg.Go(func() { handled = c.Handle(context.Background(), m) })
		wg.Go(func() {
			code = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/sales-orders/" + order + ":change", Body: itemChange(1, "100", ""), IfMatch: tag,
				Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
		wg.Wait()
		if handled != nil || code != 200 {
			t.Fatalf("order %d: got the follow %v and the change %d, want both", i+1, handled, code)
		}
		e.handled(t, c, order)
		var same bool
		pgtest.Scan(t, `SELECT k.transaction_price = o.net_amount FROM revenue.contract k
			JOIN sales.sales_order o ON o.tenant_id = k.tenant_id AND o.id = k.sales_order_id WHERE k.tenant_id = $1 AND k.sales_order_id = $2`,
			[]any{e.Tenant.ID, order}, &same)
		if !same {
			t.Errorf("order %d: the contract's transaction price: got another than the order's net value, want the order's", i+1)
		}
	}
}

// TestFollowDatabaseFailures: every statement of a follow and its commit
// failing is an error and leaves the contract as it was, so the event is
// read again; read again without a failure, the contract follows.
func TestFollowDatabaseFailures(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods())
	// A quantity changed and an item added: the follow updates one obligation and inserts another.
	e.changeOrder(t, order, fmt.Sprintf(`{"items": [{"item_no": 1, "quantity": {"value": "100", "unit": "EA"}}], "new_items": [%s]}`,
		e.item("Installation", "", "1", "300.00", "EUR")))
	m := e.busEvent(t, order)
	pool := pgtest.Pool(t)
	failing := func(fail func(pgx.Tx) pgx.Tx) *revenue.Consumer {
		return &revenue.Consumer{Endpoints: endpoints(t), Pool: pool, AppRole: "erp_app", Log: slog.New(slog.DiscardHandler),
			Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				if err != nil {
					return nil, err
				}
				return fail(tx), nil
			}}
	}
	unchanged := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("got %v, want the injected failure", err)
		}
		if n := e.count(t, `SELECT count(*) FROM revenue.contract WHERE tenant_id = $1 AND id = $2 AND changes = 0`, id); n != 1 {
			t.Error("the contract: got it changed, want it as it was")
		}
	}
	t.Run("commit", func(t *testing.T) {
		unchanged(t, failing(func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailCommit: true} }).Handle(context.Background(), m))
	})
	statements := 0
	for n := 1; n < 100; n++ {
		err := failing(func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: n} }).Handle(context.Background(), m)
		if err == nil {
			break
		}
		statements = n
		unchanged(t, err)
	}
	// The actor, the inbox, the order's lock and read, the contract's lock and read, the follow kept, the obligations, the contract,
	// its read, the company, the customer, the audit record and the event.
	if statements < 12 {
		t.Errorf("statements failed in turn: got %d, want at least 12", statements)
	}
	if n := e.follows(t, id, "sales_order_changed"); n != 1 {
		t.Errorf("follows after the event read without a failure: got %d, want 1", n)
	}
}

// TestContractFollowsItemsAddedRemoved: an item added to the order gets
// an obligation, numbered after the highest, at a point in time with its
// net value as its stand-alone selling price; a removed item's obligation
// ends with nothing allocated; an active contract allocates its price
// again over the obligations that stand; an ended obligation is not
// changed, and a draft activated later gives it nothing (T78e, T81e).
func TestContractFollowsItemsAddedRemoved(t *testing.T) {
	e := setup(t)
	id, order := e.active(t, e.goods(), e.support())
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	e.changeOrder(t, order, fmt.Sprintf(`{"removed_items": [2], "new_items": [%s]}`, e.item("Installation", "INSTALL", "1", "300.00", "EUR")))
	e.handled(t, c, order)
	got := e.contractOf(t, order)
	fields(t, got, map[string]any{"status": "active", "changes": float64(1)})
	if a := amount(t, got, "transaction_price"); a != "1800.00 EUR" {
		t.Errorf("transaction price: got %s, want 1800.00 EUR, the brackets and the installation", a)
	}
	obs := obligations(t, got)
	if len(obs) != 3 {
		t.Fatalf("obligations: got %d, want 3", len(obs))
	}
	fields(t, obs[2], map[string]any{"obligation_no": float64(3), "order_item_no": float64(3), "item_key": "INSTALL", "timing": "point_in_time"})
	if p := amount(t, obs[2], "standalone_selling_price"); p != "300.00 EUR" {
		t.Errorf("the new obligation's stand-alone selling price: got %s, want its net value 300.00 EUR", p)
	}
	if _, ok := obs[1]["ended_at"]; !ok {
		t.Error("the removed item's obligation: got it standing, want it ended")
	}
	if a := allocated(t, got); !slices.Equal(a, []string{"1500.00 EUR", "0.00 EUR", "300.00 EUR"}) {
		t.Errorf("allocated amounts: got %v, want 1500.00, 0.00 and 300.00 EUR", a)
	}
	var added bool
	pgtest.Scan(t, `SELECT obligations @> '[{"obligation_no": 3, "added": true}, {"obligation_no": 2, "ended": true}]' FROM revenue.contract_change
		WHERE tenant_id = $1 AND contract_id = $2`, []any{e.Tenant.ID, id}, &added)
	if !added {
		t.Error("the kept follow: got no added obligation 3 and ended obligation 2, want both")
	}
	t.Run("an item added to a draft's order", func(t *testing.T) {
		_, other := e.contract(t, e.goods())
		e.changeOrder(t, other, fmt.Sprintf(`{"new_items": [%s]}`, e.item("Installation", "", "1", "300.00", "EUR")))
		e.handled(t, c, other)
		got := e.contractOf(t, other)
		if n := netAmounts(t, got); !slices.Equal(n, []string{"1500.00 EUR", "300.00 EUR"}) {
			t.Errorf("order values: got %v, want 1500.00 EUR and 300.00 EUR", n)
		}
	})
	t.Run("an item removed before the confirmation", func(t *testing.T) {
		draft := e.draft(t, "EUR", e.goods(), e.support())["id"].(string)
		e.changeOrder(t, draft, `{"removed_items": [2]}`)
		e.Must(200, "POST", "/v1/sales-orders/"+draft+":confirm", "", e.versionOf(t, draft))
		e.handled(t, c, draft)
		got := e.contractOf(t, draft)
		if n := netAmounts(t, got); !slices.Equal(n, []string{"1500.00 EUR"}) {
			t.Errorf("obligations: got %v, want the brackets' alone, the removed item not ordered", n)
		}
		if a := amount(t, got, "transaction_price"); a != "1500.00 EUR" {
			t.Errorf("transaction price: got %s, want 1500.00 EUR", a)
		}
	})
	t.Run("a draft's ended obligation", func(t *testing.T) {
		draft, other := e.contract(t, e.goods(), e.support())
		e.changeOrder(t, other, `{"removed_items": [2]}`)
		e.handled(t, c, other)
		_, tag := e.In(t).Must(200, "GET", contracts+"/"+draft, "", "")
		e.In(t).Problem(409, "revenue.obligation_ended", "PATCH", contracts+"/"+draft, set(overTime(2, "2026-01-01", "2026-12-31", "1300.00")), tag)
		activated, _ := e.In(t).Must(200, "POST", contracts+"/"+draft+":activate", "", tag)
		if a := allocated(t, activated); !slices.Equal(a, []string{"1500.00 EUR", "0.00 EUR"}) {
			t.Errorf("allocated at activation: got %v, want 1500.00 EUR and nothing for the ended obligation", a)
		}
	})
}
