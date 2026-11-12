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
	shipped, _ := e.confirmed(t, e.item(line{quantity: "3"}), e.item(line{quantity: "2", unit: "KG"}))
	deliveredOrder := uuid.MustParse(shipped)
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
	calls := map[string]func(ctx context.Context) error{
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
	t.Run("list orders' page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListSalesOrders(ctx, gen.ListSalesOrdersParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	t.Run("list deliveries' page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListSalesDeliveries(ctx, gen.ListSalesDeliveriesParams{ID: deliveredOrder, PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	t.Run("list billing documents' page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListBillingDocuments(ctx, gen.ListBillingDocumentsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	t.Run("list page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListQuotations(ctx, gen.ListQuotationsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
