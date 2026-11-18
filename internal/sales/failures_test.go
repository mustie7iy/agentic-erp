package sales_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t, time.Now())
	id := uuid.MustParse(e.quoted(t))
	accepted, rejected := uuid.MustParse(e.quoted(t)), uuid.MustParse(e.quoted(t))
	create := &gen.QuotationCreate{LegalEntityID: e.DE, CustomerID: e.customer, ValidUntil: instant(t, far+"T00:00:00Z"), Currency: "EUR",
		Items: []gen.SalesItemCreate{
			{ItemKey: gen.NewOptString("BRACKET-M8"), Description: "Bracket", Quantity: gen.Quantity{Value: "2", Unit: "EA"},
				UnitPrice: gen.NewOptMoney(gen.Money{Amount: "10.00", Currency: "EUR"}), AccountID: e.revenue, TaxCode: gen.NewOptString("A1")},
			{Description: "Freight", Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.NewOptMoney(gen.Money{Amount: "5.00", Currency: "EUR"}),
				PriceUnit: gen.NewOptInt32(1), AccountID: e.other, TaxCode: gen.NewOptString("A1")}}}
	quotation := uuid.MustParse(e.accepted(t, e.customer, e.item(line{code: "A1"})))
	order, draft := uuid.MustParse(e.ordered(t)), uuid.MustParse(e.ordered(t))
	e.Must(200, "POST", changing(order.String()), changeBody("", itemChange("1 2 EA", "")), `"1"`)
	shipped, _ := e.confirmed(t, e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG"}))
	deliveredOrder := uuid.MustParse(shipped)
	twoItems, _ := e.confirmed(t, e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG"}))
	reshaped := uuid.MustParse(twoItems)
	e.Must(201, "POST", deliveries(shipped), `{"delivery_date": "2026-01-02", "items": [{"item_no": 1, "quantity": {"value": "1", "unit": "EA"}}]}`, "")
	// An order with a billing document for its first delivery and a second delivery still to bill, one item with a tax code.
	toBill, today := e.deliveredOrder(t, []string{e.item(line{quantity: "3", code: "A1"}), e.item(line{quantity: "2", account: e.other})}, "1 1 EA")
	billedOrder := uuid.MustParse(toBill)
	created, _ := e.Must(201, "POST", billings, bill(toBill, ""), "")
	billing := uuid.MustParse(created["id"].(string))
	e.Must(201, "POST", deliveries(toBill), delivered(today, "1 1 EA", "2 2 EA"), "")
	// A billing document still to post, one item with a tax code.
	_, toPost, _ := e.billedOrder(t, []string{e.item(line{quantity: "2", code: "A1"}), e.item(line{account: e.other})}, "1 2 EA", "2 1 EA")
	postable := uuid.MustParse(toPost)
	// The customer has a limit and confirmed orders, so a confirmation reads its limit, its orders and its open items;
	// another customer's order is blocked, and one is in US dollars at a recorded rate.
	e.limited(t, e.customer, "100000.00")
	blocked := uuid.MustParse(e.placed(t, e.buyer(t, "BP70000090", "0.00"), e.item(line{code: "A1"}))["id"].(string))
	e.usd(t)
	dollars := uuid.MustParse(e.drafted(t, e.customer, "USD", e.item(line{currency: "USD", code: "A1"})))
	// A posted billing document with a returns order of one of its two items, a draft, and room for another.
	_, returnedFrom, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG"})}, "1 3 EA", "2 2 KG")
	billedDoc := uuid.MustParse(returnedFrom)
	draftReturn, _ := e.Must(201, "POST", returnsOrders, returning(returnedFrom, "defective", "1 1 EA"), "")
	returns := uuid.MustParse(draftReturn["id"].(string))
	// A confirmed returns order of the document's second item with one receipt and more still expected.
	receivedReturn := uuid.MustParse(e.returnedOrder(t, returnedFrom, "2 1.5 KG"))
	e.Must(201, "POST", receipts(receivedReturn.String()), `{"receipt_date": "2026-01-02", "items": [{"item_no": 1, "quantity": {"value": "0.5", "unit": "KG"}}]}`, "")
	// A credit memo request for what the returns order received, and one by amount to decide.
	_, creditedFrom, _ := e.postedOrder(t, []string{e.item(line{quantity: "3"})}, "1 3 EA")
	creditDoc := uuid.MustParse(creditedFrom)
	creditFor := uuid.MustParse(e.returnedOrder(t, creditedFrom, "1 2 EA"))
	e.Must(201, "POST", receipts(creditFor.String()), `{"receipt_date": "2026-01-02", "items": [{"item_no": 1, "quantity": {"value": "1", "unit": "EA"}}]}`, "")
	requested, _ := e.Must(201, "POST", creditRequests, byAmount(creditedFrom, "Goodwill", "1 0.10"), "")
	request := uuid.MustParse(requested["id"].(string))
	toPostCredit := uuid.MustParse(e.approvedRequest(t, byAmount(creditedFrom, "Goodwill", "1 0.20"))["id"].(string))
	// A posted billing document nothing names, to cancel.
	_, cancellable, _ := e.postedOrder(t, []string{e.item(line{quantity: "2", code: "A1"})}, "1 2 EA")
	toCancel := uuid.MustParse(cancellable)
	calls := map[string]func(ctx context.Context) error{
		"cancel a posted billing document": func(ctx context.Context) error {
			_, err := h.CancelBillingDocument(ctx, &gen.BillingDocumentCancel{Reason: "Wrong"}, gen.CancelBillingDocumentParams{ID: toCancel, IfMatch: `"2"`})
			return err
		},
		"cancel a created billing document": func(ctx context.Context) error {
			_, err := h.CancelBillingDocument(ctx, &gen.BillingDocumentCancel{Reason: "Wrong"}, gen.CancelBillingDocumentParams{ID: postable, IfMatch: `"1"`})
			return err
		},
		"request a credit for a return": func(ctx context.Context) error {
			_, err := h.CreateCreditMemoRequest(ctx, &gen.CreditMemoRequestCreate{BillingDocumentID: creditDoc, ReturnsOrderID: gen.NewOptUUID(creditFor),
				Reason: gen.NewOptString("Returned")}, gen.CreateCreditMemoRequestParams{})
			return err
		},
		"request a credit by amount": func(ctx context.Context) error {
			_, err := h.CreateCreditMemoRequest(ctx, &gen.CreditMemoRequestCreate{BillingDocumentID: creditDoc, Reason: gen.NewOptString("Goodwill"),
				Items: []gen.CreditMemoRequestItemCreate{{BillingItemNo: 1, NetAmount: gen.Money{Amount: "1.00", Currency: "EUR"}}}}, gen.CreateCreditMemoRequestParams{})
			return err
		},
		"post a credit memo request": func(ctx context.Context) error {
			_, err := h.PostCreditMemoRequest(ctx, gen.OptCreditMemoRequestPost{}, gen.PostCreditMemoRequestParams{ID: toPostCredit, IfMatch: `"2"`})
			return err
		},
		"get a credit memo request": func(ctx context.Context) error {
			_, err := h.GetCreditMemoRequest(ctx, gen.GetCreditMemoRequestParams{ID: request})
			return err
		},
		"list credit memo requests": func(ctx context.Context) error {
			_, err := h.ListCreditMemoRequests(ctx, gen.ListCreditMemoRequestsParams{})
			return err
		},
		"approve a credit memo request": func(ctx context.Context) error {
			_, err := h.ReleaseCreditMemoRequest(ctx, gen.ReleaseCreditMemoRequestParams{ID: request, IfMatch: `"1"`})
			return err
		},
		"reject a credit memo request": func(ctx context.Context) error {
			_, err := h.DeclineCreditMemoRequest(ctx, &gen.CreditMemoRequestReject{Reason: "No"}, gen.DeclineCreditMemoRequestParams{ID: request, IfMatch: `"1"`})
			return err
		},
		"confirm a return receipt": func(ctx context.Context) error {
			_, err := h.ConfirmReturnReceipt(ctx, &gen.ReturnReceiptCreate{ReceiptDate: instant(t, "2026-01-03T00:00:00Z"), Items: []gen.ReturnReceiptItemCreate{
				{ItemNo: 1, Quantity: gen.Quantity{Value: "1", Unit: "KG"}}}}, gen.ConfirmReturnReceiptParams{ID: receivedReturn})
			return err
		},
		"list return receipts": func(ctx context.Context) error {
			_, err := h.ListReturnReceipts(ctx, gen.ListReturnReceiptsParams{ID: receivedReturn})
			return err
		},
		"create a returns order": func(ctx context.Context) error {
			_, err := h.CreateReturnsOrder(ctx, &gen.ReturnsOrderCreate{BillingDocumentID: billedDoc, Reason: "other", ReasonText: gen.NewOptString("Late"),
				Items: []gen.ReturnsOrderItemCreate{{BillingItemNo: 1, Quantity: gen.Quantity{Value: "2", Unit: "EA"}},
					{BillingItemNo: 2, Quantity: gen.Quantity{Value: "0.5", Unit: "KG"}}}}, gen.CreateReturnsOrderParams{})
			return err
		},
		"get a returns order": func(ctx context.Context) error {
			_, err := h.GetReturnsOrder(ctx, gen.GetReturnsOrderParams{ID: returns})
			return err
		},
		"list returns orders": func(ctx context.Context) error {
			_, err := h.ListReturnsOrders(ctx, gen.ListReturnsOrdersParams{})
			return err
		},
		"confirm a returns order": func(ctx context.Context) error {
			_, err := h.ConfirmReturnsOrder(ctx, gen.ConfirmReturnsOrderParams{ID: returns, IfMatch: `"1"`})
			return err
		},
		"change an order, its increase checked for credit": func(ctx context.Context) error {
			_, err := h.ChangeSalesOrder(ctx, &gen.SalesOrderChangeRequest{Reason: gen.NewOptString("More"), Items: []gen.SalesOrderItemChange{
				{ItemNo: 2, Quantity: gen.NewOptQuantity(gen.Quantity{Value: "3", Unit: "KG"})}}}, gen.ChangeSalesOrderParams{ID: deliveredOrder, IfMatch: `"3"`})
			return err
		},
		"add and remove an order's items and change its payment term": func(ctx context.Context) error {
			_, err := h.ChangeSalesOrder(ctx, &gen.SalesOrderChangeRequest{PaymentTermCode: gen.NewOptString("NT14"), RemovedItems: []int32{2},
				NewItems: []gen.SalesItemCreate{{Description: "Freight", Quantity: gen.Quantity{Value: "1", Unit: "EA"},
					UnitPrice: gen.NewOptMoney(gen.Money{Amount: "5.00", Currency: "EUR"}), AccountID: e.revenue}}},
				gen.ChangeSalesOrderParams{ID: reshaped, IfMatch: `"2"`})
			return err
		},
		"cancel an order": func(ctx context.Context) error {
			_, err := h.CancelSalesOrder(ctx, &gen.ReasonInput{Reason: "Withdrawn"}, gen.CancelSalesOrderParams{ID: draft, IfMatch: `"1"`})
			return err
		},
		"list an order's changes": func(ctx context.Context) error {
			_, err := h.ListSalesOrderChanges(ctx, gen.ListSalesOrderChangesParams{ID: order})
			return err
		},
		"release a credit block": func(ctx context.Context) error {
			_, err := h.ReleaseSalesOrderCreditBlock(ctx, gen.ReleaseSalesOrderCreditBlockParams{ID: blocked, IfMatch: `"2"`})
			return err
		},
		"confirm in another currency": func(ctx context.Context) error {
			_, err := h.ConfirmSalesOrder(ctx, gen.ConfirmSalesOrderParams{ID: dollars, IfMatch: `"1"`})
			return err
		},
		"open orders": func(ctx context.Context) error {
			_, _, err := h.OpenOrders(ctx, e.DE, e.customer)
			return err
		},
		"confirm a delivery": func(ctx context.Context) error {
			_, err := h.ConfirmSalesDelivery(ctx, &gen.SalesDeliveryCreate{DeliveryDate: instant(t, "2026-01-03T00:00:00Z"), Items: []gen.SalesDeliveryItemCreate{
				{ItemNo: 1, Quantity: gen.Quantity{Value: "2", Unit: "EA"}}, {ItemNo: 2, Quantity: gen.Quantity{Value: "0.5", Unit: "KG"}}}},
				gen.ConfirmSalesDeliveryParams{ID: deliveredOrder})
			return err
		},
		"create a billing document": func(ctx context.Context) error {
			_, err := h.CreateBillingDocument(ctx, &gen.BillingDocumentCreate{SalesOrderID: billedOrder}, gen.CreateBillingDocumentParams{})
			return err
		},
		"post a billing document": func(ctx context.Context) error {
			_, err := h.PostBillingDocument(ctx, gen.OptBillingDocumentPost{}, gen.PostBillingDocumentParams{ID: postable, IfMatch: `"1"`})
			return err
		},
		"get a billing document": func(ctx context.Context) error {
			_, err := h.GetBillingDocument(ctx, gen.GetBillingDocumentParams{ID: billing})
			return err
		},
		"list billing documents": func(ctx context.Context) error {
			_, err := h.ListBillingDocuments(ctx, gen.ListBillingDocumentsParams{})
			return err
		},
		"list deliveries": func(ctx context.Context) error {
			_, err := h.ListSalesDeliveries(ctx, gen.ListSalesDeliveriesParams{ID: deliveredOrder})
			return err
		},
		"order from a quotation": func(ctx context.Context) error {
			_, err := h.CreateSalesOrder(ctx, &gen.SalesOrderCreate{QuotationID: gen.NewOptUUID(quotation)}, gen.CreateSalesOrderParams{})
			return err
		},
		"order directly": func(ctx context.Context) error {
			_, err := h.CreateSalesOrder(ctx, &gen.SalesOrderCreate{LegalEntityID: gen.NewOptUUID(e.DE), CustomerID: gen.NewOptUUID(e.customer),
				Currency: gen.NewOptString("EUR"), PaymentTermCode: gen.NewOptString("NT14"), Items: create.Items}, gen.CreateSalesOrderParams{})
			return err
		},
		"get an order": func(ctx context.Context) error {
			_, err := h.GetSalesOrder(ctx, gen.GetSalesOrderParams{ID: order})
			return err
		},
		"list orders": func(ctx context.Context) error {
			_, err := h.ListSalesOrders(ctx, gen.ListSalesOrdersParams{})
			return err
		},
		"confirm": func(ctx context.Context) error {
			_, err := h.ConfirmSalesOrder(ctx, gen.ConfirmSalesOrderParams{ID: draft, IfMatch: `"1"`})
			return err
		},
		"create": func(ctx context.Context) error {
			_, err := h.CreateQuotation(ctx, create, gen.CreateQuotationParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetQuotation(ctx, gen.GetQuotationParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListQuotations(ctx, gen.ListQuotationsParams{})
			return err
		},
		"accept": func(ctx context.Context) error {
			_, err := h.AcceptQuotation(ctx, gen.AcceptQuotationParams{ID: accepted, IfMatch: `"1"`})
			return err
		},
		"decline": func(ctx context.Context) error {
			_, err := h.DeclineQuotation(ctx, gen.DeclineQuotationParams{ID: rejected, IfMatch: `"1"`})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	// A page size of 0 is refused by every list.
	lists := map[string]func(ctx context.Context) error{
		"orders": func(ctx context.Context) error {
			_, err := h.ListSalesOrders(ctx, gen.ListSalesOrdersParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"order changes": func(ctx context.Context) error {
			_, err := h.ListSalesOrderChanges(ctx, gen.ListSalesOrderChangesParams{ID: order, PageSize: gen.NewOptInt32(0)})
			return err
		},
		"deliveries": func(ctx context.Context) error {
			_, err := h.ListSalesDeliveries(ctx, gen.ListSalesDeliveriesParams{ID: deliveredOrder, PageSize: gen.NewOptInt32(0)})
			return err
		},
		"return receipts": func(ctx context.Context) error {
			_, err := h.ListReturnReceipts(ctx, gen.ListReturnReceiptsParams{ID: receivedReturn, PageSize: gen.NewOptInt32(0)})
			return err
		},
		"credit memo requests": func(ctx context.Context) error {
			_, err := h.ListCreditMemoRequests(ctx, gen.ListCreditMemoRequestsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"returns orders": func(ctx context.Context) error {
			_, err := h.ListReturnsOrders(ctx, gen.ListReturnsOrdersParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"billing documents": func(ctx context.Context) error {
			_, err := h.ListBillingDocuments(ctx, gen.ListBillingDocumentsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"quotations": func(ctx context.Context) error {
			_, err := h.ListQuotations(ctx, gen.ListQuotationsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
	}
	for name, list := range lists {
		t.Run(name+" page size", func(t *testing.T) {
			if p := problem.From(e.inRequest(t, list)); p.Code != "request.invalid_page_size" {
				t.Errorf("got %v, want request.invalid_page_size", p)
			}
		})
	}
}
