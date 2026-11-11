package procurement_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// GR/IR clearing of closed orders (T72e, SAP MR11): a closed order's
// residual GR/IR positions are proposed for clearing, approved by a
// controller who is not the proposer, and posted against the price
// difference account; the GR/IR balances then show them cleared.

// closedGRIROrder is a closed EUR order of 10 EA at 2.50, valued through
// GR/IR, with the receipts given (item:quantity:unit) posted before it
// was closed, and their ids.
func (e env) closedGRIROrder(t *testing.T, receipts ...string) (string, []string) {
	t.Helper()
	order := e.grirOrder(t, "EUR")
	var ids []string
	for _, r := range receipts {
		code, gr := e.receipt(t, order, r)
		if code != 201 {
			t.Fatalf("receipt %s: got %d %v, want 201", r, code, gr)
		}
		ids = append(ids, id(gr))
	}
	e.act(t, "/v1/purchase-orders/"+order, "close", "")
	return order, ids
}

func clearingsOf(order string) string { return "/v1/purchase-orders/" + order + "/gr-ir-clearings" }

const clearingBody = `{"reason": "Delivered short; the supplier will not invoice", "posting_date": "2026-03-20"}`

// position is the order item's GR/IR balance as listed: received,
// invoiced and cleared quantities and the balance, "" when it is not
// listed.
func (e env) position(t *testing.T, order, query string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/gr-ir-balances?legal_entity_id=%s&purchase_order_id=%s%s", e.DE, order, query), "", "")
	items := page["items"].([]any)
	if len(items) == 0 {
		return ""
	}
	b := items[0].(map[string]any)
	q := func(f string) any { return b[f].(map[string]any)["value"] }
	return fmt.Sprint(q("received_quantity"), " ", q("invoiced_quantity"), " ", q("cleared_quantity"), " ", b["balance"].(map[string]any)["amount"])
}

