package procurement_test

import (
	"fmt"
	"testing"
)

const largest = "92233720368547758.07" // 2^63-1 cents

// A document whose extended amounts or total are out of the bigint range is
// refused. Before, the total wrapped: two items at the largest price came to
// -0.02 EUR, which selected the lowest approval tier and would have passed
// any contract's remaining value.
func TestTotalOutOfRange(t *testing.T) {
	e := setup(t)
	cases := map[string]string{
		"extended amount": items(item(e.office, "2", "EA", largest, "EUR", "")),
		"total":           items(item(e.office, "1", "EA", largest, "EUR", ""), item(e.office, "1", "EA", largest, "EUR", "")),
	}
	for name, its := range cases {
		t.Run(name, func(t *testing.T) {
			api := e.Client
			api.T = t
			api.Problem(422, "procurement.amount_too_large", "POST", "/v1/requisitions",
				fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, its), "")
			api.Problem(422, "procurement.amount_too_large", "POST", "/v1/purchase-orders",
				fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10", "items": %s}`, e.DE, e.Supplier, its), "")
		})
	}
	// The largest total itself is fine.
	e.requisition(t, items(item(e.office, "1", "EA", largest, "EUR", "")), "")
}

// An order that exceeds what remains of a contract is refused even when the
// consumed value plus the order is out of range: before, the sum wrapped
// negative and the order passed.
func TestContractRemainderOutOfRange(t *testing.T) {
	e := setup(t)
	c, _ := e.Must(201, "POST", "/v1/contracts", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "valid_from": "2026-01-01",
		"valid_to": "2026-12-31", "target_value": {"amount": %q, "currency": "EUR"}, "items": [
		{"material_key": "A", "description": "Paper", "unit_price": {"amount": "1.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q},
		{"material_key": "B", "description": "Plant", "unit_price": {"amount": %q, "currency": "EUR"}, "uom_code": "EA", "account_id": %q}]}`,
		e.DE, e.Supplier, largest, e.office, largest, e.office), "")
	e.act(t, "/v1/contracts/"+id(c), "release", "")
	contract := fmt.Sprintf(`, "contract_id": %q`, id(c))
	small := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", `, "contract_item_no": 1`)), contract)
	e.act(t, "/v1/purchase-orders/"+id(small), "release", "")
	whole := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", `, "contract_item_no": 2`)), contract)
	e.Problem(409, "procurement.contract_exceeded", "POST", "/v1/purchase-orders/"+id(whole)+":release", "", "*")
}
