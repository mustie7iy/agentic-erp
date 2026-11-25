package mrp

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// s is a quantity given, as a plan's field holds it.
func s(v string) *string { return &v }

// TestLots (ADR-0084 As built, lot sizing): each procedure sizes a
// shortage by hand-computed figures, in the base unit, exactly: lot for
// lot proposes the shortage, raised to the minimum, rounded up to the
// rounding value and split by the maximum, the last lot raised to the
// minimum; a fixed lot covers the shortage in whole lots; replenishing
// to the maximum proposes what brings the available quantity up to the
// level, bounded and rounded as lot for lot. No shortage proposes
// nothing.
func TestLots(t *testing.T) {
	ex := func(minimum, maximum, rounding *string) LotSizing {
		return LotSizing{Procedure: LotForLot, Minimum: minimum, Maximum: maximum, Rounding: rounding}
	}
	hb := func(level string, minimum, maximum, rounding *string) LotSizing {
		return LotSizing{Procedure: ReplenishToMaximum, MaximumLevel: s(level), Minimum: minimum, Maximum: maximum, Rounding: rounding}
	}
	fx := LotSizing{Procedure: FixedLot, Fixed: s("100")}
	for _, c := range []struct {
		name                string
		lots                LotSizing
		shortage, available string
		want                string
	}{
		{"lot for lot: the shortage", ex(nil, nil, nil), "37", "13", "37"},
		{"lot for lot: no shortage", ex(s("50"), nil, nil), "0", "60", ""},
		{"lot for lot: raised to the minimum", ex(s("50"), nil, nil), "37", "13", "50"},
		{"lot for lot: above the minimum", ex(s("30"), nil, nil), "37", "13", "37"},
		{"lot for lot: rounded up to a pack of 12", ex(nil, nil, s("12")), "37", "13", "48"},
		{"lot for lot: a multiple of the pack as it is", ex(nil, nil, s("12")), "36", "14", "36"},
		{"lot for lot: the minimum, then rounded", ex(s("24"), nil, s("12")), "13", "37", "24"},
		{"lot for lot: rounded to a fraction", ex(nil, nil, s("0.75")), "2.5", "1", "3"},
		{"lot for lot: rounded, then split by the maximum", ex(nil, s("20"), s("5")), "37", "13", "20 20"},
		{"lot for lot: split, the last lot the rest", ex(nil, s("15"), nil), "37", "13", "15 15 7"},
		{"lot for lot: split, the last lot raised to the minimum", ex(s("10"), s("15"), nil), "32", "18", "15 15 10"},
		{"lot for lot: the maximum exactly", ex(nil, s("37"), nil), "37", "13", "37"},
		{"fixed: one lot covers a small shortage", fx, "0.5", "49.5", "100"},
		{"fixed: one lot exactly", fx, "100", "-50", "100"},
		{"fixed: as many lots as cover it", fx, "250", "-200", "100 100 100"},
		{"fixed: no shortage", fx, "0", "50", ""},
		{"to the maximum: the level less what is available", hb("200", nil, nil, nil), "37", "13", "187"},
		{"to the maximum: from below zero", hb("200", nil, nil, nil), "70", "-20", "220"},
		{"to the maximum: rounded up past the level", hb("200", nil, nil, s("10")), "37", "13", "190"},
		{"to the maximum: split by the maximum lot", hb("200", nil, s("100"), s("10")), "37", "13", "100 90"},
		{"to the maximum: the minimum", hb("60", s("80"), nil, nil), "37", "13", "80"},
		{"to the maximum: a fraction of the base unit rounded up to six decimals", hb("60", nil, nil, nil), "36.666667", "13.3333333", "46.666667"},
		{"to the maximum: no shortage", hb("200", nil, nil, nil), "0", "60", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			lots, err := c.lots.lots(money.ParseRat(c.shortage), money.ParseRat(c.available), "MA1")
			if err != nil {
				t.Fatalf("got %v, want lots", err)
			}
			got := make([]string, len(lots))
			for i, q := range lots {
				got[i] = money.PlainQuantity(q)
			}
			if strings.Join(got, " ") != c.want {
				t.Errorf("got %v, want %s", got, c.want)
			}
		})
	}
}