func TestGRIRClearing(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	e.Controller(t)
	open := e.grirOrder(t, "EUR")
	if code, gr := e.receipt(t, open, "1:4:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	e.Problem(409, "procurement.invalid_status", "POST", clearingsOf(open), clearingBody, "")
	order, _ := e.closedGRIROrder(t, "1:4:EA")
	e.Problem(422, "procurement.invalid_item_no", "POST", clearingsOf(order),
		`{"reason": "r", "posting_date": "2026-03-20", "po_item_nos": [2]}`, "")
	c, tag := e.Must(201, "POST", clearingsOf(order), clearingBody, "")
	path := "/v1/gr-ir-clearings/" + id(c)
	t.Run("proposed", func(t *testing.T) {
		e := e.in(t)
		want(t, "clearing", c, map[string]any{"status": "pending", "purchase_order_id": order, "purchase_order_number": "PO100001",
			"posting_date": "2026-03-20", "legal_entity_id": e.DE.String()})
		wantItems := []any{map[string]any{"po_item_no": float64(1), "description": "Paper", "quantity": map[string]any{"value": "-4", "unit": "EA"},
			"amount": map[string]any{"amount": "-10.00", "currency": "EUR"}, "offset_account": c["offset_account"]}}
		if !reflect.DeepEqual(c["items"], wantItems) {
			t.Errorf("items: got %v, want %v", c["items"], wantItems)
		}
		want(t, "total", c["total"].(map[string]any), map[string]any{"amount": "-10.00", "currency": "EUR"})
		want(t, "offset account", c["offset_account"].(map[string]any), map[string]any{"id": e.Account(t, "2000", "590000").String(),
			"number": "590000", "name": "Price differences"})
		r := e.Pending(t, id(c))
		want(t, "request", r, map[string]any{"object_type": "gr_ir_clearing", "id": c["approval_request_id"], "requester_id": e.Tenant.Human.ID.String()})
		want(t, "request amount", r["amount"].(map[string]any), map[string]any{"amount": "10.00", "currency": "EUR"})
		if got := r["object_resource"]; got != path {
			t.Errorf("request's object: got %v, want %s", got, path)
		}
		page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/gr-ir-balances?legal_entity_id=%s&purchase_order_id=%s", e.DE, order), "", "")
		if got := page["items"].([]any)[0].(map[string]any)["purchase_order_number"]; got != "PO100001" {
			t.Errorf("position's order number: got %v, want PO100001", got)
		}
		if tag != `"1"` {
			t.Errorf("ETag: got %s, want \"1\"", tag)
		}
		if got, _ := e.Must(200, "GET", path, "", ""); !reflect.DeepEqual(got, c) {
			t.Errorf("read back: got %v, want %v", got, c)
		}
		e.Problem(409, "procurement.gr_ir_clearing_pending", "POST", clearingsOf(order), clearingBody, "")
		e.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "Mine"}`, fmt.Sprintf(`"%v"`, r["version"]))
		if got := e.position(t, order, ""); got != "4 0 0 -10.00" {
			t.Errorf("position before the approval: got %q, want 4 0 0 -10.00", got)
		}
	})
	e.Approve(t, id(c))
	posted, _ := e.Must(200, "GET", path, "", "")
	t.Run("posted", func(t *testing.T) {
		e := e.in(t)
		want(t, "posted clearing", posted, map[string]any{"status": "posted", "version": float64(2), "updated_by": e.Approver.ID.String()})
		entry := posted["journal_entry"].(map[string]any)
		docType, source, doc, lines := e.entryOf(t, entry["id"])
		if got := [3]any{docType, source, doc.String()}; got != [3]any{"WE", "procurement", id(c)} {
			t.Errorf("entry: got %v, want WE by procurement for the clearing", got)
		}
		o, four := uuid.MustParse(order), "4"
		wantLines := []entryLine{{e.Account(t, "2000", "219000"), 1000, 1000, &four, &o}, {e.Account(t, "2000", "590000"), -1000, -1000, nil, nil}}
		if !reflect.DeepEqual(lines, wantLines) {
			t.Errorf("entry lines: got %+v, want %+v", lines, wantLines)
		}
		if got := e.position(t, order, ""); got != "" {
			t.Errorf("open positions after the clearing: got %q, want none", got)
		}
		if got := e.position(t, order, "&open_only=false"); got != "4 0 4 0.00" {
			t.Errorf("position after the clearing: got %q, want 4 0 4 0.00", got)
		}
		if got := e.position(t, order, "&posting_date_to=2026-03-19&open_only=false"); got != "4 0 0 -10.00" {
			t.Errorf("position the day before: got %q, want 4 0 0 -10.00", got)
		}
		if got := e.position(t, order, "&posting_date_to=2026-03-20&open_only=false"); got != "4 0 4 0.00" {
			t.Errorf("position on the clearing's date: got %q, want 4 0 4 0.00", got)
		}
		cleared := events(t, e, "gr_ir_cleared")
		if len(cleared) != 1 {
			t.Fatalf("GrIrCleared events: got %d, want 1", len(cleared))
		}
		want(t, "GrIrCleared", cleared[0], map[string]any{"gr_ir_clearing_id": id(c), "purchase_order_id": order, "document_number": "PO100001",
			"journal_entry_id": entry["id"], "journal_entry_number": entry["document_number"], "posting_date": "2026-03-20"})
		var actions []string
		pgtest.Scan(t, `SELECT array_agg(action ORDER BY occurred_at) FROM audit.audit_event WHERE object_id = $1`, []any{id(c)}, &actions)
		if fmt.Sprint(actions) != "[procurement.gr_ir_clearing.create procurement.gr_ir_clearing.post]" {
			t.Errorf("audit: got %v, want create then post", actions)
		}
		// Nothing is left to clear, and the request is decided.
		e.Problem(409, "procurement.nothing_to_clear", "POST", clearingsOf(order), clearingBody, "")
		r, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+c["approval_request_id"].(string), "", "")
		e.As(e.Approver).Problem(409, "workflow.not_pending", "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "again"}`, fmt.Sprintf(`"%v"`, r["version"]))
		page, _ := e.Must(200, "GET", clearingsOf(order), "", "")
		if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != id(c) {
			t.Errorf("clearings of the order: got %v, want the posted one", items)
		}
	})
}

