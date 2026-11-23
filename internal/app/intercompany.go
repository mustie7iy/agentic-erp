package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// salesIntercompany answers the sales context's question to procurement,
// which sales does not import (ADR-0082 As built S2i, S2i2): the
// intercompany stock transport order whose goods issue the supplying
// company bills, read under its share lock, in sales' own type.
type salesIntercompany struct{}

func (salesIntercompany) IssuedOrder(ctx context.Context, id uuid.UUID) (sales.IssuedOrder, error) {
	o, err := procurement.Orders{}.Issued(ctx, id)
	if err != nil {
		return sales.IssuedOrder{}, err
	}
	out := sales.IssuedOrder{ID: o.ID, DocumentNumber: o.DocumentNumber, LegalEntityID: o.LegalEntityID, SupplyingPlantID: o.SupplyingPlantID,
		Currency: o.Currency, Exponent: o.Exponent, IssueDocumentID: o.IssueDocumentID, IssueDocumentNumber: o.IssueDocumentNumber, IssueDate: o.IssueDate,
		Items: make([]sales.IntercompanyOrderItem, len(o.Items))}
	for i, it := range o.Items {
		out.Items[i] = sales.IntercompanyOrderItem{No: it.No, MaterialID: it.MaterialID, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
			UnitPrice: it.UnitPrice, PriceUnit: it.PriceUnit, Net: it.Net}
	}
	return out, nil
}

// payablesIntercompany answers the payables context's question to sales,
// which payables does not import (ADR-0082 As built S2i3): the posted
// intercompany billing the receiving company enters as its supplier
// invoice, one the caller may read, in payables' own type.
type payablesIntercompany struct{}

func (payablesIntercompany) IntercompanyBilling(ctx context.Context, id uuid.UUID) (payables.IntercompanyBilling, error) {
	b, err := sales.GetIntercompany(ctx, id)
	if err != nil {
		return payables.IntercompanyBilling{}, err
	}
	out := payables.IntercompanyBilling{ID: b.ID, DocumentNumber: b.DocumentNumber, LegalEntityID: b.LegalEntityID, ReceivingID: b.ReceivingID,
		OrderID: b.OrderID, OrderNumber: b.OrderNumber, BillingDate: b.BillingDate, Currency: b.Currency, Exponent: b.Exponent, Net: b.Net, Tax: b.Tax,
		Items: make([]payables.IntercompanyBillingItem, len(b.Items))}
	for i, it := range b.Items {
		out.Items[i] = payables.IntercompanyBillingItem{POItemNo: it.POItemNo, Description: it.Description, Quantity: it.Quantity, Net: it.Net,
			TaxCode: it.TaxCode}
	}
	return out, nil
}
