package procurement

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// WEDocumentType is the journal document type of goods movement postings
// (SAP WE); it draws numbers from the journal_we number range.
const WEDocumentType = "WE"

// postMovement posts a goods movement's WE entry (T33): for each item of
// an order valued through GR/IR, Dr the order item's account / Cr GR/IR at
// the order price, which the ledger books as the item's GR/IR position
// moves (ledger.GRIRItem). The GR/IR line's quantity is the movement's
// negated: a receipt credits GR/IR, its reversal debits it. A movement
// without such items posts nothing (nil year and entry); one whose items
// are of orders in several currencies cannot share one entry.
func (e *Endpoints) postMovement(ctx context.Context, id uuid.UUID, number string, g GoodsReceipt, current []orderItem) (*int16, *uuid.UUID, error) {
	var currency string
	var items []ledger.GRIRItem
	for i, it := range g.Items {
		oi := current[i]
		if oi.valuation != ValuationGRIR {
			continue
		}
		if currency != "" && oi.currency != currency {
			return nil, nil, problem.Unprocessable("procurement.mixed_currencies",
				"Items of orders valued through GR/IR post one entry, so they are in one currency; receive orders in "+currency+" and "+oi.currency+" apart.")
		}
		currency = oi.currency
		items = append(items, ledger.GRIRItem{PurchaseOrderID: it.PurchaseOrderID, POItemNo: it.POItemNo, Quantity: negated(it.Quantity), Unit: it.Unit,
			UnitPrice: oi.unitPrice, PriceUnit: oi.priceUnit, OffsetAccountID: &current[i].account})
	}
	if len(items) == 0 {
		return nil, nil, nil
	}
	text := "Goods receipt " + number
	if g.MovementType == Reversal {
		text = "Reversal of a goods receipt, " + number
	}
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: g.LegalEntityID, DocumentType: WEDocumentType, DocumentDate: g.DocumentDate,
		PostingDate: g.PostingDate, Currency: currency, Reference: &number, HeaderText: &text, SourceContext: "procurement", SourceDocumentID: id,
		GRIR: items})
	if err != nil {
		return nil, nil, err
	}
	return &entry.FiscalYear, &entry.ID, nil
}

// negated flips the sign of a plain decimal quantity.
func negated(q string) string {
	if rest, ok := strings.CutPrefix(q, "-"); ok {
		return rest
	}
	return "-" + q
}
