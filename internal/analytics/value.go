package analytics

import (
	"fmt"
	"math/big"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// ratioScale is the number of digits a ratio keeps (0.1234 = 12.34%).
const ratioScale = 4

// Value is a measure in a result: an exact decimal of Units at Scale
// digits. An amount is minor units of its Currency at that currency's
// exponent (ref.currency, ADR-0005), so a BHD total keeps its third decimal
// and a JPY total shows none; a count has scale 0 and a ratio ratioScale.
// Units is a money.Minor for its exact, range-checked arithmetic. Floats
// appear only where a value is drawn or matched against prose (render.go,
// narrative.go).
type Value struct {
	Units    money.Minor
	Scale    int
	Currency string
}

// String is the plain decimal, "-1234.567" or "5000".
func (v Value) String() string { return v.Units.Plain(v.Scale) }

// MarshalJSON writes the value as a JSON number with exactly its digits,
// so an agent reads 1234.567 for a BHD total and 5000 for a JPY one.
func (v Value) MarshalJSON() ([]byte, error) { return []byte(v.String()), nil }

// rat is the value as an exact rational in major units.
func (v Value) rat() *big.Rat {
	return money.Rat(v.Units, v.Scale)
}

// abs is the value without its sign; an amount always has a positive
// counterpart (money.ErrOverflow).
func (v Value) abs() Value {
	v.Units = v.Units.Abs()
	return v
}

// compare orders two values by size, exactly, whatever their scales.
func (v Value) compare(o Value) int { return v.rat().Cmp(o.rat()) }

// sameKind refuses arithmetic across currencies (or scales): adding a BHD
// amount to a USD one means nothing.
func (v Value) sameKind(o Value) error {
	if v.Currency != o.Currency || v.Scale != o.Scale {
		return fmt.Errorf("cannot combine %s and %s", v.kind(), o.kind())
	}
	return nil
}

// kind names a value's unit for messages: its currency, or its digits.
func (v Value) kind() string {
	if v.Currency != "" {
		return "amounts in " + v.Currency
	}
	return fmt.Sprintf("values with %d decimals", v.Scale)
}

// sumValues adds values of one kind exactly; an empty list is no value.
func sumValues(vals []Value) (Value, bool, error) {
	if len(vals) == 0 {
		return Value{}, false, nil
	}
	units := make([]money.Minor, len(vals))
	for i, v := range vals {
		if err := vals[0].sameKind(v); err != nil {
			return Value{}, false, err
		}
		units[i] = v.Units
	}
	total, err := money.Sum(units...)
	if err != nil {
		return Value{}, false, fmt.Errorf("the total is out of range: %w", err)
	}
	return Value{Units: total, Scale: vals[0].Scale, Currency: vals[0].Currency}, true, nil
}

// diff is cur - base, of one kind.
func diff(cur, base Value) (Value, error) {
	if err := cur.sameKind(base); err != nil {
		return Value{}, err
	}
	d, err := money.Diff(cur.Units, base.Units)
	if err != nil {
		return Value{}, fmt.Errorf("the change is out of range: %w", err)
	}
	return Value{Units: d, Scale: cur.Scale, Currency: cur.Currency}, nil
}
