package payables

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// TestOpenValue: what of an order item is not invoiced yet, at its price
// per price unit; credits beyond what is invoiced leave the item open as
// ordered, and invoicing beyond the order leaves nothing; quantities that
// are not decimals, or a value out of range, are errors.
func TestOpenValue(t *testing.T) {
	it := procurement.OrderItem{Quantity: "10", UnitPrice: 2500, PriceUnit: 2}
	for name, c := range map[string]struct {
		invoiced string
		want     money.Minor
	}{
		"nothing invoiced":   {"", 12500},
		"part invoiced":      {"3.5", 8125},
		"credited more":      {"-1", 12500},
		"invoiced in full":   {"10", 0},
		"invoiced more":      {"12", 0},
		"a sixth of an item": {"9.833333", 208},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := openValue(it, c.invoiced)
			if err != nil {
				t.Fatalf("error: got %v, want none", err)
			}
			if got != c.want {
				t.Errorf("open value: got %d, want %d", got, c.want)
			}
		})
	}
	for name, c := range map[string]struct {
		item     procurement.OrderItem
		invoiced string
	}{
		"an ordered quantity not a decimal":  {procurement.OrderItem{Quantity: "x", PriceUnit: 1}, ""},
		"an invoiced quantity not a decimal": {procurement.OrderItem{Quantity: "1", PriceUnit: 1}, "x"},
		"a value out of range":               {procurement.OrderItem{Quantity: "1000", UnitPrice: money.Minor(1) << 60, PriceUnit: 1}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := openValue(c.item, c.invoiced); err == nil {
				t.Error("error: got none, want one")
			}
		})
	}
}

// TestWithTax: the open value plus its tax, the value alone without a tax
// or when the sum is out of range.
func TestWithTax(t *testing.T) {
	large := money.Minor(1) << 62
	for name, c := range map[string]struct {
		net, charged money.Minor
		ok           bool
		want         money.Minor
	}{
		"with its tax":     {10000, 1900, true, 11900},
		"no tax":           {10000, 1900, false, 10000},
		"out of range":     {large, large, true, large},
		"a credit of both": {-100, -19, true, -119},
	} {
		t.Run(name, func(t *testing.T) {
			if got := withTax(c.net, c.charged, c.ok); got != c.want {
				t.Errorf("gross: got %d, want %d", got, c.want)
			}
		})
	}
}

// codes answers every tax code with one code, or with err.
type codes struct {
	Accounts
	code  ledger.TaxCode
	ok    bool
	err   error
	asked int
}

func (c *codes) TaxCode(context.Context, uuid.UUID, string, time.Time) (ledger.TaxCode, bool, error) {
	c.asked++
	return c.code, c.ok, c.err
}

// TestOpenOrderTax: an order's tax is calculated as an invoice's, each
// code asked for once per read; a code that is not an input code adds
// none; a failing ledger, or a document the calculator refuses, fails the
// read; an item whose quantity is not a decimal fails it before.
func TestOpenOrderTax(t *testing.T) {
	ctx, v1 := context.Background(), "V1"
	input := ledger.TaxCode{Code: "V1", Direction: config.TaxInput, Kind: "standard",
		Components: []ledger.TaxComponent{{Key: "vat", AccountKey: tax.KeyInput, RecoverablePct: "100", RatePct: "19"}}}
	v := Invoice{Currency: "EUR", Exponent: 2, Items: []Item{{Amount: 10000, TaxCode: &v1}, {Amount: 5000, TaxCode: &v1}}}
	reader := func(c *codes, rounding string) openReader {
		return openReader{e: &Endpoints{Accounts: &taxCodes{Accounts: c, codes: map[string]cachedCode{}}}, settings: config.TaxSettings{Rounding: rounding}}
	}
	c := &codes{code: input, ok: true}
	r := reader(c, "")
	for range 2 {
		if got, err := r.gross(ctx, v); err != nil || got != 17850 {
			t.Errorf("with 19%%: got %d, %v, want 17850", got, err)
		}
	}
	if c.asked != 1 {
		t.Errorf("the code asked for: got %d times, want once", c.asked)
	}
	if got, err := reader(&codes{}, "").gross(ctx, v); err != nil || got != 15000 {
		t.Errorf("a code that is no input code: got %d, %v, want 15000", got, err)
	}
	broken := errors.New("broken")
	if _, err := reader(&codes{err: broken}, "").gross(ctx, v); !errors.Is(err, broken) {
		t.Errorf("a failing ledger: got %v, want it wrapped", err)
	}
	if _, err := reader(&codes{code: input, ok: true}, "bankers").gross(ctx, v); !errors.Is(err, tax.ErrInvalid) {
		t.Errorf("a document the calculator refuses: got %v, want tax.ErrInvalid", err)
	}
	bad := procurement.Order{Items: []procurement.OrderItem{{ItemNo: 1, Quantity: "x", PriceUnit: 1}}}
	if _, err := r.order(ctx, bad, nil); err == nil {
		t.Error("an item whose quantity is not a decimal: got no error, want one")
	}
}
