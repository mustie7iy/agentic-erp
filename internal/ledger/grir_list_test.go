package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestGRIRBalances lists order items' GR/IR positions: received and
// invoiced quantities and the balance in the three currencies, open ones
// by default, on a key date, for one order, in pages; a position is zero
// once its quantities agree, the listed balances add up to the GR/IR
// account's, and a principal without company access sees none.
func TestGRIRBalances(t *testing.T) {
	g := grirSetup(t)
	open, closed, p := uuid.New(), uuid.New(), price{500, 1}
	g.post(t, g.receiptPosting("2026-03-02", "EUR", open, "-6", p))
	g.post(t, g.invoicePosting("2026-03-10", "EUR", open, "4", p, 2080))
	g.post(t, g.receiptPosting("2026-03-03", "EUR", closed, "-2", p))
	g.post(t, g.invoicePosting("2026-03-04", "EUR", closed, "2", p, 1000))
	list := func(t *testing.T, query string) map[string]any {
		t.Helper()
		c := g.Client
		c.T = t
		page, _ := c.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String()+query, "", "")
		return page
	}
	only := func(t *testing.T, page map[string]any) map[string]any {
		t.Helper()
		items := page["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items: got %d, want 1: %v", len(items), items)
		}
		return items[0].(map[string]any)
	}
	t.Run("open positions", func(t *testing.T) {
		b := only(t, list(t, ""))
		got := fmt.Sprintf("%v %v %v %v %v %v %v %v", b["purchase_order_id"], b["po_item_no"], b["account_id"], b["received_quantity"], b["invoiced_quantity"],
			b["balance"], b["balance_lc"], b["last_posting_date"])
		want := fmt.Sprintf("%s 10 %s map[unit:EA value:6] map[unit:EA value:4] map[amount:-10.00 currency:EUR] map[amount:-10.00 currency:EUR] 2026-03-10", open, g.grir)
		if got != want {
			t.Errorf("position: got %s, want %s", got, want)
		}
	})
	t.Run("zero positions too", func(t *testing.T) {
		if n := len(list(t, "&open_only=false")["items"].([]any)); n != 2 {
			t.Errorf("items: got %d, want both positions", n)
		}
	})
	t.Run("on a key date", func(t *testing.T) {
		b := only(t, list(t, "&posting_date_to=2026-03-09&purchase_order_id="+open.String()))
		if b["balance"].(map[string]any)["amount"] != "-30.00" {
			t.Errorf("balance on 9 March: got %v, want -30.00 (before the invoice)", b["balance"])
		}
	})
	t.Run("in pages", func(t *testing.T) {
		first := list(t, "&open_only=false&page_size=1&order_by=last_posting_date%20desc")
		if got := only(t, first)["purchase_order_id"]; got != open.String() {
			t.Errorf("first page: got %v, want the latest posted position", got)
		}
		second := list(t, "&open_only=false&page_size=1&order_by=last_posting_date%20desc&page_token="+first["next_page_token"].(string))
		if got := only(t, second)["purchase_order_id"]; got != closed.String() {
			t.Errorf("second page: got %v, want the other position", got)
		}
		if _, more := second["next_page_token"]; more {
			t.Error("second page: got a next page token, want none")
		}
	})
	t.Run("in pages by order", func(t *testing.T) {
		first := list(t, "&open_only=false&page_size=1")
		second := list(t, "&open_only=false&page_size=1&page_token="+first["next_page_token"].(string))
		ids := []any{only(t, first)["purchase_order_id"], only(t, second)["purchase_order_id"]}
		if want := []any{min(open.String(), closed.String()), max(open.String(), closed.String())}; fmt.Sprint(ids) != fmt.Sprint(want) {
			t.Errorf("orders by page: got %v, want %v", ids, want)
		}
	})
	t.Run("adds up to the account", func(t *testing.T) {
		var account int64
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT sum(amount_tc)::bigint FROM ledger.journal_line WHERE tenant_id = $1 AND account_id = $2`,
			g.ten.ID, g.grir).Scan(&account); err != nil {
			t.Fatal(err)
		}
		if account != -1000 {
			t.Errorf("GR/IR account balance: got %d, want -1000, the listed positions' sum", account)
		}
	})
	t.Run("without company access", func(t *testing.T) {
		agent := g.Client
		agent.T, agent.As = t, g.ten.Agent
		page, _ := agent.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String(), "", "")
		if n := len(page["items"].([]any)); n != 0 {
			t.Errorf("items: got %d, want none", n)
		}
	})
	g.Problem(400, "request.invalid_order_by", "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String()+"&order_by=balance", "", "")
}
