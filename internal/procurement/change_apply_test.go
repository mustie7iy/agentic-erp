package procurement_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Submitting and applying order changes (T47f2): a change that does not
// raise the order's total, or needs no approval, is applied at once; one
// above the order tiers waits for an order_change approval request.

// draftChange drafts a change of the order at path with the items and
// returns its path and ETag.
func (e env) draftChange(t *testing.T, path, its string) (string, string) {
	t.Helper()
	ch, tag := e.Must(201, "POST", path+"/changes", `{"reason": "As agreed", "items": `+its+`}`, "")
	return path + "/changes/" + id(ch), tag
}

// submitted drafts and submits a change and returns it after the
// submission.
func (e env) submitted(t *testing.T, path, its string) map[string]any {
	t.Helper()
	cpath, tag := e.draftChange(t, path, its)
	ch, _ := e.Must(200, "POST", cpath+":submit", "", tag)
	return ch
}

func orderItem(t *testing.T, order map[string]any, i int) map[string]any {
	t.Helper()
	its := order["items"].([]any)
	if i >= len(its) {
		t.Fatalf("item %d of %d", i+1, len(its))
	}
	return its[i].(map[string]any)
}

func TestApplyOrderChange(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t) // 10 EA at 2.50, 4 received
	before := len(events(t, e, "purchase_order_changed"))
	cpath, tag := e.draftChange(t, path, items(item(e.office, "8", "EA", "2.50", "EUR", ""), item(e.office, "2", "EA", "1.00", "EUR", "")))
	e.Must(200, "PATCH", cpath, `{"order_date": "2026-03-20"}`, tag)
	// The schema version the change was checked against is applied with it.
	pgtest.Unguarded(t, `UPDATE procurement.purchase_order SET extensions_schema = NULL WHERE id = $1`, path[len("/v1/purchase-orders/"):])
	ch, _ := e.Must(200, "POST", cpath+":submit", "", `"2"`)
	want(t, "change lowering the total", ch, map[string]any{"status": "applied", "revision": float64(2), "approval_request_id": nil})
	var schema *int
	pgtest.Scan(t, `SELECT extensions_schema FROM procurement.purchase_order WHERE id = $1`, []any{path[len("/v1/purchase-orders/"):]}, &schema)
	if schema == nil || *schema != 1 {
		t.Errorf("applied change's extensions schema: got %v, want 1", schema)
	}
	order, _ := e.Must(200, "GET", path, "", "")
	want(t, "changed order", order, map[string]any{"revision": float64(2), "order_date": "2026-03-20", "status": "released"})
	want(t, "changed total", order["total_amount"].(map[string]any), map[string]any{"amount": "22.00"})
	want(t, "kept item", orderItem(t, order, 0)["quantity"].(map[string]any), map[string]any{"value": "8"})
	want(t, "kept item's valuation", orderItem(t, order, 0)["valuation_price"].(map[string]any), map[string]any{"amount": "2.50"})
	want(t, "added item's valuation", orderItem(t, order, 1)["valuation_price"].(map[string]any), map[string]any{"amount": "1.00"})
	published := events(t, e, "purchase_order_changed")
	if len(published) != before+1 {
		t.Fatalf("PurchaseOrderChanged events: got %d, want %d", len(published), before+1)
	}
	last := published[len(published)-1]
	want(t, "event", last, map[string]any{"revision": float64(2), "change_id": ch["id"], "reason": "As agreed", "order_date": "2026-03-20"})
	want(t, "event total after", last["total_after"].(map[string]any), map[string]any{"amount": "22.00", "currency": "EUR"})
	// A change raising the total within the first tier needs no approval.
	next := e.submitted(t, path, items(item(e.office, "12", "EA", "2.50", "EUR", ""), item(e.office, "2", "EA", "1.00", "EUR", "")))
	want(t, "change within the first tier", next, map[string]any{"status": "applied", "revision": float64(3), "base_revision": float64(2)})
	e.Problem(409, "procurement.invalid_status", "POST", path+"/changes/"+id(next)+":submit", "", "*")
}

