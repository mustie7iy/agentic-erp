package revenue_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/revenue"
)

const orderChanged = "com.example.erp.sales.sales_order_changed.v1"

// consumer is the contract consumer as the server wires it, logging to
// log.
func consumer(t *testing.T, log *pgtest.SyncBuffer) *revenue.Consumer {
	t.Helper()
	return &revenue.Consumer{Endpoints: endpoints(t), Pool: pgtest.Pool(t), AppRole: "erp_app", Log: slog.New(slog.NewTextHandler(log, nil))}
}

// busEvent is the order's latest SalesOrderChanged as the relay publishes
// it: the outbox row's id, actor and payload under the tenant's subject.
func (e env) busEvent(t *testing.T, order string) outbox.Message {
	t.Helper()
	var id, actor uuid.UUID
	var payload []byte
	pgtest.Scan(t, `SELECT id, actor_id, payload FROM sales.outbox WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3
		ORDER BY id DESC LIMIT 1`, []any{e.Tenant.ID, orderChanged, order}, &id, &actor, &payload)
	return outbox.Message{ID: id, Subject: "erp.test.sales." + e.Tenant.ID.String() + ".sales_order_changed", Data: payload,
		Header: map[string]string{"ce-specversion": "1.0", "ce-id": id.String(), "ce-source": "/erp/test/sales", "ce-type": orderChanged,
			"ce-subject": order, "ce-time": "2026-11-20T08:00:00Z", "ce-datacontenttype": "application/json",
			"ce-tenantid": e.Tenant.ID.String(), "ce-actorid": actor.String(), "Content-Type": "application/json"}}
}

// again is the event published a second time under another id: what a
// later change of the order that leaves it confirmed would publish.
func again(m outbox.Message) outbox.Message {
	id := uuid.Must(uuid.NewV7())
	h := map[string]string{}
	for k, v := range m.Header {
		h[k] = v
	}
	h["ce-id"] = id.String()
	return outbox.Message{ID: id, Subject: m.Subject, Data: m.Data, Header: h}
}

// inbox counts the events the consumer recorded as read for the tenant.
func (e env) inbox(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM revenue.inbox WHERE tenant_id = $1 AND consumer = $2`, revenue.ContractConsumer)
}

// TestContractOfConfirmedOrder: a confirmed sales order's event gives the
// order its revenue contract, the system's, the same contract a person's
// request creates; the event read twice, or published again, leaves one
// contract and one RevenueContractChanged; a person's request for the
// order is then refused (T81a acceptance, product owner 2026-11-12).
func TestContractOfConfirmedOrder(t *testing.T) {
	e := setup(t)
	order, number := e.confirmed(t, e.goods(), e.support())
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	m := e.busEvent(t, order)
	for i := range 2 {
		if err := c.Handle(context.Background(), m); err != nil {
			t.Fatalf("read %d: got %v, want nil", i+1, err)
		}
	}
	if n := e.contractsOf(t, order); n != 1 {
		t.Fatalf("contracts of the order after its event was read twice: got %d, want 1", n)
	}
	got := e.contractOf(t, order)
	id, _ := got["id"].(string)
	fields(t, got, map[string]any{"status": "draft", "version": float64(1), "sales_order_number": number, "currency": "EUR",
		"customer_number": "BP70000001", "created_by": revenue.BuilderActor.String()})
	if a := amount(t, got, "transaction_price"); a != "2700.00 EUR" {
		t.Errorf("transaction price: got %s, want 2700.00 EUR, the order's net value", a)
	}
	obs := obligations(t, got)
	if len(obs) != 2 {
		t.Fatalf("obligations: got %d, want 2, one per order item", len(obs))
	}
	for i, want := range []string{"1500.00 EUR", "1200.00 EUR"} {
		if a := amount(t, obs[i], "standalone_selling_price"); a != want {
			t.Errorf("obligation %d's stand-alone selling price: got %s, want the item's net value %s", i+1, a, want)
		}
		if obs[i]["timing"] != "point_in_time" {
			t.Errorf("obligation %d's timing: got %v, want point_in_time", i+1, obs[i]["timing"])
		}
	}
	if n := e.audits(t, id, "sales_order_confirmed"); n != 1 {
		t.Errorf("audit records of the creation by the order's confirmation: got %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND actor_id = $3 AND actor_kind = 'system'`,
		id, revenue.BuilderActor); n != 1 {
		t.Errorf("audit records by the system's principal: got %d, want 1", n)
	}
	if n := len(e.events(t, order)); n != 1 {
		t.Errorf("RevenueContractChanged events: got %d, want 1", n)
	}
	if n := e.inbox(t); n != 1 {
		t.Errorf("events recorded in the inbox: got %d, want 1", n)
	}
	t.Run("the event published again", func(t *testing.T) {
		if err := c.Handle(context.Background(), again(m)); err != nil {
			t.Fatalf("got %v, want nil", err)
		}
		if n := e.contractsOf(t, order); n != 1 {
			t.Errorf("contracts of the order: got %d, want 1", n)
		}
		if n := len(e.events(t, order)); n != 1 {
			t.Errorf("RevenueContractChanged events: got %d, want 1", n)
		}
	})
	t.Run("a person's request for the order", func(t *testing.T) {
		e.In(t).Problem(409, "revenue.contract_exists", "POST", contracts, forOrder(order), "")
	})
	t.Run("as a person's request creates it", func(t *testing.T) {
		other, _ := e.confirmed(t, e.goods(), e.support())
		asked, _ := e.In(t).Must(201, "POST", contracts, forOrder(other), "")
		same := func(c map[string]any) string {
			return mustJSON(t, []any{c["status"], c["currency"], c["transaction_price"], c["obligations"], c["customer_id"], c["legal_entity_id"]})
		}
		if a, b := same(got), same(asked); a != b {
			t.Errorf("the automatic contract: got %s, want what a request creates %s", a, b)
		}
		if err := c.Handle(context.Background(), e.busEvent(t, other)); err != nil {
			t.Fatalf("the order's event after the request: got %v, want nil", err)
		}
		if n := e.contractsOf(t, other); n != 1 {
			t.Errorf("contracts of an order asked for before its event was read: got %d, want 1", n)
		}
		if n := len(e.events(t, other)); n != 1 {
			t.Errorf("RevenueContractChanged events of that order: got %d, want 1", n)
		}
	})
}

