package sales

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// What a record's scale is read by (ADR-0067 §12): the item's quantity,
// or its value before the condition, a discount's only.
const (
	ScaleQuantity = "quantity"
	ScaleValue    = "value"
)

// ScaleLevel is a level of a record's scale (ADR-0067 §12): its number
// from 1, where it starts, a quantity in the scale's unit or a value in
// minor units of the scale's currency, and its rate of the record's kind,
// a price per the record's price unit in its currency or a percent.
type ScaleLevel struct {
	Level        int32
	FromQuantity *string
	FromAmount   *money.Minor
	UnitPrice    *money.Minor
	Percent      *string
}

// Measure is a quantity as a client sends it: a decimal string in a unit
// of measure.
type Measure struct{ Value, Unit string }

// ScaleLevelInput is a scale level as a client sends it: where it starts
// by the scale's basis, and its rate by the record's kind.
type ScaleLevelInput struct {
	FromQuantity *Measure
	FromAmount   *Amount
	UnitPrice    *Amount
	Percent      *string
}

// ConditionScale is the scale level an item reached on the record its
// condition was found by: the record's basis, the level, 0 below the
// first, and where the level starts, a quantity in Unit or a value in
// minor units of the document's currency.
type ConditionScale struct {
	Basis        string       `json:"basis"`
	Level        int32        `json:"level"`
	FromQuantity *string      `json:"from_quantity,omitempty"`
	Unit         *string      `json:"unit,omitempty"`
	FromAmount   *money.Minor `json:"from_amount,omitempty"`
}

// scaled is a record's scale as checked: its basis, the unit or the
// currency it is read in, and its levels by their start; the zero value
// is a record without a scale.
type scaled struct {
	basis, unit, currency *string
	levels                []ScaleLevel
}

// invalidScale refuses a scale naming the request's field (422
// sales.invalid_scale).
func invalidScale(field, detail string) *problem.Problem {
	return problem.Invalid("sales.invalid_scale", field, detail)
}

// scale checks a record's scale (ADR-0067 §12): a basis and levels come
// together; the basis is quantity or, for a discount, value; each level
// starts above the one before and gives the rate of the record's kind
// (scaleCheck). A record without either has no scale.
func scale(ctx context.Context, tx pgx.Tx, in PriceInput, r rate) (scaled, error) {
	switch {
	case in.ScaleBasis == nil && len(in.Scale) == 0:
		return scaled{}, nil
	case in.ScaleBasis == nil:
		return scaled{}, invalidScale("scale_basis", "A scale states its basis, quantity or value.")
	case len(in.Scale) == 0:
		return scaled{}, invalidScale("scale", "A scale basis comes with the scale's levels.")
	case *in.ScaleBasis != ScaleQuantity && *in.ScaleBasis != ScaleValue:
		return scaled{}, invalidScale("scale_basis", "A scale is read by quantity or by value.")
	case *in.ScaleBasis == ScaleValue && r.percent == nil:
		return scaled{}, invalidScale("scale_basis", "A price's scale is by quantity; a value scale is a discount's, read by the item's value before it.")
	}
	c := scaleCheck{ctx: ctx, tx: tx, basis: *in.ScaleBasis, r: r, out: scaled{basis: in.ScaleBasis}}
	if r.percent == nil { // a price's scale is in its own unit
		c.out.unit = r.unit
	}
	for i, l := range in.Scale {
		level, err := c.level(i, l)
		if err != nil {
			return scaled{}, err
		}
		c.out.levels = append(c.out.levels, level)
	}
	return c.out, nil
}

// scaleCheck checks a scale's levels in order: the unit or the currency
// the first states, which the others repeat, and where the one before
// starts, which each passes.
type scaleCheck struct {
	ctx      context.Context
	tx       pgx.Tx
	basis    string
	r        rate
	exp      int // the scale currency's minor-unit digits
	previous *big.Rat
	out      scaled
}

// level checks level i: where it starts (from) and its rate.
func (c *scaleCheck) level(i int, l ScaleLevelInput) (ScaleLevel, error) {
	field := fmt.Sprintf("scale[%d]", i)
	level := ScaleLevel{Level: int32(i + 1)}
	var err error
	if c.basis == ScaleQuantity {
		level.FromQuantity, err = c.fromQuantity(field, l)
	} else {
		level.FromAmount, err = c.fromAmount(field, l)
	}
	if err != nil {
		return ScaleLevel{}, err
	}
	if c.r.percent != nil {
		level.Percent, err = levelPercent(field, l)
	} else {
		level.UnitPrice, err = c.levelPrice(field, l)
	}
	return level, err
}