// TestOrderChangeApproval: a change above the first tier waits for an
// order_change request on its new total; approved it is applied at the
// new price, the valuation price staying the one released at.
func TestOrderChangeApproval(t *testing.T) {
	e := setup(t)
	path := "/v1/purchase-orders/" + e.big(t, "90.00") // 900.00, within the first tier
	e.act(t, path, "release", "")
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	agent := e.As(e.Tenant.Agent)
	ch, tag := agent.Must(201, "POST", path+"/changes", `{"reason": "Dearer", "items": `+items(item(e.office, "10", "EA", "150.00", "EUR", ""))+`}`, "")
	cpath := path + "/changes/" + id(ch)
	pending, tag := agent.Must(200, "POST", cpath+":submit", "", tag)
	want(t, "change above the first tier", pending, map[string]any{"status": "pending_approval", "revision": nil})
	r := e.Pending(t, id(ch))
	want(t, "request", r, map[string]any{"object_type": "order_change", "id": pending["approval_request_id"], "requester_id": e.Tenant.Agent.ID.String()})
	// The change is read through its order: the request says where.
	want(t, "request's object", r, map[string]any{"object_resource": cpath})
	want(t, "request amount", r["amount"].(map[string]any), map[string]any{"amount": "1500.00", "currency": "EUR"})
	e.Problem(409, "procurement.invalid_status", "PATCH", cpath, `{"reason": "x"}`, tag)
	order, _ := e.Must(200, "GET", path, "", "")
	want(t, "order while the change waits", order, map[string]any{"revision": float64(1)})
	e.Approve(t, id(ch))
	applied, _ := e.Must(200, "GET", cpath, "", "")
	want(t, "approved change", applied, map[string]any{"status": "applied", "revision": float64(2), "approval_request_id": pending["approval_request_id"]})
	order, _ = e.Must(200, "GET", path, "", "")
	want(t, "approved price", orderItem(t, order, 0)["unit_price"].(map[string]any), map[string]any{"amount": "150.00"})
	want(t, "valuation price", orderItem(t, order, 0)["valuation_price"].(map[string]any), map[string]any{"amount": "90.00"})
}

func TestOrderChangeRejectedOrWithdrawn(t *testing.T) {
	e := setup(t)
	path := "/v1/purchase-orders/" + e.big(t, "90.00")
	e.act(t, path, "release", "")
	dearer := items(item(e.office, "10", "EA", "150.00", "EUR", ""))
	ch := e.submitted(t, path, dearer)
	r := e.Pending(t, id(ch))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+id(r)+":reject", `{"comment": "No"}`, fmt.Sprintf(`"%v"`, r["version"]))
	got, _ := e.Must(200, "GET", path+"/changes/"+id(ch), "", "")
	want(t, "rejected change", got, map[string]any{"status": "rejected", "approval_request_id": r["id"]})
	ch = e.submitted(t, path, dearer) // a rejected change is not open
	r = e.Pending(t, id(ch))
	e.Must(200, "POST", "/v1/approval-requests/"+id(r)+":cancel", `{"comment": "Wrong price"}`, fmt.Sprintf(`"%v"`, r["version"]))
	got, tag := e.Must(200, "GET", path+"/changes/"+id(ch), "", "")
	want(t, "withdrawn change", got, map[string]any{"status": "draft", "approval_request_id": nil})
	e.Must(200, "PATCH", path+"/changes/"+id(ch), `{"reason": "Cheaper after all"}`, tag)
	order, _ := e.Must(200, "GET", path, "", "")
	want(t, "order after the rejection and the withdrawal", order, map[string]any{"revision": float64(1)})
	// Cancelling the order cancels the open change and withdraws its request.
	_, tag = e.Must(200, "GET", path+"/changes/"+id(ch), "", "")
	e.Must(200, "POST", path+"/changes/"+id(ch)+":submit", "", tag)
	r = e.Pending(t, id(ch))
	e.act(t, path, "cancel", "")
	got, _ = e.Must(200, "GET", path+"/changes/"+id(ch), "", "")
	want(t, "change of the cancelled order", got, map[string]any{"status": "cancelled"})
	req, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+id(r), "", "")
	want(t, "request of the cancelled order's change", req, map[string]any{"status": "cancelled"})
}