// TestGRIRClearingReplayed: the same request with its Idempotency-Key
// again answers what it answered and proposes nothing more.
func TestGRIRClearingReplayed(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	e.Controller(t)
	order, _ := e.closedGRIROrder(t, "1:4:EA")
	req := apitest.Req{Method: http.MethodPost, Path: clearingsOf(order), Body: clearingBody, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, again := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || again.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice", first.Code, again.Code)
	}
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", again.Header().Get("Idempotent-Replayed"))
	}
	if got, want := apitest.JSON(t, again), apitest.JSON(t, first); !reflect.DeepEqual(got, want) {
		t.Errorf("replayed body: got %v, want %v", got, want)
	}
	page, _ := e.Must(200, "GET", clearingsOf(order), "", "")
	if n := len(page["items"].([]any)); n != 1 {
		t.Errorf("clearings: got %d, want 1", n)
	}
}

// TestGRIRClearingOutdated: a position that moved after the proposal
// refuses the approval, and the request stays pending until it is
// rejected; a new proposal clears what is left. A withdrawn request
// cancels its clearing.
func TestGRIRClearingOutdated(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	e.Controller(t)
	order, receipts := e.closedGRIROrder(t, "1:2:EA", "1:2:EA")
	c, _ := e.Must(201, "POST", clearingsOf(order), clearingBody, "")
	// A mistaken receipt is reversed on a closed order too.
	e.act(t, "/v1/goods-receipts/"+receipts[1], "reverse", "")
	r := e.Pending(t, id(c))
	version := fmt.Sprintf(`"%v"`, r["version"])
	e.As(e.Approver).Problem(409, "ledger.gr_ir_clearing_outdated", "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "ok"}`, version)
	if got := e.Pending(t, id(c)); got["status"] != "pending" {
		t.Errorf("request after the refusal: got %v, want pending", got["status"])
	}
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+id(r)+":reject", `{"comment": "The position moved"}`, version)
	got, _ := e.Must(200, "GET", "/v1/gr-ir-clearings/"+id(c), "", "")
	want(t, "rejected clearing", got, map[string]any{"status": "rejected", "decision_comment": "The position moved"})
	if _, ok := got["journal_entry"]; ok {
		t.Errorf("rejected clearing's entry: got %v, want none", got["journal_entry"])
	}
	if got := e.position(t, order, ""); got != "2 0 0 -5.00" {
		t.Errorf("position after the rejection: got %q, want 2 0 0 -5.00", got)
	}
	again, _ := e.Must(201, "POST", clearingsOf(order), clearingBody, "")
	w := e.Pending(t, id(again))
	e.Must(200, "POST", "/v1/approval-requests/"+id(w)+":cancel", `{"comment": "Not yet"}`, fmt.Sprintf(`"%v"`, w["version"]))
	got, _ = e.Must(200, "GET", "/v1/gr-ir-clearings/"+id(again), "", "")
	want(t, "withdrawn clearing", got, map[string]any{"status": "cancelled", "decision_comment": "Not yet"})
	last, _ := e.Must(201, "POST", clearingsOf(order), clearingBody, "")
	e.Approve(t, id(last))
	// The order's clearings, newest first, a page at a time.
	var seen []any
	for page, token := 0, ""; page == 0 || token != ""; page++ {
		query := "?page_size=1"
		if token != "" {
			query += "&page_token=" + url.QueryEscape(token)
		}
		got, _ := e.Must(200, "GET", clearingsOf(order)+query, "", "")
		seen = append(seen, got["items"].([]any)[0].(map[string]any)["id"])
		token, _ = got["next_page_token"].(string)
	}
	if want := []any{id(last), id(again), id(c)}; !reflect.DeepEqual(seen, want) {
		t.Errorf("clearings by page: got %v, want %v", seen, want)
	}
	if got := e.position(t, order, "&open_only=false"); got != "2 0 2 0.00" {
		t.Errorf("position after the clearing: got %q, want 2 0 2 0.00", got)
	}
}

