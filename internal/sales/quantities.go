package sales

import (
	"fmt"
	"math/big"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// ItemQuantity is a quantity of one item of a document as a client sends
// it: the item's number in that document and a plain decimal string in
// the item's unit. A delivery names a sales order's item, a returns
// order a billing document's, a return receipt a returns order's.
type ItemQuantity struct {
	ItemNo         int32
	Quantity, Unit string
}

// taking is how the quantities a request takes of a document's items are
// checked (deliveries, returns orders, return receipts): each names an
// item, once, with a quantity more than zero in the item's unit that is
// at most what is open of it. The document was read under the row lock
// that serializes the requests taking of it (ADR-0008). Every refusal is
// a 422 naming the request's own field.
type taking struct {
	// document is what takes the quantities, as a refusal names it: "a
	// delivery".
	document string
	// noField is the request's field that names the item, under items[i].
	noField string
	// find gives the item of the number: its unit and what is open of it.
	find func(no int32) (unit string, open *big.Rat, ok bool)
	// otherUnit words the refusal of a quantity in another unit than the
	// item's.
	otherUnit func(no int32, unit string) string
}

// untaken is the first quantity of a request that its document cannot
// take for a reason of the document's own, which the caller refuses with
// its own code: it names an item the document does not have (open nil),
// or it is more than what is open of the item.
type untaken struct {
	// noField and quantityField are the request's fields that name the
	// item and its quantity.
	noField, quantityField string
	no                     int32
	open                   *big.Rat
	quantity               string
}

// check refuses the first quantity that cannot be taken whatever the
// document: none at all (422 sales.items_required), an item named twice
// (sales.item_repeated), a quantity that is no plain decimal above zero
// (sales.invalid_quantity), another unit than the item's
// (sales.unit_mismatch). One the document cannot take is returned for
// the caller to refuse (untaken); nil, nil: every quantity can be taken.
func (t taking) check(in []ItemQuantity) (*untaken, error) {
	if len(in) == 0 {
		return nil, problem.Invalid("sales.items_required", "items", capital(t.document)+" has at least one item.")
	}
	named := make(map[int32]bool, len(in))
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		short := untaken{noField: field + "." + t.noField, quantityField: field + ".quantity.value", no: it.ItemNo, quantity: it.Quantity}
		unit, open, ok := t.find(it.ItemNo)
		switch {
		case !ok:
			return &short, nil
		case named[it.ItemNo]:
			return nil, problem.Invalid("sales.item_repeated", short.noField,
				fmt.Sprintf("Item %d is named more than once; %s names an item once.", it.ItemNo, t.document))
		case money.PositiveQuantity(it.Quantity) != nil:
			return nil, problem.Invalid("sales.invalid_quantity", short.quantityField, fmt.Sprintf(
				"A quantity is a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits, money.QuantityDecimals))
		case it.Unit != unit:
			return nil, problem.Invalid("sales.unit_mismatch", field+".quantity.unit", t.otherUnit(it.ItemNo, unit))
		}
		named[it.ItemNo] = true
		if money.ParseRat(it.Quantity).Cmp(open) > 0 {
			short.open = open
			return &short, nil
		}
	}
	return nil, nil
}

// capital is the text with its first letter, an ASCII one, in upper
// case: how a refusal begins with what it names.
func capital(s string) string {
	return string(s[0]-'a'+'A') + s[1:]
}
