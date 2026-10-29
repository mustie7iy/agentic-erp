package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// threeOrders is tiny with three more orders of 10 units, 4500000002 to
// 4500000004, none received yet.
func threeOrders() *sap.Extract {
	x := tiny()
	for _, n := range []string{"4500000002", "4500000003", "4500000004"} {
		x.EKKO = append(x.EKKO, sap.EKKO{EBELN: n, BUKRS: "2000", LIFNR: "0000000001", WAERS: "EUR", BEDAT: "20260301"})
		x.EKPO = append(x.EKPO, sap.EKPO{EBELN: n, EBELP: "00010", MATNR: "MAT-1", MENGE: "10", NETPR: "5.00", PEINH: "1", SAKTO: "0000520000"})
	}
	return x
}

// TestLoadAndGoodsReceiptDoNotDeadlock runs a delta load whose receipts
// are on orders an earlier run loaded against a goods receipt posted
// through the API on one of them (T72b2's pre-existing finding, review:
// batch19). The goods receipt locks its order items and then the
// goods_receipt range; the load must take its order item locks in that
// order too, not lock an order's items while it holds the range its first
// receipt took. The interleaving is forced without sleeps: a third
// transaction holds the items of the second receipt's order until the load
// waits for them, the goods receipt starts and either finishes or waits
// for the load, and then lets go.
func TestLoadAndGoodsReceiptDoNotDeadlock(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.receipt.post")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	e := env{Env: f, dir: dir}
	x := threeOrders()
	write(t, dir, "delta.json", x)
	c := e.connector(t, "delta.json")
	if _, got := e.act(t, e.run(t, c, "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	for i, n := range []string{"4500000002", "4500000003", "4500000004"} {
		x.MSEG = append(x.MSEG, sap.MSEG{MBLNR: fmt.Sprintf("50000000%d", 11+i), MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: n, EBELP: "00010",
			MENGE: "1", BUDAT: "20260306"})
	}
	write(t, dir, "delta.json", x)
	gated, received := e.loadedOrder(t, "4500000003"), e.loadedOrder(t, "4500000004")

	run := e.run(t, c, "documents", false, e.DE.String())
	_, tag := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", "")
	gate, letGo := e.holdItems(t, gated)
	defer letGo()
	load := e.async(apitest.Req{Method: "POST", Path: "/v1/migration-runs/" + run["id"].(string) + ":start", IfMatch: tag})
	var loadPID int32
	pgtest.Eventually(t, 10*time.Second, func() bool { loadPID = blockedBy(t, gate); return loadPID != 0 }, "the load never waited for the items of order 4500000003")
	receipt := e.async(apitest.Req{Method: "POST", Path: "/v1/goods-receipts", Body: fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-07",
		"posting_date": "2026-03-07", "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "2", "unit": "EA"}}]}`, e.DE, received)})
	var posted *httptest.ResponseRecorder
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case posted = <-receipt:
			return true
		default:
			return blockedBy(t, loadPID) != 0
		}
	}, "the goods receipt neither finished nor waited")
	letGo()
	if posted == nil {
		posted = <-receipt
	}
	if r := <-load; r.Code != 202 {
		t.Errorf("load: got %d, want 202: %s", r.Code, r.Body)
	}
	if posted.Code != 201 {
		t.Errorf("goods receipt: got %d, want 201: %s", posted.Code, posted.Body)
	}
	got, _ := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", "")
	if n := got["report"].(map[string]any)["loaded"].(map[string]any)["goods_receipts"]; got["status"] != "loaded" || n != float64(3) {
		t.Errorf("delta run: got %v with %v goods receipts, want loaded with 3: %v", got["status"], n, got["report"])
	}
}

// loadedOrder is the platform order a run made of an SAP order.
func (e env) loadedOrder(t *testing.T, ebeln string) string {
	t.Helper()
	var id string
	pgtest.Scan(t, `SELECT entity_id::text FROM integration.external_ref WHERE tenant_id = $1 AND object = 'EKKO' AND external_key = $2`,
		[]any{e.Tenant.ID, ebeln}, &id)
	return id
}

// async performs the call as the requester in a goroutine, answering on
// the channel.
func (e env) async(r apitest.Req) <-chan *httptest.ResponseRecorder {
	r.Tenant, r.As, r.Key = e.Tenant.ID, e.Tenant.Human, apitest.NewKey()
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- e.API.Do(r) }()
	return out
}

// errSetup is what holdItems' transaction reports when its setup failed:
// apitest.Committed then fails the test and ends the goroutine, which
// still answers, so the test fails here rather than waiting for it.
var errSetup = errors.New("transaction setup failed")

// holdItems locks the order's item 1 in a transaction of its own, as an
// invoice's posting does (Orders.LockItems), and holds it until letGo,
// returning that transaction's backend.
func (e env) holdItems(t *testing.T, order string) (int32, func()) {
	t.Helper()
	var pid atomic.Int32
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		err := errSetup
		defer func() { done <- err }()
		err = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			var n int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&n); err != nil {
				return err
			}
			pid.Store(n)
			if err := (procurement.Orders{}).LockItems(ctx, uuid.MustParse(order), []int32{1}); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("hold the items of order %s: %v", order, err)
	}
	return pid.Load(), sync.OnceFunc(func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("the transaction holding the items of order %s: %v", order, err)
		}
	})
}

// blockedBy is a backend waiting for a lock pid holds, or 0 if none does.
func blockedBy(t *testing.T, pid int32) int32 {
	t.Helper()
	var waiting int32
	pgtest.Scan(t, `SELECT coalesce(min(pid), 0) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &waiting)
	return waiting
}
