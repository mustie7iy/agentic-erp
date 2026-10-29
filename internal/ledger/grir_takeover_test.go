package ledger_test

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// takeOverPosting takes over a source system's position of item 10 of the
// order: the open quantity and the balance, as a migration run does.
func (g grirEnv) takeOverPosting(order uuid.UUID, qty string, balance money.Minor) ledger.Posting {
	return ledger.Posting{LegalEntityID: g.de, DocumentType: "SA", DocumentDate: date("2026-03-05"), PostingDate: date("2026-03-05"), Currency: "EUR",
		SourceContext: "integration", SourceDocumentID: uuid.New(),
		GRIR: []ledger.GRIRItem{{PurchaseOrderID: order, POItemNo: 10, Quantity: qty, Unit: "EA", TakesOver: &balance}}}
}

// projected reads an order item's projected position: received, invoiced,
// migrated, balance and whether it is open.
func (g grirEnv) projected(t *testing.T, order uuid.UUID) string {
	t.Helper()
	var received, invoiced, migrated string
	var tc int64
	var open bool
	pgtest.Scan(t, `SELECT trim_scale(received)::text, trim_scale(invoiced)::text, trim_scale(migrated)::text, amount_tc, is_open
		FROM ledger.gr_ir_position WHERE tenant_id = $1 AND purchase_order_id = $2 AND po_item_no = 10`, []any{g.ten.ID, order},
		&received, &invoiced, &migrated, &tc, &open)
	return fmt.Sprintf("received %s, invoiced %s, migrated %s, balance %d, open %t", received, invoiced, migrated, tc, open)
}

// TestGRIRTakeOver: a migration run takes a source system's open GR/IR
// position over onto an order item without one (T72f): the item's GR/IR
// line with SAP's balance and open quantity, against a line on the GR/IR
// account without an order item, the quantity counted as migrated. An
// invoice then clears it at the order price, without a price difference,
// and the listing and the reconciliation's report show it.
func TestGRIRTakeOver(t *testing.T) {
	g := grirSetup(t)
	order, p := uuid.New(), price{15000, 1}
	entry := g.post(t, g.takeOverPosting(order, "-8", -120000))
	lines := on(entry, g.grir)
	if len(lines) != 2 || lines[0] != [3]money.Minor{-120000, -120000, -120000} || lines[1] != [3]money.Minor{120000, 120000, 120000} {
		t.Fatalf("the take-over's GR/IR lines: got %v, want the position and its offset", lines)
	}
	if entry.Lines[0].PurchaseOrderID == nil || entry.Lines[1].PurchaseOrderID != nil {
		t.Errorf("order items: got %v and %v, want the first line's only", entry.Lines[0].PurchaseOrderID, entry.Lines[1].PurchaseOrderID)
	}
	if got, want := g.projected(t, order), "received 0, invoiced 0, migrated -8, balance -120000, open true"; got != want {
		t.Errorf("after the take-over: got %s, want %s", got, want)
	}
	page, _ := g.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String()+"&purchase_order_id="+order.String()+"&posting_date_to=2026-03-31", "", "")
	if got := fmt.Sprint(page["items"].([]any)[0].(map[string]any)["migrated_quantity"]); got != "map[unit:EA value:-8]" {
		t.Errorf("migrated quantity on a key date: got %s, want -8 EA", got)
	}
	var open map[uuid.UUID]map[int32]ledger.OpenGRIR
	if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) (err error) {
		open, err = ledger.Reports{}.OpenGRIR(ctx, g.de, []uuid.UUID{order})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := open[order][10]; got != (ledger.OpenGRIR{Quantity: "-8", LC: -120000}) {
		t.Errorf("open GR/IR for the reconciliation: got %+v, want -8 and -120000", got)
	}
	invoice := g.post(t, g.invoicePosting("2026-03-20", "EUR", order, "8", p, 120000))
	if got := one(t, invoice, g.grir, "the invoice's GR/IR line"); got != [3]money.Minor{120000, 120000, 120000} {
		t.Errorf("the invoice's GR/IR line: got %v, want 1200.00 clearing the position", got)
	}
	if n := len(on(invoice, g.diff)) + len(on(invoice, g.expense)); n != 0 {
		t.Errorf("price difference and expense lines of the invoice: got %d, want none", n)
	}
	if got, want := g.projected(t, order), "received 0, invoiced 8, migrated -8, balance 0, open false"; got != want {
		t.Errorf("after the invoice: got %s, want %s", got, want)
	}
}

