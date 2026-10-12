package procurement_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// endpoints are wired as the API wires them.
func endpoints() *procurement.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &procurement.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{}, Accounts: ledger.Accounts{},
		Numbers: configuration.Numbers{}, Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}}, Settings: configuration.Settings{},
		Approvals: fixture.Approvals()}
}

func genCancel(id string) gen.CancelPurchaseOrderParams {
	return gen.CancelPurchaseOrderParams{ID: uuid.MustParse(id), IfMatch: "*"}
}

func TestLists(t *testing.T) {
	e := setup(t)
	ok := items(item(e.office, "1", "EA", "1.00", "EUR", ""))
	for range 3 {
		e.requisition(t, ok, fmt.Sprintf(`, "supplier_id": %q`, e.Supplier))
	}
	reqs := func(q string) (int, string) {
		page, _ := e.Must(200, "GET", "/v1/requisitions?"+q, "", "")
		tok, _ := page["next_page_token"].(string)
		return len(page["items"].([]any)), tok
	}
	if n, _ := reqs(fmt.Sprintf("legal_entity_id=%s&status=draft&requester_id=%s&document_number=PR100001&expand=supplier", e.DE, e.Tenant.Human.ID)); n != 1 {
		t.Fatalf("filtered requisitions: %d", n)
	}
	for _, order := range []string{"created_at", "document_number"} {
		n, tok := reqs("page_size=2&order_by=" + order)
		if n != 2 || tok == "" {
			t.Fatalf("%s page 1: %d", order, n)
		}
		if n, tok := reqs("page_size=2&order_by=" + order + "&page_token=" + tok); n != 1 || tok != "" {
			t.Fatalf("%s page 2: %d", order, n)
		}
	}
	for _, d := range []string{"2026-03-01", "2026-03-05", "2026-03-09"} {
		e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": %q, "items": %s}`,
			e.DE, e.Supplier, d, ok), "")
	}
	orders := func(q string) (int, string) {
		page, _ := e.Must(200, "GET", "/v1/purchase-orders?"+q, "", "")
		tok, _ := page["next_page_token"].(string)
		return len(page["items"].([]any)), tok
	}
	if n, _ := orders(fmt.Sprintf("legal_entity_id=%s&supplier_id=%s&status=draft&order_date_from=2026-03-02&order_date_to=2026-03-08&expand=supplier", e.DE, e.Supplier)); n != 1 {
		t.Fatalf("filtered orders: %d", n)
	}
	if n, _ := orders("document_number=PO100002"); n != 1 {
		t.Fatalf("order by number: %d", n)
	}
	for _, order := range []string{"order_date", "document_number"} {
		n, tok := orders("page_size=2&order_by=" + order)
		if n != 2 || tok == "" {
			t.Fatalf("%s page 1: %d", order, n)
		}
		if n, _ := orders("page_size=2&order_by=" + order + "&page_token=" + tok); n != 1 {
			t.Fatalf("%s page 2: %d", order, n)
		}
	}
}

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	ok := []gen.RequisitionItem{{Description: "P", Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.Money{Amount: "1.00", Currency: "EUR"},
		AccountID: uuid.MustParse(e.office)}}
	poItems := []gen.PurchaseOrderItem{{Description: "P", Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.Money{Amount: "1.00", Currency: "EUR"},
		AccountID: uuid.MustParse(e.office), TaxCode: gen.NewOptString("V1"), ContractItemNo: gen.NewOptInt32(1),
		OverDeliveryPct: gen.NewOptDecimal("5")}}
	cItems := []gen.ContractItem{{MaterialKey: "A", Description: "P", UnitPrice: gen.Money{Amount: "1.00", Currency: "EUR"}, UomCode: "EA",
		AccountID: uuid.MustParse(e.office)}}
	plant, org := e.orgUnit(t, "plant", "PL1"), e.orgUnit(t, "purchasing_org", "P1")
	draftReq := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	pendingReq := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), fmt.Sprintf(`, "supplier_id": %q`, e.Supplier))
	e.act(t, "/v1/requisitions/"+id(pendingReq), "submit", "")
	approvedReq := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	e.act(t, "/v1/requisitions/"+id(approvedReq), "submit", "")
	e.Approve(t, id(approvedReq))
	contract := e.contract(t, e.Supplier.String(), "")
	active := e.contract(t, e.Supplier.String(), "")
	e.act(t, "/v1/contracts/"+id(active), "release", "")
	draftPO := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", `, "contract_item_no": 1`)), fmt.Sprintf(`, "contract_id": %q`, id(active)))
	released := e.order(t, items(item(e.office, "10", "EA", "1.00", "EUR", `, "contract_item_no": 1`)), fmt.Sprintf(`, "contract_id": %q, "requisition_id": %q`, id(active), id(approvedReq)))
	e.act(t, "/v1/purchase-orders/"+id(released), "release", "")
	_, gr := e.receipt(t, id(released), "1:2:EA")
	closable := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	e.act(t, "/v1/purchase-orders/"+id(closable), "release", "")
	u := func(m map[string]any) uuid.UUID { return uuid.MustParse(id(m)) }
	d := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	sup := gen.NewOptUUID(e.Supplier)

	calls := map[string]func(ctx context.Context) error{
		"create requisition": func(ctx context.Context) error {
			_, err := h.CreateRequisition(ctx, &gen.RequisitionCreate{LegalEntityID: e.DE, Currency: "EUR", Items: ok, SupplierID: sup,
				ContractID: gen.NewOptUUID(u(active))}, gen.CreateRequisitionParams{})
			return err
		},
		"get requisition": func(ctx context.Context) error {
			_, err := h.GetRequisition(ctx, gen.GetRequisitionParams{ID: u(pendingReq), Expand: []string{"supplier"}})
			return err
		},
		"list requisitions": func(ctx context.Context) error {
			_, err := h.ListRequisitions(ctx, gen.ListRequisitionsParams{Expand: []string{"supplier"}})
			return err
		},
		"update requisition": func(ctx context.Context) error {
			_, err := h.UpdateRequisition(ctx, &gen.RequisitionUpdate{Items: ok, SupplierID: gen.NewOptNilUUID(e.Supplier), ContractID: gen.NewOptNilUUID(u(active))},
				gen.UpdateRequisitionParams{ID: u(draftReq), IfMatch: "*"})
			return err
		},
		"submit requisition": func(ctx context.Context) error {
			_, err := h.SubmitRequisition(ctx, gen.SubmitRequisitionParams{ID: u(draftReq), IfMatch: "*"})
			return err
		},
		"cancel requisition": func(ctx context.Context) error {
			_, err := h.CancelRequisition(ctx, gen.CancelRequisitionParams{ID: u(pendingReq), IfMatch: "*"})
			return err
		},
		"approved": func(ctx context.Context) error {
			return procurement.Approvals{}.Approved(ctx, u(pendingReq), uuid.New())
		},
		"rejected": func(ctx context.Context) error {
			return procurement.Approvals{}.Rejected(ctx, u(pendingReq), uuid.New(), "c")
		},
		"cancelled": func(ctx context.Context) error {
			return procurement.Approvals{}.Cancelled(ctx, u(pendingReq), uuid.New(), "c")
		},
		"create contract": func(ctx context.Context) error {
			_, err := h.CreateContract(ctx, &gen.ContractCreate{LegalEntityID: e.DE, SupplierID: e.Supplier, Currency: "EUR", ValidFrom: d("2026-01-01"),
				ValidTo: d("2026-12-31"), TargetValue: gen.Money{Amount: "1.00", Currency: "EUR"}, Items: cItems}, gen.CreateContractParams{})
			return err
		},
		"get contract": func(ctx context.Context) error {
			_, err := h.GetContract(ctx, gen.GetContractParams{ID: u(contract)})
			return err
		},
		"list contracts": func(ctx context.Context) error {
			_, err := h.ListContracts(ctx, gen.ListContractsParams{})
			return err
		},
		"update contract": func(ctx context.Context) error {
			_, err := h.UpdateContract(ctx, &gen.ContractUpdate{Items: cItems, TargetValue: gen.NewOptMoney(gen.Money{Amount: "2.00", Currency: "EUR"})},
				gen.UpdateContractParams{ID: u(contract), IfMatch: "*"})
			return err
		},
		"release contract": func(ctx context.Context) error {
			_, err := h.ReleaseContract(ctx, gen.ReleaseContractParams{ID: u(contract), IfMatch: "*"})
			return err
		},
		"cancel contract": func(ctx context.Context) error {
			_, err := h.CancelContract(ctx, gen.CancelContractParams{ID: u(contract), IfMatch: "*"})
			return err
		},
		"terminate contract": func(ctx context.Context) error {
			_, err := h.TerminateContract(ctx, &gen.ReasonInput{Reason: "r"}, gen.TerminateContractParams{ID: u(active), IfMatch: "*"})
			return err
		},
		"create order": func(ctx context.Context) error {
			_, err := h.CreatePurchaseOrder(ctx, &gen.PurchaseOrderCreate{LegalEntityID: e.DE, SupplierID: e.Supplier, Currency: "EUR", OrderDate: d("2026-03-10"),
				Items: poItems, ContractID: gen.NewOptUUID(u(active)), PurchasingOrgID: gen.NewOptUUID(uuid.MustParse(org)),
				PaymentTermCode: gen.NewOptString("NT60")}, gen.CreatePurchaseOrderParams{})
			return err
		},
		"get order": func(ctx context.Context) error {
			_, err := h.GetPurchaseOrder(ctx, gen.GetPurchaseOrderParams{ID: u(draftPO), Expand: []string{"supplier"}})
			return err
		},
		"list orders": func(ctx context.Context) error {
			_, err := h.ListPurchaseOrders(ctx, gen.ListPurchaseOrdersParams{Expand: []string{"supplier"}})
			return err
		},
		"update order": func(ctx context.Context) error {
			_, err := h.UpdatePurchaseOrder(ctx, &gen.PurchaseOrderUpdate{PaymentTermCode: gen.NewOptString("NT60"), SupplierID: gen.NewOptUUID(e.Vendor),
				ContractID: gen.OptNilUUID{Set: true, Null: true}, PurchasingOrgID: gen.NewOptNilUUID(uuid.MustParse(org)),
				Items: []gen.PurchaseOrderItem{{Description: "P", Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.Money{Amount: "1.00", Currency: "EUR"},
					AccountID: uuid.MustParse(e.office)}}}, gen.UpdatePurchaseOrderParams{ID: u(draftPO), IfMatch: "*"})
			return err
		},
		"release order": func(ctx context.Context) error {
			_, err := h.ReleasePurchaseOrder(ctx, gen.ReleasePurchaseOrderParams{ID: u(draftPO), IfMatch: "*"})
			return err
		},
		"close order": func(ctx context.Context) error {
			_, err := h.ClosePurchaseOrder(ctx, gen.ClosePurchaseOrderParams{ID: u(closable), IfMatch: "*"})
			return err
		},
		"post receipt": func(ctx context.Context) error {
			_, err := h.PostGoodsReceipt(ctx, &gen.GoodsReceiptCreate{LegalEntityID: e.DE, PlantID: gen.NewOptUUID(uuid.MustParse(plant)), DocumentDate: d("2026-03-15"),
				PostingDate: d("2026-03-15"), Items: []gen.GoodsReceiptItemInput{{PurchaseOrderID: u(released), PoItemNo: 1,
					Quantity: gen.Quantity{Value: "1", Unit: "EA"}}}}, gen.PostGoodsReceiptParams{})
			return err
		},
		"get receipt": func(ctx context.Context) error {
			_, err := h.GetGoodsReceipt(ctx, gen.GetGoodsReceiptParams{ID: u(gr)})
			return err
		},
		"list receipts": func(ctx context.Context) error {
			_, err := h.ListGoodsReceipts(ctx, gen.ListGoodsReceiptsParams{})
			return err
		},
		"reverse receipt": func(ctx context.Context) error {
			_, err := h.ReverseGoodsReceipt(ctx, gen.OptGoodsReceiptReverse{}, gen.ReverseGoodsReceiptParams{ID: u(gr), IfMatch: "*"})
			return err
		},
	}
	for name, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
	}
	// An order from a requisition, cancelled: the requisition becomes approved again.
	fromReq := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	e.act(t, "/v1/requisitions/"+id(fromReq), "submit", "")
	e.Approve(t, id(fromReq))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "order from requisition", func(ctx context.Context) error {
		o, err := h.CreatePurchaseOrder(ctx, &gen.PurchaseOrderCreate{LegalEntityID: e.DE, SupplierID: e.Supplier, Currency: "EUR", OrderDate: d("2026-03-10"),
			Items: poItems[:1:1], RequisitionID: gen.NewOptUUID(u(fromReq)), ContractID: gen.NewOptUUID(u(active))}, gen.CreatePurchaseOrderParams{})
		if err != nil {
			return err
		}
		po := o.(*gen.PurchaseOrderHeaders).Response
		if _, err := h.ReleasePurchaseOrder(ctx, gen.ReleasePurchaseOrderParams{ID: po.ID, IfMatch: "*"}); err != nil {
			return err
		}
		_, err = h.CancelPurchaseOrder(ctx, gen.CancelPurchaseOrderParams{ID: po.ID, IfMatch: "*"})
		return err
	})
	if _, err := h.CancelPurchaseOrder(context.Background(), genCancel(id(draftPO))); err == nil {
		t.Error("cancel order outside a unit of work")
	}
	if _, _, err := (procurement.Orders{}).Order(context.Background(), uuid.New()); err == nil {
		t.Error("orders port outside a unit of work")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "orders port", func(ctx context.Context) error {
		_, _, err := procurement.Orders{}.Order(ctx, u(released))
		return err
	})

	// Handlers check page sizes for callers other than the generated router.
	zero := gen.NewOptInt32(0)
	for name, call := range map[string]func(ctx context.Context) error{
		"requisitions": func(ctx context.Context) error {
			_, err := h.ListRequisitions(ctx, gen.ListRequisitionsParams{PageSize: zero})
			return err
		},
		"orders": func(ctx context.Context) error {
			_, err := h.ListPurchaseOrders(ctx, gen.ListPurchaseOrdersParams{PageSize: zero})
			return err
		},
		"contracts": func(ctx context.Context) error {
			_, err := h.ListContracts(ctx, gen.ListContractsParams{PageSize: zero})
			return err
		},
		"receipts": func(ctx context.Context) error {
			_, err := h.ListGoodsReceipts(ctx, gen.ListGoodsReceiptsParams{PageSize: zero})
			return err
		},
	} {
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); err == nil {
			t.Errorf("%s: page_size 0 accepted", name)
		}
	}
	// A decision for a requisition that is not pending is refused.
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return procurement.Approvals{}.Approved(ctx, u(draftReq), uuid.New())
	}); err == nil || !strings.Contains(err.Error(), "procurement.invalid_status") {
		t.Errorf("decision on a draft: %v", err)
	}
	// Orders port.
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		o, ok, err := procurement.Orders{}.Order(ctx, u(released))
		if err != nil || !ok || o.Status != "released" || len(o.Items) != 1 || o.Items[0].Received != "2" || o.Items[0].Quantity != "10" {
			t.Errorf("order: %+v %v %v", o, ok, err)
		}
		if _, ok, err := (procurement.Orders{}).Order(ctx, uuid.New()); ok || err != nil {
			t.Errorf("unknown order: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// noApproval is a workflow that requires no approval.
type noApproval struct{ procurement.Opener }

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func TestEdges(t *testing.T) {
	e := setup(t)
	// A requisition the configuration needs no approval for is approved on submission.
	r := e.requisition(t, items(item(e.office, "1.5", "EA", "0.01", "EUR", "")), `, "needed_by": "2026-12-01"`)
	if r["total_amount"].(map[string]any)["amount"] != "0.02" {
		t.Fatalf("rounded total: %v", r)
	}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		h := endpoints()
		h.Approvals = noApproval{}
		got, err := h.SubmitRequisition(ctx, gen.SubmitRequisitionParams{ID: uuid.MustParse(id(r)), IfMatch: "*"})
		if err == nil && got.(*gen.UpdateRequisitionOKHeaders).Response.Status != "approved" {
			t.Errorf("submitted: %+v", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Custom fields merge recursively.
	o := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), `, "extensions": {"routing": {"desk": "north", "floor": 2}}`)
	upd, _ := e.Must(200, "PATCH", "/v1/purchase-orders/"+id(o), `{"extensions": {"routing": {"floor": null}}}`, `"1"`)
	if fmt.Sprint(upd["extensions"]) != "map[routing:map[desk:north]]" {
		t.Fatalf("merged: %v", upd["extensions"])
	}
	e.Problem(412, "request.version_mismatch", "PATCH", "/v1/purchase-orders/"+id(o), `{}`, `"1"`)
	// A contract can be attached to a draft order.
	c := e.contract(t, e.Supplier.String(), "")
	e.Problem(412, "request.version_mismatch", "POST", "/v1/contracts/"+id(c)+":release", "", `"7"`)
	e.act(t, "/v1/contracts/"+id(c), "release", "")
	with, _ := e.Must(200, "PATCH", "/v1/purchase-orders/"+id(o), fmt.Sprintf(`{"contract_id": %q}`, id(c)), `"2"`)
	if with["contract_id"] != id(c) {
		t.Fatalf("contract: %v", with)
	}
	// Contract items are checked like other items.
	bad := fmt.Sprint(`{"items": [{"material_key": "A", "description": "X", "unit_price": {"amount": "1.00", "currency": "USD"}, "uom_code": "EA", "account_id": "`, e.office, `"}]}`)
	d := e.contract(t, e.Supplier.String(), "")
	e.Problem(422, "procurement.currency_mismatch", "PATCH", "/v1/contracts/"+id(d), bad, `"1"`)
	e.Problem(422, "procurement.invalid_item_no", "PATCH", "/v1/contracts/"+id(d), strings.Replace(strings.Replace(bad, "USD", "EUR", 1), `"material_key"`, `"item_no": 4, "material_key"`, 1), `"1"`)
	if page, _ := e.Must(200, "GET", "/v1/contracts?page_size=1", "", ""); page["next_page_token"] == nil {
		t.Fatalf("contracts page: %v", page)
	} else if rest, _ := e.Must(200, "GET", "/v1/contracts?page_size=1&page_token="+page["next_page_token"].(string), "", ""); len(rest["items"].([]any)) != 1 {
		t.Fatalf("contracts page 2: %v", rest)
	}
	// A blocked legal entity takes no new documents.
	blocked := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Blocked', 'DE', 'EUR', gen_random_uuid(), 'blocked', $3, $3)`, e.Tenant.ID, blocked, e.Tenant.Human.ID)
	e.Problem(422, "procurement.legal_entity_not_active", "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`,
		blocked, items(item(e.office, "1", "EA", "1.00", "EUR", ""))), "")
	// Constraint translations.
	for name, code := range map[string]string{"purchase_order_one_per_requisition": "procurement.requisition_ordered",
		"contract_item_material_key": "procurement.duplicate_material_key", "requisition_item_uom_code_fkey": "procurement.unknown_unit",
		"goods_movement_item_tenant_id_purchase_order_id_po_item_no_fkey": "procurement.unknown_order_item"} {
		if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: name}); p.Code != code {
			t.Errorf("%s: %v", name, p)
		}
	}
}