// TestClosingCancelsTheChange: an order closed cancels its open change.
func TestClosingCancelsTheChange(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	cpath, _ := e.draftChange(t, path, items(item(e.office, "9", "EA", "2.50", "EUR", "")))
	e.act(t, path, "close", "")
	got, _ := e.Must(200, "GET", cpath, "", "")
	want(t, "change of the closed order", got, map[string]any{"status": "cancelled"})
}

// TestOrderChangeChecksAgain: receipts posted after a change was drafted
// refuse its submission, and its approval, when it would fall below them.
func TestOrderChangeChecksAgain(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t) // 4 of 10 received
	cpath, tag := e.draftChange(t, path, items(item(e.office, "5", "EA", "2.50", "EUR", "")))
	if code, gr := e.receipt(t, path[len("/v1/purchase-orders/"):], "1:2:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	e.Problem(422, "procurement.invalid_change", "POST", cpath+":submit", "", tag)

	big := e.big(t, "90.00")
	bpath := "/v1/purchase-orders/" + big
	e.act(t, bpath, "release", "")
	ch := e.submitted(t, bpath, items(item(e.office, "6", "EA", "250.00", "EUR", ""))) // 1500.00
	if code, gr := e.receipt(t, big, "1:8:EA"); code != 201 {
		t.Fatalf("receipt: got %d %v, want 201", code, gr)
	}
	r := e.Pending(t, id(ch))
	e.As(e.Approver).Problem(422, "procurement.invalid_change", "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	got, _ := e.Must(200, "GET", bpath+"/changes/"+id(ch), "", "")
	want(t, "change after the refused approval", got, map[string]any{"status": "pending_approval"})
}

// TestRemovedItemKeepsItsIdentity: an item a change removed is added back
// only as it was: on its account, unit, material and contract item.
func TestRemovedItemKeepsItsIdentity(t *testing.T) {
	e := setup(t)
	o := e.order(t, items(item(e.office, "1", "EA", "2.50", "EUR", ""), item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	path := "/v1/purchase-orders/" + id(o)
	e.act(t, path, "release", "")
	if got := e.submitted(t, path, items(item(e.office, "1", "EA", "2.50", "EUR", ""))); got["status"] != "applied" {
		t.Fatalf("change removing item 2: got %v, want applied", got["status"])
	}
	e.Problem(422, "procurement.invalid_change", "POST", path+"/changes",
		`{"reason": "r", "items": `+items(item(e.office, "1", "EA", "2.50", "EUR", ""), item(e.expense, "1", "EA", "1.00", "EUR", ""))+`}`, "")
	back := e.submitted(t, path, items(item(e.office, "1", "EA", "2.50", "EUR", ""), item(e.office, "1", "EA", "1.20", "EUR", "")))
	if back["status"] != "applied" {
		t.Errorf("item 2 added back as it was: got %v, want applied", back["status"])
	}
	order, _ := e.Must(200, "GET", path, "", "")
	want(t, "item added back", orderItem(t, order, 1)["unit_price"].(map[string]any), map[string]any{"amount": "1.20"})
	want(t, "valuation of the item added back", orderItem(t, order, 1)["valuation_price"].(map[string]any), map[string]any{"amount": "1.00"})
}

// TestContractOrderChange: a contract order's change consumes the
// difference of its total, and an increase is refused when what remains
// does not cover it.
func TestContractOrderChange(t *testing.T) {
	e := setup(t)
	c := e.contract(t, e.Supplier.String(), "") // target 1000.00, item 1 at 5.00 EA
	cp := "/v1/contracts/" + id(c)
	e.act(t, cp, "release", "")
	o := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", `, "contract_item_no": 1`)), fmt.Sprintf(`, "contract_id": %q`, id(c)))
	path := "/v1/purchase-orders/" + id(o)
	e.act(t, path, "release", "")
	consumed := func(want string) {
		t.Helper()
		got, _ := e.Must(200, "GET", cp, "", "")
		if v := got["consumed_value"].(map[string]any)["amount"]; v != want {
			t.Errorf("consumed value: got %v, want %s", v, want)
		}
	}
	consumed("50.00")
	e.submitted(t, path, items(item(e.office, "20", "EA", "5.00", "EUR", `, "contract_item_no": 1`)))
	consumed("100.00")
	e.submitted(t, path, items(item(e.office, "5", "EA", "5.00", "EUR", `, "contract_item_no": 1`)))
	consumed("25.00")
	cpath, tag := e.draftChange(t, path, items(item(e.office, "250", "EA", "5.00", "EUR", `, "contract_item_no": 1`))) // 1250.00
	e.Problem(409, "procurement.contract_exceeded", "POST", cpath+":submit", "", tag)
	consumed("25.00")
}

// TestOrderChangeDecisionFailures: every statement of a submission and of
// the decisions on its request reports its failure, and none runs outside
// a unit of work.
func TestOrderChangeDecisionFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	changes := procurement.OrderChanges{Endpoints: h}
	path := "/v1/purchase-orders/" + e.big(t, "90.00")
	e.act(t, path, "release", "")
	order := uuid.MustParse(path[len("/v1/purchase-orders/"):])
	// 1501.00, above the first tier; the item it adds is checked against
	// the ledger as it is submitted and approved.
	cpath, _ := e.draftChange(t, path, items(item(e.office, "10", "EA", "150.00", "EUR", ""), item(e.office, "1", "EA", "1.00", "EUR", "")))
	change := uuid.MustParse(cpath[len(path+"/changes/"):])
	submit := func(ctx context.Context) (uuid.UUID, error) {
		got, err := h.SubmitPurchaseOrderChange(ctx, gen.SubmitPurchaseOrderChangeParams{ID: order, ChangeID: change, IfMatch: "*"})
		if err != nil {
			return uuid.Nil, err
		}
		return got.(*gen.UpdatePurchaseOrderChangeOKHeaders).Response.ApprovalRequestID.Value, nil
	}
	for name, decide := range map[string]func(ctx context.Context, request uuid.UUID) error{
		"submit":   func(context.Context, uuid.UUID) error { return nil },
		"approve":  func(ctx context.Context, r uuid.UUID) error { return changes.Approved(ctx, change, r) },
		"reject":   func(ctx context.Context, r uuid.UUID) error { return changes.Rejected(ctx, change, r, "no") },
		"withdraw": func(ctx context.Context, r uuid.UUID) error { return changes.Cancelled(ctx, change, r, "no") },
		"close the order": func(ctx context.Context, _ uuid.UUID) error {
			_, err := h.ClosePurchaseOrder(ctx, gen.ClosePurchaseOrderParams{ID: order, IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := decide(context.Background(), uuid.New()); name != "submit" && err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, func(ctx context.Context) error {
				request, err := submit(ctx)
				if err != nil {
					return err
				}
				return decide(ctx, request)
			})
		})
	}
	t.Run("another request", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			if _, err := submit(ctx); err != nil {
				return err
			}
			return changes.Approved(ctx, change, uuid.New())
		})
		pgtest.WantErr(t, err, "does not wait for request")
	})
	t.Run("applied at once", func(t *testing.T) {
		// A contract order, so that its consumption moves too.
		c := e.contract(t, e.Supplier.String(), "")
		e.act(t, "/v1/contracts/"+id(c), "release", "")
		o := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", `, "contract_item_no": 1`)), fmt.Sprintf(`, "contract_id": %q`, id(c)))
		opath := "/v1/purchase-orders/" + id(o)
		e.act(t, opath, "release", "")
		raised, _ := e.draftChange(t, opath, items(item(e.office, "20", "EA", "5.00", "EUR", `, "contract_item_no": 1`)))
		po, ch := uuidOf(o), uuid.MustParse(raised[len(opath+"/changes/"):])
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "apply", func(ctx context.Context) error {
			_, err := h.SubmitPurchaseOrderChange(ctx, gen.SubmitPurchaseOrderChangeParams{ID: po, ChangeID: ch, IfMatch: "*"})
			return err
		})
	})
}
