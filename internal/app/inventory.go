package app

import (
	"context"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Inventory is the inventory management context as the API wires it
// (ADR-0082 §1-§3): it reads the materials context's published API,
// tenancy's companies and org units, the ledger's cost centers and fiscal
// calendar and the configuration's number ranges, company dates and
// valuation classes through its ports, and posts the value of its
// movements and price changes through the ledger's posting port.
func Inventory(cfg Config) *inventory.Endpoints {
	return &inventory.Endpoints{Pages: cfg.Pages, Materials: materials.Materials{}, LegalEntities: tenancy.Directory{}, CostCenters: ledger.AssignmentBalances{},
		Numbers: configuration.Numbers{}, Calendar: calendar(), Settings: configuration.Settings{}, Postings: postings(), Accounts: ledger.Accounts{},
		Requests: opener(), People: LedgerPeople{}, Now: cfg.Now}
}

// salesReturns posts the sales context's return receipts' items of
// materials kept in stock into stock through inventory's published API
// (ADR-0083 As built S2b4), which sales does not import, in sales' own
// types: a 653 on the receipt's day, its reference the returns order's
// number, each item valued at what costs asks; each item's cost is what
// its line put into the stock with its price difference, the cost of
// goods sold the 653 took back.
type salesReturns struct{ inventory *inventory.Endpoints }

func (s salesReturns) ReceiveReturn(ctx context.Context, in sales.StockReturn, costs func(context.Context) ([]*money.Minor, error)) (sales.StockReturned,
	error) {
	items := make([]inventory.DeliveryItem, len(in.Items))
	for i, it := range in.Items {
		items[i] = inventory.DeliveryItem{MaterialID: it.MaterialID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, Quantity: it.Quantity,
			Unit: it.Unit, Batch: it.Batch, SerialNumbers: it.SerialNumbers}
	}
	d, err := s.inventory.ReceiveForReturn(ctx, in.Reference, in.Day, items, costs)
	if err != nil {
		return sales.StockReturned{}, err
	}
	out := sales.StockReturned{MaterialDocumentID: d.ID, MaterialDocumentNumber: d.Number, Currency: d.Currency, Costs: make([]money.Minor, len(items))}
	for _, l := range d.Lines {
		out.Costs[l.ItemNo-1] = l.Amount + l.Difference // a 653's item is one line, numbered from 1, valued at what it was asked: an amount
	}
	return out, nil
}
