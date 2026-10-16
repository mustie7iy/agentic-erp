package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
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
		var listed money.Minor
		for _, it := range list(t, "&open_only=false")["items"].([]any) {
			amount, err := money.ParseAmount(it.(map[string]any)["balance"].(map[string]any)["amount"].(string), 2)
			if err != nil {
				t.Fatal(err)
			}
			listed += amount
		}
		var account money.Minor
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT sum(amount_tc)::bigint FROM ledger.journal_line WHERE tenant_id = $1 AND account_id = $2`,
			g.ten.ID, g.grir).Scan(&account); err != nil {
			t.Fatal(err)
		}
		if listed != account {
			t.Errorf("listed balances: got %d, want the GR/IR account's %d", listed, account)
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
	t.Run("key date without an order", func(t *testing.T) {
		c := g.Client
		c.T = t
		p := c.Problem(400, "request.invalid_parameter", "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String()+"&posting_date_to=2026-03-09", "", "")
		if got := fmt.Sprint(p["errors"]); !strings.Contains(got, "purchase_order_id") {
			t.Errorf("field errors: got %s, want one on purchase_order_id", got)
		}
	})
	g.checkProjected(t)
	g.Problem(400, "request.invalid_order_by", "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String()+"&order_by=balance", "", "")
}

// TestGRIRBalanceAmounts: an invoice's reversal takes its quantity off the
// invoiced; a reversal's adjustment line moves no quantity; a position of
// a USD order shows its local and group balances at the receipt's rate.
func TestGRIRBalanceAmounts(t *testing.T) {
	g := grirSetup(t)
	settleRate(t, g.env, "USD", "2026-03-01", "0.9")
	reversed, residue, usd := uuid.New(), uuid.New(), uuid.New()
	g.post(t, g.receiptPosting("2026-03-02", "EUR", reversed, "-5", price{500, 1}))
	invoice := g.post(t, g.invoicePosting("2026-03-03", "EUR", reversed, "5", price{500, 1}, 2500))
	g.post(t, g.invoicePosting("2026-03-02", "EUR", residue, "1", price{1, 3}, 1))
	second := g.post(t, g.invoicePosting("2026-03-03", "EUR", residue, "1", price{1, 3}, 1))
	g.post(t, g.receiptPosting("2026-03-04", "EUR", residue, "-1", price{1, 3}))
	for _, e := range []ledger.Entry{invoice, second} {
		if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) error {
			_, err := postings().Reverse(ctx, e.ID, nil, nil)
			return err
		}); err != nil {
			t.Fatalf("reverse: %v", err)
		}
	}
	g.post(t, g.receiptPosting("2026-03-05", "USD", usd, "-100", price{100, 1}))
	for _, c := range []struct {
		name  string
		order uuid.UUID
		want  string
	}{
		{"invoice reversed", reversed, "5 0 -25.00 -25.00 -25.00"},
		{"residue adjusted", residue, "1 1 0.00 0.00 0.00"},
		{"in USD", usd, "100 0 -100.00 -90.00 -90.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := g.Client
			client.T = t
			page, _ := client.Must(200, "GET", "/v1/gr-ir-balances?open_only=false&legal_entity_id="+g.de.String()+"&purchase_order_id="+c.order.String(), "", "")
			items := page["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("positions: got %d, want 1", len(items))
			}
			b := items[0].(map[string]any)
			amount := func(k string) any { return b[k].(map[string]any)["amount"] }
			got := fmt.Sprint(b["received_quantity"].(map[string]any)["value"], " ", b["invoiced_quantity"].(map[string]any)["value"], " ",
				amount("balance"), " ", amount("balance_lc"), " ", amount("balance_gc"))
			if got != c.want {
				t.Errorf("received, invoiced, balances: got %s, want %s", got, c.want)
			}
		})
	}
}

// TestGRIRBalancesBudget checks 01-requirements' list budget (p95 150 ms)
// on a legal entity's GR/IR history: 20,000 order items received and
// invoiced (60,000 lines, closed positions) and 50 open ones, listed by
// order and by last posting date. The history's positions are projected
// as migration 000054 projects them; a page reads only the open ones.
func TestGRIRBalancesBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	g := grirSetup(t)
	// Item i: a receipt of 2 (entries 3i-2 and 3i-1: 1 each) and an invoice of
	// 2 (entry 3i); items above 19,950 lack their invoice. One transaction:
	// an entry is balanced when it commits.
	keep := `NOT (n % 3 = 0 AND (n + 2) / 3 > 19950)`
	if err := pgx.BeginFunc(context.Background(), pgtest.Admin(t), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period,
				document_type, document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
			SELECT $1, 2026, md5('e' || n)::uuid, $2, l.id, 3, 'SA', 'H' || n, '2026-03-02', '2026-03-02', 'EUR', 'EUR', 'EUR', 1, 1,
				CASE WHEN n % 3 = 0 THEN 'payables' ELSE 'procurement' END, $3
			FROM generate_series(1, 60000) n, ledger.ledger l WHERE l.tenant_id = $1 AND l.is_leading AND `+keep, g.ten.ID, g.de, g.ten.Human.ID); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id,
				fiscal_period, posting_date, account_id, amount_tc, amount_lc, amount_gc, quantity, uom_code, purchase_order_id, po_item_no)
			SELECT $1, 2026, md5('e' || n)::uuid, x.no, $2, l.id, 3, '2026-03-02', CASE x.no WHEN 1 THEN $3::uuid ELSE $4::uuid END,
				x.sign * a.amt, x.sign * a.amt, x.sign * a.amt, CASE x.no WHEN 1 THEN a.amt / 500 END, CASE x.no WHEN 1 THEN 'EA' END,
				CASE x.no WHEN 1 THEN md5('o' || ((n + 2) / 3))::uuid END, CASE x.no WHEN 1 THEN 1 END
			FROM generate_series(1, 60000) n
			CROSS JOIN LATERAL (SELECT CASE WHEN n % 3 = 0 THEN 1000 ELSE -500 END AS amt) a
			CROSS JOIN (VALUES (1, 1), (2, -1)) AS x(no, sign), ledger.ledger l
			WHERE l.tenant_id = $1 AND l.is_leading AND `+keep, g.ten.ID, g.de, g.grir, g.expense)
		if err != nil {
			return err
		}
		_, err = tx.Exec(context.Background(), `INSERT INTO ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id,
				currency_tc, currency_lc, currency_gc, uom_code, received, invoiced, amount_tc, amount_lc, amount_gc, last_posting_date)
			SELECT l.tenant_id, l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id, 'EUR', 'EUR', 'EUR', min(l.uom_code),
				coalesce(-sum(l.quantity) FILTER (WHERE e.source_context = 'procurement'), 0), coalesce(sum(l.quantity) FILTER (WHERE e.source_context = 'payables'), 0),
				sum(l.amount_tc), sum(l.amount_lc), sum(l.amount_gc), max(l.posting_date)
			FROM ledger.journal_line l JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
			WHERE l.tenant_id = $1 AND l.purchase_order_id IS NOT NULL GROUP BY 1, 2, 3, 4, 5`, g.ten.ID)
		return err
	}); err != nil {
		t.Fatalf("insert the history: %v", err)
	}
	pgtest.Exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, ledger.gr_ir_position`)
	for _, order := range []string{"purchase_order_id", "last_posting_date%20desc"} {
		var times []time.Duration
		for range 10 {
			start := time.Now()
			page, _ := g.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+g.de.String()+"&order_by="+order, "", "")
			times = append(times, time.Since(start))
			if n := len(page["items"].([]any)); n != 50 {
				t.Fatalf("open positions: got %d, want 50", n)
			}
		}
		slices.Sort(times)
		p95 := times[len(times)*95/100]
		t.Logf("GR/IR balances by %s over 20,000 positions: median %v, p95 %v", order, times[len(times)/2], p95)
		if budget := nfr.Budget(150 * time.Millisecond); p95 > budget {
			t.Errorf("GR/IR balances by %s: p95 %v exceeds the %v budget (150 ms scaled by ERP_BUDGET_SCALE)", order, p95, budget)
		}
	}
}