// rises refuses a start not above the one before (sales.invalid_scale
// naming field) and keeps it for the next.
func (c *scaleCheck) rises(field string, from *big.Rat) error {
	if c.previous != nil && from.Cmp(c.previous) <= 0 {
		return invalidScale(field, "Each scale level starts above the one before.")
	}
	c.previous = from
	return nil
}

// fromQuantity is where a level of a quantity scale starts: a quantity
// more than zero, in the unit the first level states, a price's own (422
// sales.invalid_scale), a unit of the reference data (422
// sales.unknown_unit).
func (c *scaleCheck) fromQuantity(field string, l ScaleLevelInput) (*string, error) {
	if l.FromAmount != nil {
		return nil, invalidScale(field+".from_amount", "A level of a quantity scale starts at a from_quantity.")
	}
	if l.FromQuantity == nil {
		return nil, invalidScale(field+".from_quantity", "A level of a quantity scale starts at a from_quantity.")
	}
	q := *l.FromQuantity
	if money.PositiveQuantity(q.Value) != nil {
		return nil, invalidScale(field+".from_quantity.value", fmt.Sprintf(
			"A scale level starts at a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits, money.QuantityDecimals))
	}
	switch {
	case c.out.unit == nil:
		units, err := knownUnits(c.ctx, c.tx, []string{q.Unit})
		if err != nil {
			return nil, err
		}
		if !units[q.Unit] {
			return nil, problem.Invalid("sales.unknown_unit", field+".from_quantity.unit", "Unit of measure "+q.Unit+" is not known.")
		}
		c.out.unit = &q.Unit
	case q.Unit != *c.out.unit:
		return nil, invalidScale(field+".from_quantity.unit", "A scale's levels are in one unit, "+*c.out.unit+".")
	}
	return &q.Value, c.rises(field+".from_quantity.value", money.ParseRat(q.Value))
}

// fromAmount is where a level of a value scale starts: an amount more
// than zero, in the currency the first level states (422
// sales.invalid_scale), a known one (422 sales.unknown_currency).
func (c *scaleCheck) fromAmount(field string, l ScaleLevelInput) (*money.Minor, error) {
	if l.FromQuantity != nil {
		return nil, invalidScale(field+".from_quantity", "A level of a value scale starts at a from_amount.")
	}
	if l.FromAmount == nil {
		return nil, invalidScale(field+".from_amount", "A level of a value scale starts at a from_amount.")
	}
	a := *l.FromAmount
	switch {
	case c.out.currency == nil:
		exp, err := currency.Exponent(c.ctx, c.tx, a.Currency, problem.Invalid("sales.unknown_currency", field+".from_amount.currency", "The currency is not known."))
		if err != nil {
			return nil, fmt.Errorf("sales: currency %s of a scale: %w", a.Currency, err)
		}
		c.out.currency, c.exp = &a.Currency, exp
	case a.Currency != *c.out.currency:
		return nil, invalidScale(field+".from_amount.currency", "A scale's levels are in one currency, "+*c.out.currency+".")
	}
	from, err := currency.ParseAmount(a.Value, a.Currency, c.exp, field+".from_amount.amount")
	if err != nil {
		return nil, fmt.Errorf("sales: start of a scale level: %w", err)
	}
	if from <= 0 {
		return nil, invalidScale(field+".from_amount.amount", "A scale level starts at an amount more than zero.")
	}
	return &from, c.rises(field+".from_amount.amount", big.NewRat(int64(from), 1))
}

// levelPrice is a price's rate at a level: a unit price per the record's
// price unit, in its currency (422 sales.currency_mismatch), more than
// zero (422 sales.amount_not_positive); a percent is a discount's (422
// sales.invalid_scale).
func (c *scaleCheck) levelPrice(field string, l ScaleLevelInput) (*money.Minor, error) {
	if l.Percent != nil {
		return nil, invalidScale(field+".percent", "A price's scale level gives a unit_price, not a percent.")
	}
	if l.UnitPrice == nil {
		return nil, invalidScale(field+".unit_price", "A price's scale level gives a unit_price.")
	}
	if l.UnitPrice.Currency != *c.r.currency {
		return nil, problem.Invalid("sales.currency_mismatch", field+".unit_price.currency", "A scale level's price is in the record's currency, "+*c.r.currency+".")
	}
	amount, err := currency.ParseAmount(l.UnitPrice.Value, l.UnitPrice.Currency, c.r.exp, field+".unit_price.amount")
	if err != nil {
		return nil, fmt.Errorf("sales: price of a scale level: %w", err)
	}
	if amount <= 0 {
		return nil, problem.Invalid("sales.amount_not_positive", field+".unit_price.amount", "A price is more than zero.")
	}
	return &amount, nil
}

// levelPercent is a discount's rate at a level, a percent as a
// discount's own (discountRate); a unit price is a price's (422
// sales.invalid_scale).
func levelPercent(field string, l ScaleLevelInput) (*string, error) {
	if l.UnitPrice != nil {
		return nil, invalidScale(field+".unit_price", "A discount's scale level gives a percent, not a unit_price.")
	}
	if l.Percent == nil {
		return nil, invalidScale(field+".percent", "A discount's scale level gives a percent.")
	}
	r, err := discountRate(*l.Percent, field+".percent")
	return r.percent, err
}

// insertScale writes a new record's scale levels.
func insertScale(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, levels []ScaleLevel) error {
	if len(levels) == 0 {
		return nil
	}
	n := len(levels)
	nos, quantities, amounts, prices, percents := make([]int32, n), make([]*string, n), make([]*money.Minor, n), make([]*money.Minor, n), make([]*string, n)
	for i, l := range levels {
		nos[i], quantities[i], amounts[i], prices[i], percents[i] = l.Level, l.FromQuantity, l.FromAmount, l.UnitPrice, l.Percent
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.price_scale (tenant_id, price_id, level, from_quantity, from_amount, unit_price, percent)
		SELECT $1, $2, n, q::numeric, a, p, pc::numeric FROM unnest($3::int[], $4::text[], $5::bigint[], $6::bigint[], $7::text[]) AS x(n, q, a, p, pc)`,
		tenant, id, nos, quantities, amounts, prices, percents); err != nil {
		return fmt.Errorf("sales: insert the scale of price %s: %w", id, err)
	}
	return nil
}

// withScales gives the records their scale levels, each record's by
// level.
func withScales(ctx context.Context, tx pgx.Tx, prices []Price) error {
	ids := make([]uuid.UUID, 0, len(prices))
	for _, p := range prices {
		if p.ScaleBasis != nil {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	type owned struct {
		price uuid.UUID
		ScaleLevel
	}
	rows, _ := tx.Query(ctx, `SELECT price_id, level, trim_scale(from_quantity)::text, from_amount, unit_price, trim_scale(percent)::text
		FROM sales.price_scale WHERE price_id = ANY($1) ORDER BY price_id, level`, ids)
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (owned, error) {
		var o owned
		err := r.Scan(&o.price, &o.Level, &o.FromQuantity, &o.FromAmount, &o.UnitPrice, &o.Percent)
		return o, errs.Wrapf(err, "scan a scale level")
	})
	if err != nil {
		return fmt.Errorf("sales: read the scales of %d prices: %w", len(ids), err)
	}
	levels := map[uuid.UUID][]ScaleLevel{}
	for _, o := range all {
		levels[o.price] = append(levels[o.price], o.ScaleLevel)
	}
	for i := range prices {
		prices[i].Scale = levels[prices[i].ID]
	}
	return nil
}

// reached is the scale level of record id an item reaches (ADR-0067 §12):
// the one with the highest start its quantity, a plain decimal in the
// scale's unit, or its value in minor units of the scale's currency
// reaches, by the basis; level 0, with nothing else, below the first.
func reached(ctx context.Context, tx pgx.Tx, id uuid.UUID, basis, quantity string, value money.Minor) (ScaleLevel, error) {
	var l ScaleLevel
	err := tx.QueryRow(ctx, `SELECT level, trim_scale(from_quantity)::text, from_amount, unit_price, trim_scale(percent)::text
		FROM sales.price_scale WHERE price_id = $1 AND CASE WHEN $2 = 'quantity' THEN from_quantity <= $3::numeric ELSE from_amount <= $4 END
		ORDER BY coalesce(from_quantity, from_amount) DESC LIMIT 1`, id, basis, quantity, value).Scan(&l.Level, &l.FromQuantity, &l.FromAmount, &l.UnitPrice, &l.Percent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScaleLevel{}, nil
	}
	if err != nil {
		return ScaleLevel{}, fmt.Errorf("sales: scale level of price %s: %w", id, err)
	}
	return l, nil
}

// conditionScale is the scale level l of a record of basis an item in
// unit reached, as its condition keeps it.
func conditionScale(basis string, l ScaleLevel, unit string) *ConditionScale {
	s := &ConditionScale{Basis: basis, Level: l.Level, FromQuantity: l.FromQuantity, FromAmount: l.FromAmount}
	if l.FromQuantity != nil {
		s.Unit = &unit
	}
	return s
}