// TestGRIRTopUp: a later migration run tops a position up to the source
// system's, from what the earlier take-overs took over, which
// TakenOverGRIR reads; what the platform posted on the item stays beside
// it (review:batch21).
func TestGRIRTopUp(t *testing.T) {
	g := grirSetup(t)
	order := uuid.New()
	g.post(t, g.takeOverPosting(order, "-2", -500))
	taken := func(t *testing.T) map[uuid.UUID]ledger.GRIRTakenOverOrder {
		t.Helper()
		var out map[uuid.UUID]ledger.GRIRTakenOverOrder
		if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) (err error) {
			out, err = ledger.Postings{}.TakenOverGRIR(ctx, g.de, []uuid.UUID{order})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	read := taken(t)[order].Items[10]
	if read != (ledger.GRIRTakenOver{Quantity: "-2", Balance: -500}) {
		t.Fatalf("taken over: got %+v, want -2 and -500", read)
	}
	g.post(t, g.receiptPosting("2026-03-06", "EUR", order, "-1", price{250, 1}))
	topUp := g.takeOverPosting(order, "-5", -1250)
	topUp.GRIR[0].TakenOver = &read
	entry := g.post(t, topUp)
	if lines := on(entry, g.grir); len(lines) != 2 || lines[0] != [3]money.Minor{-750, -750, -750} || lines[1] != [3]money.Minor{750, 750, 750} {
		t.Errorf("the top-up's GR/IR lines: got %v, want what the position moved and its offset", lines)
	}
	if got := *entry.Lines[0].Quantity; money.ParseRat(got).Cmp(money.ParseRat("-3")) != 0 {
		t.Errorf("the top-up's quantity: got %s, want -3", got)
	}
	if got, want := g.projected(t, order), "received 1, invoiced 0, migrated -5, balance -1500, open true"; got != want {
		t.Errorf("after the top-up: got %s, want %s", got, want)
	}
	after := taken(t)[order]
	if got := after.Items[10]; got != (ledger.GRIRTakenOver{Quantity: "-5", Balance: -1250}) {
		t.Errorf("taken over after the top-up: got %+v, want -5 and -1250", got)
	}
	if got := len(after.Sources); got != 2 || !slices.Contains(after.Sources, topUp.SourceDocumentID) {
		t.Errorf("the take-overs' source documents: got %v, want the first take-over's and %s", after.Sources, topUp.SourceDocumentID)
	}
	// The source system's position invoiced in full: topped up to nothing.
	cleared := g.takeOverPosting(order, "0", 0)
	cleared.GRIR[0].TakenOver = &ledger.GRIRTakenOver{Quantity: "-5", Balance: -1250}
	g.post(t, cleared)
	if got, want := g.projected(t, order), "received 1, invoiced 0, migrated 0, balance -250, open true"; got != want {
		t.Errorf("topped up to nothing: got %s, want %s", got, want)
	}
	if got := taken(t)[order].Items[10]; got != (ledger.GRIRTakenOver{Quantity: "0", Balance: 0}) {
		t.Errorf("taken over at the end: got %+v, want nothing", got)
	}
}

// TestGRIRTakeOverRefusals: a position is taken over once, and topped up
// only from what was taken over, by an integration entry of take-over
// items only.
func TestGRIRTakeOverRefusals(t *testing.T) {
	g := grirSetup(t)
	post := func(t *testing.T, in ledger.Posting) error {
		t.Helper()
		return apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, in)
			return err
		})
	}
	taken := uuid.New()
	g.post(t, g.takeOverPosting(taken, "-2", -500))
	stale := g.takeOverPosting(taken, "-3", -700)
	stale.GRIR[0].TakenOver = &ledger.GRIRTakenOver{Quantity: "-2", Balance: -400}
	staleQty := g.takeOverPosting(taken, "-3", -700)
	staleQty.GRIR[0].TakenOver = &ledger.GRIRTakenOver{Quantity: "-1", Balance: -500}
	unread := g.takeOverPosting(taken, "-3", -700)
	unread.GRIR[0].TakenOver = &ledger.GRIRTakenOver{Quantity: "two", Balance: -500}
	twice := g.takeOverPosting(uuid.New(), "-1", -100)
	twice.GRIR = append(twice.GRIR, twice.GRIR[0])
	mixed := g.takeOverPosting(uuid.New(), "-1", -100)
	mixed.GRIR = append(mixed.GRIR, g.receiptPosting("2026-03-02", "EUR", uuid.New(), "-1", price{500, 1}).GRIR...)
	notMigrated := g.takeOverPosting(uuid.New(), "-1", -100)
	notMigrated.SourceContext = "procurement"
	bad := g.takeOverPosting(uuid.New(), "-1.1234567", -100)
	settleRate(t, g.env, "USD", "2026-03-05", "1.5")
	huge := g.takeOverPosting(uuid.New(), "-1", -math.MaxInt64)
	huge.Currency = "USD"
	for _, c := range []struct {
		name   string
		in     ledger.Posting
		status int
		code   string
	}{
		{"taken over before", g.takeOverPosting(taken, "-2", -500), 409, "ledger.gr_ir_position_exists"},
		{"a balance taken over other than read", stale, 409, "ledger.gr_ir_position_exists"},
		{"a quantity taken over other than read", staleQty, 409, "ledger.gr_ir_position_exists"},
		{"a quantity taken over that does not parse", unread, 500, ""},
		{"the same item twice", twice, 409, "ledger.gr_ir_position_exists"},
		{"mixed with a receipt", mixed, 500, ""},
		{"not a migration run's", notMigrated, 500, ""},
		{"a quantity of seven decimals", bad, 500, ""},
		{"a balance out of range once converted", huge, 422, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := problem.From(post(t, c.in))
			if p.Status != c.status || c.code != "" && p.Code != c.code {
				t.Errorf("got %d %s, want %d %s", p.Status, p.Code, c.status, c.code)
			}
		})
	}
}
