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
// an order valued through GR/IR, Dr the order item's account (with its
// cost center, T56e) / Cr GR/IR at the item's valuation price, which the
// ledger books as the item's GR/IR position moves (ledger.GRIRItem); a
// stock item's (S1e) debit goes to its stock as its booking says (stock,
// by goods item, nil for another item). The GR/IR line's quantity is the
// movement's negated: a receipt (101) and a return's reversal (123) credit
// GR/IR, a receipt's reversal (102) and a return (122) debit it. A movement
// without such items posts nothing (ok false); one whose items are of
// orders in several currencies cannot share one entry. index is each goods
// item's position among the entry's GR/IR items, -1 for one it has not.
func (e *Endpoints) postMovement(ctx context.Context, id uuid.UUID, number string, g GoodsReceipt, current []orderItem, stock []*ledger.GRIRStock) (
	entry ledger.Entry, index []int, ok bool, err error) {
	var currency string
	var items []ledger.GRIRItem
	index = make([]int, len(g.Items))
	for i, it := range g.Items {
		index[i] = -1
		oi := current[i]
		if oi.valuation != ValuationGRIR {
			continue
		}
		if currency != "" && oi.currency != currency {
			return ledger.Entry{}, nil, false, problem.Unprocessable("procurement.mixed_currencies",
				"Items of orders valued through GR/IR post one entry, so they are in one currency; receive orders in "+currency+" and "+oi.currency+" apart.")
		}
		currency = oi.currency
		item := ledger.GRIRItem{PurchaseOrderID: it.PurchaseOrderID, POItemNo: it.POItemNo, Quantity: negated(it.Quantity), Unit: it.Unit,
			UnitPrice: oi.valuationPrice, PriceUnit: oi.valuationPriceUnit, OffsetAccountID: &current[i].account, CostCenterID: oi.costCenter}
		if stock != nil && stock[i] != nil {
			item.OffsetAccountID, item.Stock = &stock[i].AccountID, stock[i]
		}
		index[i] = len(items)
		items = append(items, item)
	}
	if len(items) == 0 {
		return ledger.Entry{}, index, false, nil
	}
	text := movementTexts[g.MovementType] + number
	entry, err = e.Postings.Post(ctx, ledger.Posting{LegalEntityID: g.LegalEntityID, DocumentType: WEDocumentType, DocumentDate: g.DocumentDate,
		PostingDate: g.PostingDate, Currency: currency, Reference: &number, HeaderText: &text, SourceContext: "procurement", SourceDocumentID: id,
		GRIR: items})
	if err != nil {
		return ledger.Entry{}, nil, false, err
	}
	return entry, index, true, nil
}

// movementTexts start the header text of each movement type's WE entry.
var movementTexts = map[string]string{Receipt: "Goods receipt ", Reversal: "Reversal of a goods receipt, ", Return: "Return to the supplier ",
	ReturnReversal: "Reversal of a return to the supplier, "}

// negated flips the sign of a plain decimal quantity, as the database
// gives it (trim_scale): a return's items and every reversal's, and the
// GR/IR side of a receipt. Quantities are never zero.
func negated(q string) string {
	if rest, ok := strings.CutPrefix(q, "-"); ok {
		return rest
	}
	return "-" + q
}
