package payables

import (
	"context"
	"errors"
	"fmt"
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

// TestOpenOrderTax: an order's tax is calculated per code as an
// invoice's, each code asked for once per read; a code that is not an
// input code adds no tax and leaves the other codes' alone; a failing
// ledger, or a document the calculator refuses, fails the read.
func TestOpenOrderTax(t *testing.T) {
	ctx, v1, q9 := context.Background(), "V1", "Q9"
	input := ledger.TaxCode{Code: "V1", Direction: config.TaxInput, Kind: "standard",
		Components: []ledger.TaxComponent{{Key: "vat", AccountKey: tax.KeyInput, RecoverablePct: "100", RatePct: "19"}}}
	v := Invoice{Currency: "EUR", Exponent: 2, Items: []Item{{Amount: 10000, TaxCode: &v1}, {Amount: 5000, TaxCode: &v1}}}
	reader := func(c *codes, rounding string) openReader {
		return openReader{e: &Endpoints{Accounts: &taxCodes{Accounts: c, codes: map[string]cachedCode{}}}, settings: config.TaxSettings{Rounding: rounding}}
	}
	t.Run("each code asked for once", func(t *testing.T) {
		c := &codes{code: input, ok: true}
		r := reader(c, "")
		for range 2 {
			got, err := r.gross(ctx, v)
			if err != nil {
				t.Fatalf("error: got %v, want none", err)
			}
			if got != 17850 {
				t.Errorf("with 19%%: got %d, want 17850", got)
			}
		}
		if c.asked != 1 {
			t.Errorf("the code asked for: got %d times, want once", c.asked)
		}
	})
	t.Run("a code that is no input code beside one that is", func(t *testing.T) {
		mixed := Invoice{Currency: "EUR", Exponent: 2, Items: []Item{{Amount: 10000, TaxCode: &v1}, {Amount: 1000, TaxCode: &q9}, {Amount: 500}}}
		r := reader(&codes{code: input, ok: true}, "")
		r.e.Accounts.(*taxCodes).codes["Q9"] = cachedCode{}
		got, err := r.gross(ctx, mixed)
		if err != nil {
			t.Fatalf("error: got %v, want none", err)
		}
		if got != 13400 {
			t.Errorf("gross: got %d, want 13400 (V1's tax alone)", got)
		}
	})
	t.Run("a failing ledger", func(t *testing.T) {
		broken := errors.New("broken")
		if _, err := reader(&codes{err: broken}, "").gross(ctx, v); !errors.Is(err, broken) {
			t.Errorf("error: got %v, want it wrapped", err)
		}
	})
	t.Run("a document the calculator refuses", func(t *testing.T) {
		if _, err := reader(&codes{code: input, ok: true}, "bankers").gross(ctx, v); !errors.Is(err, tax.ErrInvalid) {
			t.Errorf("error: got %v, want tax.ErrInvalid", err)
		}
	})
}

// TestOpenOrderAdvance: the down payments on an order not yet cleared are
// taken from its earliest documents first, a document they cover in full
// left out; an item whose quantity is not a decimal fails the read.
func TestOpenOrderAdvance(t *testing.T) {
	ctx := context.Background()
	day := func(d int) *time.Time { t := time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC); return &t }
	o := procurement.Order{PaymentTermCode: "NT30", Items: []procurement.OrderItem{{ItemNo: 1, Quantity: "1", UnitPrice: 30000, PriceUnit: 1, DeliveryDate: day(1)},
		{ItemNo: 2, Quantity: "1", UnitPrice: 50000, PriceUnit: 1, DeliveryDate: day(8)}}}
	r := openReader{terms: map[string]*ledger.ItemTerms{"NT30": nil}}
	for name, c := range map[string]struct {
		advance money.Minor
		want    string
	}{
		"none":                 {0, "[2026-05-01 -30000 2026-05-08 -50000]"},
		"part of the first":    {10000, "[2026-05-01 -20000 2026-05-08 -50000]"},
		"the first and beyond": {40000, "[2026-05-08 -40000]"},
		"all of it and more":   {90000, "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			items, err := r.order(ctx, o, nil, c.advance)
			if err != nil {
				t.Fatalf("error: got %v, want none", err)
			}
			var got []string
			for _, it := range items {
				got = append(got, it.DocumentDate.Format(time.DateOnly), fmt.Sprint(int64(it.Amount)))
			}
			if s := fmt.Sprint(got); s != c.want {
				t.Errorf("documents: got %s, want %s", s, c.want)
			}
		})
	}
	bad := procurement.Order{Items: []procurement.OrderItem{{ItemNo: 1, Quantity: "x", PriceUnit: 1}}}
	if _, err := r.order(ctx, bad, nil, 0); err == nil {
		t.Error("an item whose quantity is not a decimal: got no error, want one")
	}
}
