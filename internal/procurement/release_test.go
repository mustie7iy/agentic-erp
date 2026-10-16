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

// The release strategy (T47d): an order released without an approved
// requisition, or above its requisition's total, waits for an approval by
// amount tier (purchasing.order_release_approval: up to 1000.00 none, up
// to 5000.00 Z_PURCH_MGR, above Z_PURCH_MGR then Z_PURCH_HEAD).

// big is a draft order of 10 EA at price, without a requisition.
func (e env) big(t *testing.T, price string) string {
	t.Helper()
	return id(e.order(t, items(item(e.office, "10", "EA", price, "EUR", "")), ""))
}

func TestReleaseStrategy(t *testing.T) {
	e := setup(t)
	small := e.act(t, "/v1/purchase-orders/"+e.big(t, "100.00"), "release", "")
	if small["status"] != "released" {
		t.Errorf("an order within the first tier: got %v, want released at once", small["status"])
	}
	order := e.big(t, "150.00") // 1500.00
	path := "/v1/purchase-orders/" + order
	pending := e.act(t, path, "release", "")
	if pending["status"] != "pending_approval" {
		t.Fatalf("an order above the first tier: got %v, want pending_approval", pending["status"])
	}
	if pending["valuation"] != "direct" {
		t.Errorf("pending valuation: got %v, want direct until released", pending["valuation"])
	}
	r := e.Pending(t, order)
	want(t, "request", r, map[string]any{"object_type": "purchase_order", "requester_id": e.Tenant.Human.ID.String(), "id": pending["release_request_id"]})
	want(t, "request amount", r["amount"].(map[string]any), map[string]any{"amount": "1500.00", "currency": "EUR"})
	version := fmt.Sprintf(`"%v"`, r["version"])
	e.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "Mine"}`, version)
	_, tag := e.Must(200, "GET", path, "", "")
	e.Problem(409, "procurement.invalid_status", "PATCH", path, `{"order_date": "2026-03-11"}`, tag)
	e.Problem(409, "procurement.invalid_status", "POST", path+":release", "", tag)
	before := len(events(t, e, "purchase_order_released"))
	e.Approve(t, order)
	got, _ := e.Must(200, "GET", path, "", "")
	want(t, "approved order", got, map[string]any{"status": "released", "release_request_id": nil})
	if n := len(events(t, e, "purchase_order_released")); n != before+1 {
		t.Errorf("PurchaseOrderReleased events: got %d, want %d", n, before+1)
	}
}

func TestReleaseRejectedOrWithdrawn(t *testing.T) {
	e := setup(t)
	order := e.big(t, "150.00")
	path := "/v1/purchase-orders/" + order
	e.act(t, path, "release", "")
	r := e.Pending(t, order)
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+id(r)+":reject", `{"comment": "Too much"}`, fmt.Sprintf(`"%v"`, r["version"]))
	got, tag := e.Must(200, "GET", path, "", "")
	want(t, "rejected order", got, map[string]any{"status": "draft", "release_request_id": nil})
	e.Must(200, "PATCH", path, `{"order_date": "2026-03-11"}`, tag)
	e.act(t, path, "release", "")
	r = e.Pending(t, order)
	e.Must(200, "POST", "/v1/approval-requests/"+id(r)+":cancel", `{"comment": "Wrong supplier"}`, fmt.Sprintf(`"%v"`, r["version"]))
	got, _ = e.Must(200, "GET", path, "", "")
	want(t, "withdrawn order", got, map[string]any{"status": "draft", "release_request_id": nil})
	// Cancelling a pending order withdraws its request.
	e.act(t, path, "release", "")
	r = e.Pending(t, order)
	if got := e.act(t, path, "cancel", ""); got["status"] != "cancelled" {
		t.Errorf("cancelled pending order: got %v, want cancelled", got["status"])
	}
	req, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+id(r), "", "")
	if req["status"] != "cancelled" {
		t.Errorf("request of the cancelled order: got %v, want cancelled", req["status"])
	}
}

// TestReleaseApprovalChecksAgain: an approval releases the order as the
// release does, so a supplier blocked meanwhile refuses the decision and
// the request stays pending.
func TestReleaseApprovalChecksAgain(t *testing.T) {
	e := setup(t)
	order := e.big(t, "150.00")
	e.act(t, "/v1/purchase-orders/"+order, "release", "")
	r := e.Pending(t, order)
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	e.As(e.Approver).Problem(422, "procurement.supplier_not_usable", "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	got, _ := e.Must(200, "GET", "/v1/purchase-orders/"+order, "", "")
	if got["status"] != "pending_approval" {
		t.Errorf("order after the refused approval: got %v, want pending_approval", got["status"])
	}
}

// TestReleaseFromARequisition: an order from an approved requisition is
// released at once within the requisition's total, and by amount tier
// above it.
func TestReleaseFromARequisition(t *testing.T) {
	e := setup(t)
	order := func(price string) string {
		r := e.requisition(t, items(item(e.office, "10", "EA", "150.00", "EUR", "")), fmt.Sprintf(`, "supplier_id": %q`, e.Supplier))
		e.act(t, "/v1/requisitions/"+id(r), "submit", "")
		e.Approve(t, id(r))
		return id(e.order(t, items(item(e.office, "10", "EA", price, "EUR", "")), fmt.Sprintf(`, "requisition_id": %q`, id(r))))
	}
	if got := e.act(t, "/v1/purchase-orders/"+order("150.00"), "release", ""); got["status"] != "released" {
		t.Errorf("within the requisition: got %v, want released", got["status"])
	}
	if got := e.act(t, "/v1/purchase-orders/"+order("150.01"), "release", ""); got["status"] != "pending_approval" {
		t.Errorf("above the requisition: got %v, want pending_approval", got["status"])
	}
}

// TestAgentsRelease: an agent may release an order; a person approves it.
func TestAgentsRelease(t *testing.T) {
	e := setup(t)
	order := e.big(t, "150.00")
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	agent := e.As(e.Tenant.Agent)
	_, tag := agent.Must(200, "GET", "/v1/purchase-orders/"+order, "", "")
	got, _ := agent.Must(200, "POST", "/v1/purchase-orders/"+order+":release", "", tag)
	if got["status"] != "pending_approval" {
		t.Fatalf("released by an agent: got %v, want pending_approval", got["status"])
	}
	want(t, "request", e.Pending(t, order), map[string]any{"requester_id": e.Tenant.Agent.ID.String()})
}

func want(t *testing.T, what string, got map[string]any, fields map[string]any) {
	t.Helper()
	for field, w := range fields {
		if got[field] != w {
			t.Errorf("%s %s: got %v, want %v", what, field, got[field], w)
		}
	}
}

// TestReleaseFailures: every statement of a submission and of the
// decisions on its request reports its failure, and none runs outside a
// unit of work.
func TestReleaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	releases := procurement.OrderReleases{Endpoints: h}
	order := uuid.MustParse(e.big(t, "150.00"))
	// submit releases the order to its approval and returns the request.
	submit := func(ctx context.Context) (uuid.UUID, error) {
		got, err := h.ReleasePurchaseOrder(ctx, gen.ReleasePurchaseOrderParams{ID: order, IfMatch: "*"})
		if err != nil {
			return uuid.Nil, err
		}
		return got.(*gen.UpdatePurchaseOrderOKHeaders).Response.ReleaseRequestID.Value, nil
	}
	for name, decide := range map[string]func(ctx context.Context, request uuid.UUID) error{
		"submit":   func(context.Context, uuid.UUID) error { return nil },
		"approve":  func(ctx context.Context, r uuid.UUID) error { return releases.Approved(ctx, order, r) },
		"reject":   func(ctx context.Context, r uuid.UUID) error { return releases.Rejected(ctx, order, r, "no") },
		"withdraw": func(ctx context.Context, r uuid.UUID) error { return releases.Cancelled(ctx, order, r, "no") },
		"cancel the order": func(ctx context.Context, _ uuid.UUID) error {
			_, err := h.CancelPurchaseOrder(ctx, gen.CancelPurchaseOrderParams{ID: order, IfMatch: "*"})
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
	// A decision on a request the order does not wait for changes nothing.
	t.Run("another request", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			if _, err := submit(ctx); err != nil {
				return err
			}
			return releases.Approved(ctx, order, uuid.New())
		})
		pgtest.WantErr(t, err, "does not wait for request")
	})
}

// TestReleaseValuedAtRelease: items are valued when the order is
// released, so a release rejected and the price changed values them at
// the price released.
func TestReleaseValuedAtRelease(t *testing.T) {
	e := setup(t)
	order := e.big(t, "150.00")
	path := "/v1/purchase-orders/" + order
	e.act(t, path, "release", "")
	r := e.Pending(t, order)
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+id(r)+":reject", `{"comment": "Too dear"}`, fmt.Sprintf(`"%v"`, r["version"]))
	_, tag := e.Must(200, "GET", path, "", "")
	e.Must(200, "PATCH", path, `{"items": `+items(item(e.office, "10", "EA", "100.00", "EUR", ""))+`}`, tag)
	released := e.act(t, path, "release", "") // 1000.00: within the first tier
	if released["status"] != "released" {
		t.Fatalf("released at 100.00: got %v, want released", released["status"])
	}
	it := released["items"].([]any)[0].(map[string]any)
	want(t, "valuation price", it["valuation_price"].(map[string]any), map[string]any{"amount": "100.00", "currency": "EUR"})
}

// TestReleaseInAnotherCurrencyThanTheRequisition: an order whose currency
// changed since its requisition is released by the strategy, as their
// totals cannot be compared.
func TestReleaseInAnotherCurrencyThanTheRequisition(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	r := e.requisition(t, items(item(e.office, "10", "EA", "150.00", "EUR", "")), fmt.Sprintf(`, "supplier_id": %q`, e.Supplier))
	e.act(t, "/v1/requisitions/"+id(r), "submit", "")
	e.Approve(t, id(r))
	o := e.order(t, items(item(e.office, "10", "EA", "150.00", "EUR", "")), fmt.Sprintf(`, "requisition_id": %q`, id(r)))
	path := "/v1/purchase-orders/" + id(o)
	_, tag := e.Must(200, "GET", path, "", "")
	e.Must(200, "PATCH", path, `{"currency": "USD", "items": `+items(item(e.office, "10", "EA", "150.00", "USD", ""))+`}`, tag)
	if got := e.act(t, path, "release", ""); got["status"] != "pending_approval" {
		t.Errorf("released in USD from a EUR requisition: got %v, want pending_approval", got["status"])
	}
}

// TestReleaseApprovedOutsideTheCompany: the request authorises the
// release, so an approver who may not act in the order's legal entity
// releases a contract order too.
func TestReleaseApprovedOutsideTheCompany(t *testing.T) {
	e := setup(t)
	c, _ := e.Must(201, "POST", "/v1/contracts", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01",
		"valid_to": "2026-12-31", "target_value": {"amount": "100000.00", "currency": "EUR"}, "items": [
		{"material_key": "A", "description": "Paper", "unit_price": {"amount": "150.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q}]}`,
		e.DE, e.Supplier, e.office), "")
	e.act(t, "/v1/contracts/"+id(c), "release", "")
	o := e.order(t, items(item(e.office, "10", "EA", "150.00", "EUR", `, "contract_item_no": 1`)), fmt.Sprintf(`, "contract_id": %q`, id(c)))
	e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	pgtest.Exec(t, `UPDATE identity.principal SET all_companies = false, version = version + 1 WHERE id = $1`, e.Approver.ID)
	r := e.Pending(t, id(o))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+id(r)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
	got, _ := e.Must(200, "GET", "/v1/purchase-orders/"+id(o), "", "")
	if got["status"] != "released" {
		t.Errorf("approved outside the company: got %v, want released", got["status"])
	}
}