// TestContractRequestRacesTheEvent: of a person's request and the order's
// event at once, one creates the contract and the other finds it: the
// order has exactly one, published once (product owner 2026-11-12).
func TestContractRequestRacesTheEvent(t *testing.T) {
	e := setup(t)
	var log pgtest.SyncBuffer
	c := consumer(t, &log)
	for i := range 6 {
		order, _ := e.confirmed(t, e.goods())
		m := e.busEvent(t, order)
		var status int
		var handled error
		var wg sync.WaitGroup
		wg.Go(func() {
			status = e.API.Do(apitest.Req{Method: "POST", Path: contracts, Body: forOrder(order), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
		wg.Go(func() { handled = c.Handle(context.Background(), m) })
		wg.Wait()
		if handled != nil {
			t.Fatalf("order %d: the event: got %v, want nil", i+1, handled)
		}
		if status != 201 && status != 409 {
			t.Errorf("order %d: the request: got %d, want 201 (it created the contract) or 409 (the event did)", i+1, status)
		}
		if n := e.contractsOf(t, order); n != 1 {
			t.Errorf("order %d: contracts: got %d, want 1", i+1, n)
		}
		if n := len(e.events(t, order)); n != 1 {
			t.Errorf("order %d: RevenueContractChanged events: got %d, want 1", i+1, n)
		}
	}
}

// TestConsumerSkipsAndRefuses: another event and an order's change to
// another status than confirmed are skipped; a malformed event, or one
// whose subject names another tenant, is refused and logged; an order no
// contract is made for is logged and its event recorded as read. All are
// acknowledged and create nothing.
func TestConsumerSkipsAndRefuses(t *testing.T) {
	e := setup(t)
	order, _ := e.confirmed(t, e.goods())
	foreign := e.foreign(t)
	draft := e.draft(t, "EUR", e.goods())["id"].(string)
	stranger := uuid.New()
	for _, tc := range []struct {
		name   string
		order  string
		change func(*outbox.Message)
		logged string // what the log says; empty: nothing
		read   int    // events recorded in the inbox
	}{
		{"another event", order, func(m *outbox.Message) { m.Header["ce-type"] = "com.example.erp.sales.sales_order_delivered.v1" }, "", 0},
		// A returns order's event (T85a): it gets no revenue contract, whatever its status and the order it names.
		{"a confirmed returns order", order, func(m *outbox.Message) {
			m.Header["ce-type"] = "com.example.erp.sales.returns_order_changed.v1"
			m.Subject = strings.Replace(m.Subject, ".sales_order_changed", ".returns_order_changed", 1)
			m.Data = []byte(`{"returns_order_id": "` + uuid.NewString() + `", "sales_order_id": "` + order + `", "status": "confirmed", "status_before": "draft"}`)
		}, "", 0},
		{"an order created, not confirmed", draft, func(*outbox.Message) {}, "", 0},
		{"subject of another form", order, func(m *outbox.Message) { m.Subject = "erp.test." + e.Tenant.ID.String() }, "refused", 0},
		{"another context's stream", order, func(m *outbox.Message) { m.Subject = strings.Replace(m.Subject, ".sales.", ".payables.", 1) }, "refused", 0},
		{"another tenant", order, func(m *outbox.Message) {
			m.Subject = strings.Replace(m.Subject, e.Tenant.ID.String(), stranger.String(), 1)
		}, "refused", 0},
		{"id not a uuid", order, func(m *outbox.Message) { m.Header["ce-id"] = "e-1" }, "refused", 0},
		{"data not json", order, func(m *outbox.Message) { m.Data = []byte("{") }, "refused", 0},
		{"an order in a foreign currency", foreign, func(*outbox.Message) {}, "revenue.foreign_currency", 1},
		{"an order that is not there", order, func(m *outbox.Message) {
			m.Data = []byte(`{"sales_order_id": "` + uuid.NewString() + `", "status": "confirmed"}`)
		}, "no revenue contract for a confirmed sales order", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log pgtest.SyncBuffer
			before := e.inbox(t)
			m := e.busEvent(t, tc.order)
			tc.change(&m)
			if err := consumer(t, &log).Handle(context.Background(), m); err != nil {
				t.Fatalf("got %v, want nil (acknowledged)", err)
			}
			if n := e.count(t, `SELECT count(*) FROM revenue.contract WHERE tenant_id = $1`); n != 0 {
				t.Errorf("contracts: got %d, want none", n)
			}
			if tc.logged == "" && log.String() != "" {
				t.Errorf("logged %q, want nothing", log.String())
			}
			if !strings.Contains(log.String(), tc.logged) {
				t.Errorf("logged %q, want it to say %q", log.String(), tc.logged)
			}
			if n := e.inbox(t) - before; n != tc.read {
				t.Errorf("events recorded in the inbox: got %d, want %d", n, tc.read)
			}
		})
	}
}

// TestConsumerDatabaseFailures: the begin, every statement and the commit
// failing is an error and leaves nothing, so the event is read again; read
// again without a failure, it creates the contract.
func TestConsumerDatabaseFailures(t *testing.T) {
	e := setup(t)
	order, _ := e.confirmed(t, e.goods(), e.support())
	m := e.busEvent(t, order)
	pool := pgtest.Pool(t)
	failing := func(fail func(pgx.Tx) (pgx.Tx, error)) *revenue.Consumer {
		return &revenue.Consumer{Endpoints: endpoints(t), Pool: pool, AppRole: "erp_app", Log: slog.New(slog.DiscardHandler),
			Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				if err != nil {
					return nil, err
				}
				used, ferr := fail(tx)
				if ferr != nil {
					_ = tx.Rollback(ctx) // the injected begin failure ends the real transaction too
				}
				return used, ferr
			}}
	}
	nothing := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("got %v, want the injected failure", err)
		}
		if n := e.contractsOf(t, order); n != 0 {
			t.Errorf("contracts of the order: got %d, want none", n)
		}
		if n := e.inbox(t); n != 0 {
			t.Errorf("events recorded in the inbox: got %d, want none", n)
		}
	}
	t.Run("begin", func(t *testing.T) {
		nothing(t, failing(func(pgx.Tx) (pgx.Tx, error) { return nil, pgtest.ErrInjected }).Handle(context.Background(), m))
	})
	t.Run("commit", func(t *testing.T) {
		nothing(t, failing(func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil }).Handle(context.Background(), m))
	})
	t.Run("each statement", func(t *testing.T) {
		statements := 0
		for n := 1; n < 100; n++ {
			var last *pgtest.FailingTx
			err := failing(func(tx pgx.Tx) (pgx.Tx, error) {
				last = &pgtest.FailingTx{Tx: tx, FailAt: n}
				return last, nil
			}).Handle(context.Background(), m)
			if err == nil {
				// Statement n was never reached: the reading ran through. Undo it for the checks below.
				pgtest.Exec(t, `DELETE FROM revenue.performance_obligation WHERE tenant_id = $1`, e.Tenant.ID)
				pgtest.Exec(t, `DELETE FROM revenue.contract WHERE tenant_id = $1`, e.Tenant.ID)
				pgtest.Exec(t, `DELETE FROM revenue.inbox WHERE tenant_id = $1`, e.Tenant.ID)
				break
			}
			statements = n
			nothing(t, err)
		}
		// The actor, the inbox, the order, the company, the contract, its obligations, its read, the customer, the audit record and the event.
		if statements < 10 {
			t.Errorf("statements failed in turn: got %d, want at least 10", statements)
		}
	})
	t.Run("read again without a failure", func(t *testing.T) {
		var log pgtest.SyncBuffer
		if err := consumer(t, &log).Handle(context.Background(), m); err != nil {
			t.Fatalf("got %v, want nil", err)
		}
		if n := e.contractsOf(t, order); n != 1 {
			t.Errorf("contracts of the order: got %d, want 1", n)
		}
	})
}

// TestConsumedContexts: the consumer reads the sales context's stream.
func TestConsumedContexts(t *testing.T) {
	if got := revenue.ConsumedContexts(); len(got) != 1 || got[0] != "sales" {
		t.Errorf("contexts: got %v, want sales alone", got)
	}
}