// TestGRIRClearingRefusals: an order valued directly, or valued through
// GR/IR without an open position, has nothing to clear; a legal entity
// without a price difference account cannot clear; the API is human-only.
func TestGRIRClearingRefusals(t *testing.T) {
	e := setup(t)
	direct := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	e.act(t, "/v1/purchase-orders/"+id(direct), "release", "")
	e.act(t, "/v1/purchase-orders/"+id(direct), "close", "")
	e.Problem(409, "procurement.nothing_to_clear", "POST", clearingsOf(id(direct)), clearingBody, "")
	e.GRIR(t)
	e.Controller(t)
	empty, _ := e.closedGRIROrder(t)
	e.Problem(409, "procurement.nothing_to_clear", "POST", clearingsOf(empty), clearingBody, "")
	order, _ := e.closedGRIROrder(t, "1:4:EA")
	agent := e.Client
	agent.As = e.Tenant.Agent
	agent.Problem(403, "identity.agent_not_allowed", "POST", clearingsOf(order), clearingBody, "")
	e.Problem(404, "request.not_found", "GET", "/v1/gr-ir-clearings/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "GET", clearingsOf(uuid.NewString()), "", "")
	e.Problem(422, "procurement.reason_required", "POST", clearingsOf(order), `{"reason": "  ", "posting_date": "2026-03-20"}`, "")
	// A policy that needs no approval would post without a decision.
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		h := endpoints()
		h.Approvals = noApproval{}
		_, err := h.CreateGrIrClearing(ctx, &gen.GrIrClearingCreate{Reason: "r", PostingDate: dateOf("2026-03-20")}, gen.CreateGrIrClearingParams{ID: uuid.MustParse(order)})
		return err
	}); err == nil || problem.From(err).Status != 500 {
		t.Errorf("no approval required: got %v, want an internal error", err)
	}
	// Balances whose total is out of the range of an amount.
	huge, _ := e.closedGRIROrder(t, "1:4:EA")
	for _, no := range []int{2, 3} {
		pgtest.Exec(t, `INSERT INTO ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id, currency_tc, currency_lc,
			currency_gc, uom_code, received, invoiced, amount_tc, amount_lc, amount_gc, last_posting_date)
			VALUES ($1, $2, $3, $4, $5, 'EUR', 'EUR', 'EUR', 'EA', 1, 0, 4611686018427387904, 0, 0, '2026-03-15')`,
			e.Tenant.ID, e.DE, huge, no, e.Account(t, "2000", "219000"))
	}
	e.Problem(422, "procurement.amount_too_large", "POST", clearingsOf(huge), clearingBody, "")
	pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE tenant_id = $1 AND legal_entity_id = $2 AND key = 'price_difference'`, e.Tenant.ID, e.DE)
	e.Problem(422, "ledger.no_price_difference_account", "POST", clearingsOf(order), clearingBody, "")
}

// TestGRIRClearingCostCenter: on a primary cost price difference account
// the clearing's line carries the order item's cost center, as an
// invoice's price difference does.
func TestGRIRClearingCostCenter(t *testing.T) {
	e := setupAssignment(t)
	e.Controller(t)
	o := e.order(t, items(centered(e.expense, "5.00", e.c.Admin, "")), "")
	e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	if code, gr := e.receipt(t, id(o), "1:2:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	e.act(t, "/v1/purchase-orders/"+id(o), "close", "")
	c, _ := e.Must(201, "POST", clearingsOf(id(o)), clearingBody, "")
	e.Approve(t, id(c))
	posted, _ := e.Must(200, "GET", "/v1/gr-ir-clearings/"+id(c), "", "")
	grir, diff := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String()
	wantLines := sorted([]string{grir + ":-:-", diff + ":" + e.c.Admin.String() + ":" + e.c.Profit.String()})
	if got := centersOn(t, posted["journal_entry"].(map[string]any)["id"]); fmt.Sprint(got) != fmt.Sprint(wantLines) {
		t.Errorf("clearing lines: got %v, want %v", got, wantLines)
	}
}

// withAssetClearing gives the prototype's document asset classes (company
// 2000 clears its machines on 169000) and company 2000 the GR/IR and
// price difference accounts.
func withAssetClearing(doc *config.Config) {
	fixture.AssetClasses(doc)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "219000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "219000", "590000"
}

// TestGRIRClearingOfAssetItem: a clearing clears the position of an order
// item that is an asset's, one on an asset clearing account of the
// company, against that account, not the price difference account: what
// was received and never invoiced comes off the asset's cost, not into
// profit or loss (product owner 2026-11-10). The clearing shows each
// item's account, keeps it only where it is not the clearing's own, and
// posts against it; an item on another account clears as before.
func TestGRIRClearingOfAssetItem(t *testing.T) {
	e := setupWith(t, withAssetClearing)
	e.Controller(t)
	clearing := e.Account(t, "2000", "169000")
	o := e.order(t, items(item(clearing.String(), "10", "EA", "2.50", "EUR", ""), item(e.office, "10", "EA", "2.50", "EUR", "")), "")
	order := id(o)
	if got := e.act(t, "/v1/purchase-orders/"+order, "release", ""); got["valuation"] != "gr_ir" {
		t.Fatalf("order valuation: got %v, want gr_ir", got["valuation"])
	}
	if code, gr := e.receipt(t, order, "1:4:EA", "2:4:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	e.act(t, "/v1/purchase-orders/"+order, "close", "")
	c, _ := e.Must(201, "POST", clearingsOf(order), clearingBody, "")
	diff := map[string]any{"id": e.Account(t, "2000", "590000").String(), "number": "590000", "name": "Price differences"}
	asset := map[string]any{"id": clearing.String(), "number": "169000", "name": "Asset clearing"}
	want(t, "the clearing's account", c["offset_account"].(map[string]any), diff)
	shown := c["items"].([]any)
	want(t, "the asset's item's account", shown[0].(map[string]any)["offset_account"].(map[string]any), asset)
	want(t, "the other item's account", shown[1].(map[string]any)["offset_account"].(map[string]any), diff)
	var kept string
	pgtest.Scan(t, `SELECT string_agg(po_item_no || ' ' || coalesce(offset_account_number || ' ' || offset_account_name || ' ' || offset_account_id, 'the clearing''s'),
			'; ' ORDER BY po_item_no) FROM procurement.gr_ir_clearing_item WHERE gr_ir_clearing_id = $1`, []any{id(c)}, &kept)
	if want := fmt.Sprintf("1 169000 Asset clearing %s; 2 the clearing's", clearing); kept != want {
		t.Errorf("accounts kept with the items: got %q, want %q", kept, want)
	}
	e.Approve(t, id(c))
	posted, _ := e.Must(200, "GET", "/v1/gr-ir-clearings/"+id(c), "", "")
	want(t, "posted clearing", posted, map[string]any{"status": "posted"})
	if !reflect.DeepEqual(posted["items"], c["items"]) {
		t.Errorf("items once posted: got %v, want %v as proposed", posted["items"], c["items"])
	}
	_, _, _, lines := e.entryOf(t, posted["journal_entry"].(map[string]any)["id"])
	po, four := uuid.MustParse(order), "4"
	grir := e.Account(t, "2000", "219000")
	wantLines := []entryLine{{grir, 1000, 1000, &four, &po}, {clearing, -1000, -1000, nil, nil},
		{grir, 1000, 1000, &four, &po}, {e.Account(t, "2000", "590000"), -1000, -1000, nil, nil}}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Errorf("entry lines: got %+v, want %+v", lines, wantLines)
	}
	// The receipt's 10.00 on the asset clearing account is taken off again.
	var balance int64
	pgtest.Scan(t, `SELECT sum(amount_lc)::bigint FROM ledger.journal_line WHERE account_id = $1`, []any{clearing}, &balance)
	if balance != 0 {
		t.Errorf("asset clearing account: got %d, want 0", balance)
	}
}

// TestGRIRClearingTotal: a clearing whose balances are out of range has no
// total, which the proposal refuses.
func TestGRIRClearingTotal(t *testing.T) {
	for name, amounts := range map[string][]money.Minor{"absolute value": {money.Minor(-1 << 63)}, "sum": {1 << 62, 1 << 62}, "gross": {1 << 62, -(1 << 62)}} {
		t.Run(name, func(t *testing.T) {
			var c procurement.GRIRClearing
			for _, a := range amounts {
				c.Items = append(c.Items, procurement.GRIRClearingItem{Amount: a})
			}
			if _, _, ok := c.Total(); ok {
				t.Errorf("total of %v: got ok, want out of range", amounts)
			}
		})
	}
}

// TestGRIRClearingFailures: every statement of a proposal, a read, a list
// and each decision reports its failure, and none runs outside a unit of
// work; a decision on a clearing that is not pending on the request fails.
func TestGRIRClearingFailures(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	e.Controller(t)
	h := endpoints()
	order, _ := e.closedGRIROrder(t, "1:4:EA")
	o := uuid.MustParse(order)
	c, _ := e.Must(201, "POST", clearingsOf(order), clearingBody, "")
	cid, request := uuid.MustParse(id(c)), uuid.MustParse(c["approval_request_id"].(string))
	decisions := procurement.GRIRClearings{Endpoints: h}
	for name, call := range map[string]func(ctx context.Context) error{
		"list": func(ctx context.Context) error {
			_, err := h.ListGrIrClearings(ctx, gen.ListGrIrClearingsParams{ID: o})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetGrIrClearing(ctx, gen.GetGrIrClearingParams{ID: cid})
			return err
		},
		"approve": func(ctx context.Context) error { return decisions.Approved(ctx, cid, request) },
		"reject":  func(ctx context.Context) error { return decisions.Rejected(ctx, cid, request, "no") },
		"cancel":  func(ctx context.Context) error { return decisions.Cancelled(ctx, cid, request, "") },
		"positions": func(ctx context.Context) error {
			_, err := ledger.Postings{}.GRIROrder(ctx, e.DE, o)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Approver, name, call)
		})
	}
	t.Run("propose", func(t *testing.T) {
		other, _ := e.closedGRIROrder(t, "1:4:EA")
		call := func(ctx context.Context) error {
			_, err := h.CreateGrIrClearing(ctx, &gen.GrIrClearingCreate{Reason: "r", PostingDate: dateOf("2026-03-20")},
				gen.CreateGrIrClearingParams{ID: uuid.MustParse(other)})
			return err
		}
		if err := call(context.Background()); err == nil {
			t.Error("outside a unit of work: got nil error, want an error")
		}
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "propose", call)
	})
	t.Run("not pending on the request", func(t *testing.T) {
		for name, call := range map[string]func(ctx context.Context) error{
			"approve": func(ctx context.Context) error { return decisions.Approved(ctx, cid, uuid.New()) },
			"reject":  func(ctx context.Context) error { return decisions.Rejected(ctx, cid, uuid.New(), "") },
			"unknown": func(ctx context.Context) error { return decisions.Approved(ctx, uuid.New(), request) },
		} {
			t.Run(name, func(t *testing.T) {
				if err := apitest.InRequest(t, e.Tenant, e.Approver, call); err == nil {
					t.Error("got nil error, want the decision refused")
				}
			})
		}
	})
	t.Run("page size", func(t *testing.T) {
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListGrIrClearings(ctx, gen.ListGrIrClearingsParams{ID: o, PageSize: gen.NewOptInt32(0)})
			return err
		}); err == nil {
			t.Error("got nil error, want page_size 0 refused")
		}
	})
}
