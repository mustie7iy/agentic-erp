package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestStockLoadAndGoodsReceiptDoNotDeadlock runs a delta load that takes
// stock over (S2p) and receives on an order an earlier run loaded, valued
// through GR/IR, against a goods receipt posted through the API on the
// same order (ADR-0082 As built S2p, Locks; sapstk's review round 1). The
// goods receipt locks its order item, then takes the goods_receipt range
// and then journal_we for its WE entry; the run must take its order item
// locks before the material_document and journal_we ranges its stock
// take-over draws from. The interleaving is forced without sleeps: a third
// transaction holds the goods_receipt range until the goods receipt waits
// for it holding the order item, the run starts and waits for the goods
// receipt, and then lets go; both finish, one after the other. A run that
// took the stock over before the order item locks would hold journal_we
// while it waits for the item, and deadlock with the goods receipt here.
func TestStockLoadAndGoodsReceiptDoNotDeadlock(t *testing.T) {
	e := stockGRIRSetup(t)
	first := halfReceived()
	first.EKPO[0].MENGE = "10" // room for the person's receipt beside SAP's
	write(t, e.dir, "first.json", first)
	if _, got := e.act(t, e.run(t, e.connector(t, "first.json"), "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	order, _ := e.orderOf(t, "4500000001")
	delta := receivedAgain(halfReceived())
	delta.EKPO[0].MENGE = "10"
	stock := withStock()
	delta.T001W, delta.T001L, delta.MARA, delta.MAKT, delta.MARM, delta.MARC, delta.MARD, delta.MBEW = stock.T001W, stock.T001L, stock.MARA, stock.MAKT,
		stock.MARM, stock.MARC, stock.MARD, stock.MBEW
	write(t, e.dir, "delta.json", delta)
	run := e.run(t, e.connector(t, "delta.json"), "documents", false, e.DE.String())
	_, tag := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", "")

	gate, letGo := holdRange(t, e.Tenant.ID, e.DE, "goods_receipt")
	defer letGo()
	receipt := e.async(apitest.Req{Method: "POST", Path: "/v1/goods-receipts", Body: fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-07",
		"posting_date": "2026-03-07", "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "1", "unit": "EA"}}]}`, e.DE, order)})
	var receiptPID int32
	pgtest.Eventually(t, 10*time.Second, func() bool { receiptPID = blockedBy(t, gate); return receiptPID != 0 },
		"the goods receipt never waited for the goods_receipt range")
	load := e.async(apitest.Req{Method: "POST", Path: "/v1/migration-runs/" + run["id"].(string) + ":start", IfMatch: tag})
	var loaded *httptest.ResponseRecorder
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case loaded = <-load:
			return true
		default:
			return blockedBy(t, receiptPID) != 0
		}
	}, "the run neither finished nor waited for the goods receipt")
	letGo()
	if loaded == nil {
		loaded = <-load
	}
	if r := <-receipt; r.Code != 201 {
		t.Errorf("goods receipt: got %d, want 201: %s", r.Code, r.Body)
	}
	if loaded.Code != 202 {
		t.Errorf("the run: got %d, want 202: %s", loaded.Code, loaded.Body)
	}
	got, _ := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", "")
	if got["status"] != "loaded" {
		t.Fatalf("delta run: got %v, want loaded: %v", got["status"], got["report"])
	}
	expectFields(t, "the delta run's load", report(got)["loaded"].(map[string]any), map[string]any{"goods_receipts": 1.0, "material_valuations": 3.0,
		"initial_entries": 3.0})
}

// stockGRIRSetup is stockSetup's company 2000 with valuation class 3000
// and fixture.GRIR's accounts (in the activated configuration, so neither
// leaves the other's inactive), its people also posting goods receipts.
func stockGRIRSetup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(c *config.Config) {
		fixture.GRIRAccounts(c)
		de := c.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "150000", Name: "Inventory", Type: config.Asset},
			config.Account{Number: "391000", Name: "Initial entries of stock", Type: config.Equity},
			config.Account{Number: "517000", Name: "Price differences", Type: config.Expense})
		de.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "150000", InitialEntry: "391000",
			PriceDifference: "517000"}}
	}, append(slices.Clone(perms), "materials.material.read", "materials.material.write", "tenancy.org.read", "tenancy.org.manage",
		"inventory.stock.read", "inventory.valuation.read", "procurement.receipt.post")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir, c.Now = dir, func() time.Time { return stockDay } })
	return env{Env: f, dir: dir}
}

// holdRange locks the company's number range of the document type in a
// transaction of its own, as drawing a number does, and holds it until
// letGo, returning that transaction's backend. The transaction changes
// nothing and is rolled back.
func holdRange(t *testing.T, tenant, company uuid.UUID, documentType string) (int32, func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM config.number_range WHERE tenant_id = $1 AND legal_entity_id = $2 AND document_type = $3
		AND fiscal_year = 2026 FOR UPDATE`, tenant, company, documentType).Scan(&pid); err != nil {
		t.Fatalf("lock the %s range: %v", documentType, err)
	}
	done := false
	return pid, func() {
		if done {
			return
		}
		done = true
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("let go of the %s range: %v", documentType, err)
		}
	}
}
