package analytics

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// The sales lines (dataset sales_lines v1, T91a; ADR-0044 As built T91a):
// one fact per item of a confirmed sales order, dated the order's date,
// and per item of a posted billing document, dated its billing date, at
// the item's net amount in the document's currency. A draft order and a
// billing document not posted are not facts: order intake is what
// customers ordered for certain, billed value what was invoiced to them.

const salesLines = "sales_lines"

// The values of the sales_document dimension, which the metrics filter
// on: an order's item or a billing document's.
const (
	salesOrderLine   = "order"
	salesBillingLine = "billing"
)

// salesDocument is a sales order or a billing document as the API lists
// it: the fields both share, and each one's date.
type salesDocument struct {
	ID          string
	CustomerID  string `json:"customer_id"`
	OrderDate   string `json:"order_date"`
	BillingDate string `json:"billing_date"`
	Items       []struct {
		ItemKey string `json:"item_key"`
		Net     amount `json:"net_amount"`
	}
}

// addSales adds the legal entity's confirmed sales orders' items and its
// posted billing documents' items to sales_lines. The API's lists select
// the status, so a draft order or a billing document not posted is never
// read.
func (s Source) addSales(ctx context.Context, b *factBuilder, le legalEntity) error {
	if !s.builds(salesLines) {
		return nil
	}
	for _, list := range []struct {
		path, status, kind string
		date               func(salesDocument) string
	}{
		{"/v1/sales-orders", "confirmed", salesOrderLine, func(d salesDocument) string { return d.OrderDate }},
		{"/v1/billing-documents", "posted", salesBillingLine, func(d salesDocument) string { return d.BillingDate }},
	} {
		docs, err := apitools.List[salesDocument](ctx, s.API, list.path, url.Values{"legal_entity_id": {le.ID}, "status": {list.status}})
		if err != nil {
			return err
		}
		for _, d := range docs {
			if err := b.addSalesDocument(le, list.kind, list.date(d), d); err != nil {
				return err
			}
		}
	}
	return nil
}

// addSalesDocument adds one fact per item of the document, dated date.
func (b *factBuilder) addSalesDocument(le legalEntity, kind, date string, d salesDocument) error {
	c := b.vendors[d.CustomerID]
	for _, it := range d.Items {
		dims := map[string]string{"company": le.Code, "sales_document": kind, "currency": it.Net.Currency,
			"customer": c.Number, "customer_name": c.Name, "item_key": it.ItemKey}
		net, err := b.minor(it.Net)
		if err == nil {
			err = b.calendar(dims, "document_date", date)
		}
		if err != nil {
			return fmt.Errorf("sales %s %s: %w", kind, d.ID, err)
		}
		b.f.Tables[salesLines] = append(b.f.Tables[salesLines], Fact{ID: d.ID, Dims: dims, Amount: net, Currency: it.Net.Currency})
	}
	return nil
}
