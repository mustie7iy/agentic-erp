package procurement_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Order changes (T47f): a released order is changed by an order change,
// drafted, checked and kept as a version of the order.

// releasedOrder is a released order of 10 EA at 2.50 (office) with 4
// received, and its path.
func (e env) releasedOrder(t *testing.T) string {
	t.Helper()
	o := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", "")), "")
	path := "/v1/purchase-orders/" + id(o)
	e.act(t, path, "release", "")
	if code, gr := e.receipt(t, id(o), "1:4:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	return path
}

func TestDraftOrderChanges(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	body := `{"reason": "More paper at a new price", "payment_term_code": "NT30", "items": ` + items(item(e.office, "12", "EA", "3.00", "EUR", "")) + `}`
	ch, tag := e.Must(201, "POST", path+"/changes", body, "")
	want(t, "change", ch, map[string]any{"status": "draft", "base_revision": float64(1), "revision": nil, "reason": "More paper at a new price",
		"payment_term_code": "NT30"})
	want(t, "total before", ch["total_before"].(map[string]any), map[string]any{"amount": "25.00", "currency": "EUR"})
	want(t, "total after", ch["total_after"].(map[string]any), map[string]any{"amount": "36.00", "currency": "EUR"})
	if tag != `"1"` {
		t.Errorf("change ETag: got %s, want \"1\"", tag)
	}
	order, _ := e.Must(200, "GET", path, "", "")
	want(t, "order while the change is a draft", order, map[string]any{"revision": float64(1)})
	if q := order["items"].([]any)[0].(map[string]any)["quantity"].(map[string]any)["value"]; q != "10" {
		t.Errorf("order quantity while the change is a draft: got %v, want 10", q)
	}
	e.Problem(409, "procurement.change_open", "POST", path+"/changes", body, "")
	cpath := path + "/changes/" + id(ch)
	patched, _ := e.Must(200, "PATCH", cpath, `{"reason": "More paper"}`, tag)
	want(t, "patched change", patched, map[string]any{"reason": "More paper", "version": float64(2)})
	page, _ := e.Must(200, "GET", path+"/changes", "", "")
	if n := len(page["items"].([]any)); n != 1 {
		t.Errorf("changes listed: got %d, want 1", n)
	}
	got, _ := e.Must(200, "GET", cpath, "", "")
	if got["id"] != ch["id"] {
		t.Errorf("read back: got %v, want %v", got["id"], ch["id"])
	}
	cancelled, tag := e.Must(200, "POST", cpath+":cancel", "", `"2"`)
	want(t, "cancelled change", cancelled, map[string]any{"status": "cancelled"})
	e.Problem(409, "procurement.invalid_status", "PATCH", cpath, `{"reason": "x"}`, tag)
	e.Must(201, "POST", path+"/changes", body, "") // the next one
	first, _ := e.Must(200, "GET", path+"/changes?page_size=1", "", "")
	if first["next_page_token"] == nil {
		t.Error("changes on a page of one: got no next page, want one")
	}
	e.Problem(404, "request.not_found", "GET", path+"/changes/"+uuid.NewString(), "", "")
}

// TestOrderChangesChecked: a change is checked as an order is, and keeps
// what a released order keeps.
func TestOrderChangesChecked(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	for name, c := range map[string]struct {
		items string
		code  string
	}{
		"below what was received": {items(item(e.office, "3", "EA", "2.50", "EUR", "")), "procurement.invalid_change"},
		"another account":         {items(item(e.expense, "10", "EA", "2.50", "EUR", "")), "procurement.invalid_change"},
		"another unit":            {items(item(e.office, "10", "KG", "2.50", "EUR", "")), "procurement.invalid_change"},
		"another currency":        {items(item(e.office, "10", "EA", "2.50", "USD", "")), "procurement.currency_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			e := e.in(t)
			e.Problem(422, c.code, "POST", path+"/changes", `{"reason": "r", "items": `+c.items+`}`, "")
		})
	}
	// An item with receipts stays; one without may go.
	two := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", ""), item(e.office, "5", "EA", "1.00", "EUR", "")), "")
	tpath := "/v1/purchase-orders/" + id(two)
	e.act(t, tpath, "release", "")
	e.Must(201, "POST", tpath+"/changes", `{"reason": "No more toner", "items": `+items(item(e.office, "10", "EA", "2.50", "EUR", ""))+`}`, "")
	other := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", ""), item(e.office, "5", "EA", "1.00", "EUR", "")), "")
	opath := "/v1/purchase-orders/" + id(other)
	e.act(t, opath, "release", "")
	if code, gr := e.receipt(t, id(other), "2:1:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	e.Problem(422, "procurement.invalid_change", "POST", opath+"/changes", `{"reason": "r", "items": `+items(item(e.office, "10", "EA", "2.50", "EUR", ""))+`}`, "")
	// Only a released order changes this way.
	draft := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	e.Problem(409, "procurement.invalid_status", "POST", "/v1/purchase-orders/"+id(draft)+"/changes", `{"reason": "r"}`, "")
}

// TestAgentsDraftChanges: an agent may draft a change.
func TestAgentsDraftChanges(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	ch, _ := e.As(e.Tenant.Agent).Must(201, "POST", path+"/changes", fmt.Sprintf(`{"reason": "Proposed by the agent", "items": %s}`,
		items(item(e.office, "11", "EA", "2.50", "EUR", ""))), "")
	want(t, "agent's change", ch, map[string]any{"created_by": e.Tenant.Agent.ID.String(), "status": "draft"})
}

// TestOrderChangesOutsideTheCompany: a caller without access to the
// order's company cannot see its changes, nor change them.
func TestOrderChangesOutsideTheCompany(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	ch, _ := e.Must(201, "POST", path+"/changes", `{"reason": "r"}`, "")
	cpath := path + "/changes/" + id(ch)
	outsider := e.As(e.Tenant.Agent) // the agent acts in no company here
	outsider.Problem(404, "request.not_found", "GET", cpath, "", "")
	outsider.Problem(404, "request.not_found", "GET", path+"/changes", "", "")
	outsider.Problem(404, "request.not_found", "PATCH", cpath, `{"reason": "x"}`, `"1"`)
	outsider.Problem(404, "request.not_found", "POST", cpath+":cancel", "", `"1"`)
}

// TestChangeKeepsTolerance: an item the change leaves the tolerance out of
// keeps its own, so an item over-delivered within it can be repriced.
func TestChangeKeepsTolerance(t *testing.T) {
	e := setup(t)
	o := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", `, "over_delivery_pct": "50"`)), "")
	path := "/v1/purchase-orders/" + id(o)
	e.act(t, path, "release", "")
	if code, gr := e.receipt(t, id(o), "1:14:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	ch, _ := e.Must(201, "POST", path+"/changes", `{"reason": "New price", "items": `+items(item(e.office, "10", "EA", "2.40", "EUR", ""))+`}`, "")
	if got := ch["items"].([]any)[0].(map[string]any)["over_delivery_pct"]; got != "50" {
		t.Errorf("tolerance of the kept item: got %v, want 50", got)
	}
}

// TestOrderChangeEdges: what a change may give besides its items, and
// what it is checked against.
func TestOrderChangeEdges(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	for name, c := range map[string]struct {
		body string
		code string
	}{
		"invalid extension":    {`{"reason": "r", "extensions": {"Bad": 1}}`, "request.invalid_extension"},
		"unknown payment term": {`{"reason": "r", "payment_term_code": "NOPE"}`, "procurement.unknown_payment_term"},
	} {
		t.Run(name, func(t *testing.T) {
			e := e.in(t)
			e.Problem(422, c.code, "POST", path+"/changes", c.body, "")
		})
	}
	// Header fields alone keep the items.
	ch, tag := e.Must(201, "POST", path+"/changes", `{"reason": "Later", "order_date": "2026-03-20", "extensions": {"region": "MKT"}}`, "")
	want(t, "header change", ch, map[string]any{"order_date": "2026-03-20"})
	if n := len(ch["items"].([]any)); n != 1 {
		t.Errorf("items kept: got %d, want 1", n)
	}
	cpath := path + "/changes/" + id(ch)
	e.Problem(412, "request.version_mismatch", "PATCH", cpath, `{"reason": "x"}`, `"9"`)
	patched, _ := e.Must(200, "PATCH", cpath, `{"items": `+items(item(e.office, "9", "EA", "2.50", "EUR", ""))+`}`, tag)
	want(t, "patched total", patched["total_after"].(map[string]any), map[string]any{"amount": "22.50"})
	want(t, "patch keeps the rest", patched, map[string]any{"reason": "Later", "order_date": "2026-03-20"})
	// A closed order's change is cancelled with it; an order no longer
	// released takes none.
	e.act(t, path, "close", "")
	e.Problem(409, "procurement.invalid_status", "PATCH", cpath, `{"reason": "x"}`, "*")
	e.Problem(409, "procurement.invalid_status", "POST", path+"/changes", `{"reason": "x"}`, "")
	e.Problem(404, "request.not_found", "GET", "/v1/purchase-orders/"+uuid.NewString()+"/changes", "", "")
}

// TestOrderChangeFailures calls the change handlers outside a unit of
// work and with each of their database statements failing in turn.
func TestOrderChangeFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	open := e.releasedOrder(t)
	other := e.releasedOrder(t)
	draft, _ := e.Must(201, "POST", other+"/changes", `{"reason": "r"}`, "")
	o, c := uuid.MustParse(open[len("/v1/purchase-orders/"):]), uuidOf(draft)
	po := uuid.MustParse(other[len("/v1/purchase-orders/"):])
	its := []gen.PurchaseOrderItem{{Description: "P", Quantity: gen.Quantity{Value: "10", Unit: "EA"}, UnitPrice: gen.Money{Amount: "2.50", Currency: "EUR"},
		AccountID: uuid.MustParse(e.office)}}
	for name, call := range map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreatePurchaseOrderChange(ctx, &gen.PurchaseOrderChangeCreate{Reason: "r", PaymentTermCode: gen.NewOptString("NT30"),
				OrderDate: gen.NewOptDate(dateOf("2026-03-20"))}, gen.CreatePurchaseOrderChangeParams{ID: o})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetPurchaseOrderChange(ctx, gen.GetPurchaseOrderChangeParams{ID: po, ChangeID: c})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListPurchaseOrderChanges(ctx, gen.ListPurchaseOrderChangesParams{ID: po})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdatePurchaseOrderChange(ctx, &gen.PurchaseOrderChangeUpdate{Items: its}, gen.UpdatePurchaseOrderChangeParams{ID: po, ChangeID: c, IfMatch: "*"})
			return err
		},
		"submit": func(ctx context.Context) error {
			_, err := h.SubmitPurchaseOrderChange(ctx, gen.SubmitPurchaseOrderChangeParams{ID: po, ChangeID: c, IfMatch: "*"})
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := h.CancelPurchaseOrderChange(ctx, gen.CancelPurchaseOrderChangeParams{ID: po, ChangeID: c, IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	pgtest.CheckErr(t, apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListPurchaseOrderChanges(ctx, gen.ListPurchaseOrderChangesParams{ID: po, PageSize: gen.NewOptInt32(0)})
		return err
	}), "request.invalid_page_size")
}
