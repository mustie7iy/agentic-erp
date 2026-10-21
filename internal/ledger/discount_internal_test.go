package ledger

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestSettlementDiscountsOutOfRange: discounts each in range whose total
// is not, or whose payment is not, are refused rather than wrapped.
func TestSettlementDiscountsOutOfRange(t *testing.T) {
	const near = money.Minor(6_000_000_000_000_000_000)
	items := []item{{tc: -near}, {tc: -near}, {tc: 9_000_000_000_000_000_000}}
	for name, c := range map[string]struct {
		discounts []money.Minor
		tc        money.Minor
	}{
		"discounts adding up beyond an amount": {[]money.Minor{near - 1, near - 1, -1}, -3_000_000_000_000_000_000},
		"a payment beyond an amount":           {[]money.Minor{1, 1, -8_999_999_999_999_999_999}, -9_000_000_000_000_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := settlementDiscount(Settlement{Discounts: c.discounts}, items, c.tc, 0)
			if p := problem.From(err); p == nil || p.Code != "ledger.amount_too_large" {
				t.Errorf("got %v, want ledger.amount_too_large", err)
			}
		})
	}
	_, _, err := settlementDiscount(Settlement{Discounts: []money.Minor{math.MinInt64}}, []item{{tc: 5}}, 5, 0)
	if p := problem.From(err); p == nil || p.Code != "ledger.invalid_discount" {
		t.Errorf("a discount of the smallest amount: got %v, want ledger.invalid_discount", err)
	}
}

// taxedItems is an entry's tax items for the tests: one per key, with
// its tax and no base, component "tax".
func taxedItems(taxes map[taxKey]money.Minor) []sourceItem {
	var out []sourceItem
	n := int32(0)
	for k, t := range taxes {
		n++
		out = append(out, sourceItem{no: n, component: "tax", key: k, tax: t})
	}
	return out
}

// TestSplitTax: an entry owing nothing, or whose tax runs against its
// payment, is not split; tax shares adding up beyond an amount are refused.
func TestSplitTax(t *testing.T) {
	v1 := taxKey{code: "V1"}
	for name, c := range map[string]taxedItem{
		"owing nothing":           {discount: 100, paid: 0, items: taxedItems(map[taxKey]money.Minor{v1: 19})},
		"tax against the payment": {discount: 100, paid: 119, items: taxedItems(map[taxKey]money.Minor{v1: -19})},
		"no tax items":            {discount: 100, paid: 119},
		"tax beyond what is owed": {discount: 100, paid: 1400, items: taxedItems(map[taxKey]money.Minor{v1: 1900})},
		"tax all that is owed":    {discount: 100, paid: 1900, items: taxedItems(map[taxKey]money.Minor{v1: 1900})},
	} {
		t.Run(name, func(t *testing.T) {
			split, err := splitTax([]taxedItem{c})
			if err != nil || split.tax != 0 || len(split.taxes) != 0 || len(split.shares) != 0 {
				t.Errorf("got %+v %v, want no tax share", split, err)
			}
		})
	}
	const near = money.Minor(9_000_000_000_000_000_000)
	huge := taxedItem{discount: near, paid: near + 1, items: taxedItems(map[taxKey]money.Minor{v1: near})}
	if _, err := splitTax([]taxedItem{huge, huge}); err == nil {
		t.Error("tax shares beyond an amount: got none, want an error")
	}
	based := taxedItem{discount: near - 1, paid: near, items: []sourceItem{{no: 1, component: "tax", key: v1, base: near - 2, tax: 1}}}
	if _, err := splitTax([]taxedItem{based, based}); err == nil {
		t.Error("base shares beyond an amount: got none, want an error")
	}
}

// TestItemTaxShares: a credit memo's discount gives back tax of its sign;
// tax items on different accounts each take their share, largest
// remainder first; the net is shared among the codes' bases and the
// untaxed rest, once per code across its components.
func TestItemTaxShares(t *testing.T) {
	a, b := taxKey{account: uuid.UUID{1}, code: "V1"}, taxKey{account: uuid.UUID{2}, code: "V1"}
	g, q := taxKey{account: uuid.UUID{3}, code: "I5"}, taxKey{account: uuid.UUID{4}, code: "I5"}
	render := func(shares []itemShare) string {
		out := ""
		for _, sh := range shares {
			out += fmt.Sprintf(" %s/%s:%d/%d/%v", sh.src.key.code, sh.src.component, sh.tax, sh.base, sh.codeBase)
		}
		return out
	}
	for name, c := range map[string]struct {
		it   taxedItem
		want string
	}{
		"a credit memo": {taxedItem{discount: -238, paid: -11900, items: []sourceItem{{no: 1, component: "tax", key: a, base: -10000, tax: -1900}}},
			" V1/tax:-38/-200/true"},
		"two accounts, remainder": {taxedItem{discount: 500, paid: 10012, items: []sourceItem{{no: 1, component: "tax", key: b, tax: 5},
			{no: 2, component: "tax", key: a, tax: 7}}}, " V1/tax:1/0/true V1/tax:0/0/false"},
		"two components of one code, an untaxed rest": {taxedItem{discount: 300, paid: 15000, items: []sourceItem{
			{no: 1, component: "gst", key: g, base: 10000, tax: 500}, {no: 2, component: "qst", key: q, base: 10000, tax: 1000}}},
			" I5/gst:10/200/true I5/qst:20/200/false"},
		"a base against the payment": {taxedItem{discount: 100, paid: 1000, items: []sourceItem{{no: 1, component: "tax", key: a, base: -500, tax: 100}}},
			" V1/tax:10/0/true"},
	} {
		t.Run(name, func(t *testing.T) {
			shares, err := itemTaxShares(c.it)
			if got := render(shares); err != nil || got != c.want {
				t.Errorf("shares: got %q %v, want %q", got, err, c.want)
			}
		})
	}
}