// TestTooManyLots: a fixed or maximum lot size needing more than MaxLots
// lots for the shortage is refused, naming the material; MaxLots lots
// are proposed.
func TestTooManyLots(t *testing.T) {
	for _, c := range []struct {
		name     string
		lots     LotSizing
		shortage string
		want     int // lots proposed; 0: refused
	}{
		{"fixed: MaxLots lots", LotSizing{Procedure: FixedLot, Fixed: s("1")}, "100", MaxLots},
		{"fixed: one more", LotSizing{Procedure: FixedLot, Fixed: s("1")}, "100.5", 0},
		{"by the maximum: one more", LotSizing{Procedure: LotForLot, Maximum: s("0.5")}, "50.25", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			lots, err := c.lots.lots(money.ParseRat(c.shortage), new(big.Rat), "MA1")
			if c.want > 0 {
				if err != nil || len(lots) != c.want {
					t.Errorf("got %d lots, %v; want %d", len(lots), err, c.want)
				}
				return
			}
			var p *problem.Problem
			if !errors.As(err, &p) || p.Code != "mrp.too_many_lots" || p.Status != 409 || !strings.HasPrefix(p.Detail, "Material MA1 needs 101 lots of") {
				t.Errorf("got %v, want 409 mrp.too_many_lots naming MA1 and its 101 lots", err)
			}
		})
	}
}

// TestLotSizingChecks: a lot sizing names a known procedure, positive
// quantities and exactly the ones its procedure uses, bounded as the
// contract says, each refusal naming its field.
func TestLotSizingChecks(t *testing.T) {
	for _, c := range []struct {
		name        string
		lots        LotSizing
		code, field string // empty: accepted
	}{
		{"lot for lot", LotSizing{Procedure: LotForLot}, "", ""},
		{"lot for lot, bounded and rounded", LotSizing{Procedure: LotForLot, Minimum: s("24"), Maximum: s("480"), Rounding: s("12")}, "", ""},
		{"fixed", LotSizing{Procedure: FixedLot, Fixed: s("7.5")}, "", ""},
		{"to the maximum at the reorder point", LotSizing{Procedure: ReplenishToMaximum, MaximumLevel: s("50")}, "", ""},
		{"an unknown procedure", LotSizing{Procedure: "period"}, "mrp.invalid_lot_size", "lot_size"},
		{"a zero fixed lot", LotSizing{Procedure: FixedLot, Fixed: s("0")}, "mrp.invalid_quantity", "fixed_lot_size"},
		{"a rounding value of seven decimals", LotSizing{Procedure: LotForLot, Rounding: s("0.0000001")}, "mrp.invalid_quantity", "rounding_value"},
		{"a negative minimum", LotSizing{Procedure: LotForLot, Minimum: s("-1")}, "mrp.invalid_quantity", "minimum_lot_size"},
		{"fixed without its lot", LotSizing{Procedure: FixedLot}, "mrp.invalid_lot_size", "fixed_lot_size"},
		{"a fixed lot with lot for lot", LotSizing{Procedure: LotForLot, Fixed: s("10")}, "mrp.invalid_lot_size", "fixed_lot_size"},
		{"to the maximum without its level", LotSizing{Procedure: ReplenishToMaximum}, "mrp.invalid_lot_size", "maximum_stock_level"},
		{"a level with a fixed lot", LotSizing{Procedure: FixedLot, Fixed: s("10"), MaximumLevel: s("90")}, "mrp.invalid_lot_size", "maximum_stock_level"},
		{"a level below the reorder point", LotSizing{Procedure: ReplenishToMaximum, MaximumLevel: s("49.9")}, "mrp.invalid_lot_size", "maximum_stock_level"},
		{"a fixed lot with a minimum", LotSizing{Procedure: FixedLot, Fixed: s("10"), Minimum: s("5")}, "mrp.invalid_lot_size", "minimum_lot_size"},
		{"a fixed lot with a maximum", LotSizing{Procedure: FixedLot, Fixed: s("10"), Maximum: s("50")}, "mrp.invalid_lot_size", "maximum_lot_size"},
		{"a fixed lot rounded", LotSizing{Procedure: FixedLot, Fixed: s("10"), Rounding: s("5")}, "mrp.invalid_lot_size", "rounding_value"},
		{"a minimum above the maximum", LotSizing{Procedure: LotForLot, Minimum: s("60"), Maximum: s("50")}, "mrp.invalid_lot_size", "minimum_lot_size"},
		{"a minimum not a multiple of the rounding", LotSizing{Procedure: LotForLot, Minimum: s("25"), Rounding: s("12")}, "mrp.invalid_lot_size", "rounding_value"},
		{"a maximum not a multiple of the rounding", LotSizing{Procedure: ReplenishToMaximum, MaximumLevel: s("90"), Maximum: s("50"), Rounding: s("0.3")},
			"mrp.invalid_lot_size", "rounding_value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.lots.check("50")
			if c.code == "" {
				if err != nil {
					t.Errorf("got %v, want it accepted", err)
				}
				return
			}
			var p *problem.Problem
			if !errors.As(err, &p) {
				t.Fatalf("got %v, want 422 %s", err, c.code)
			}
			if got := fmt.Sprint(p.Status, " ", p.Code, " ", p.Errors[0].Field); got != fmt.Sprint(422, " ", c.code, " ", c.field) {
				t.Errorf("got %s, want 422 %s %s", got, c.code, c.field)
			}
		})
	}
}
