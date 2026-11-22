package demandagent

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The method (decided by default, ADR-0082 As built S2m, from the
// analytics context's moving average, ADR-0077 §7, and the cash
// forecast's accuracy, ADR-0051 §11): weeks of seven days, the latest
// ending on the run's day, which has passed as a cash forecast's value
// date has; the moving average of the last HistoryWeeks weeks; a horizon
// of HorizonWeeks weeks from the day after; the accuracy over the last
// AccuracyWeeks weeks, each forecast by the average of the HistoryWeeks
// weeks before it, so HistoryWeeks + AccuracyWeeks weeks are read.
const (
	WeekDays      = 7
	HistoryWeeks  = 13
	HorizonWeeks  = 8
	AccuracyWeeks = 4
)

// issues are the movement types whose lines are demand at the plant (an
// issue to a cost center, a delivery's goods issue, an intercompany goods
// issue) and their reversals, which give it back: a line's quantity,
// negated. Scrapping, transfers, receipts and count differences are no
// demand (SAP's consumption, from memory).
var issues = map[string]bool{"201": true, "202": true, "601": true, "602": true, "643": true}

// quantity is a Quantity as an answer gives it.
type quantity struct {
	Value string `json:"value"`
	Unit  string `json:"unit"`
}

// document is a material document as the run reads it.
type document struct {
	PostingDate  string `json:"posting_date"`
	MovementType string `json:"movement_type"`
	Lines        []struct {
		MaterialID string   `json:"material_id"`
		PlantID    string   `json:"plant_id"`
		InTransit  bool     `json:"in_transit"`
		Quantity   quantity `json:"quantity"`
	} `json:"lines"`
}

// order is a confirmed sales order as the run reads it.
type order struct {
	DocumentNumber        string `json:"document_number"`
	Status                string `json:"status"`
	OrderDate             string `json:"order_date"`
	RequestedDeliveryDate string `json:"requested_delivery_date"`
	PlantID               string `json:"plant_id"`
	Items                 []struct {
		MaterialID        string    `json:"material_id"`
		Quantity          quantity  `json:"quantity"`
		DeliveredQuantity *quantity `json:"delivered_quantity"`
		RemovedInChange   *int      `json:"removed_in_change"`
	} `json:"items"`
}

// plantForecast is a plant's weeks: what was issued in each week read,
// the latest first, and what open orders are due in each week of the
// horizon, in the base unit.
type plantForecast struct {
	id, code string
	issued   []*big.Rat // HistoryWeeks + AccuracyWeeks, the latest first
	ordered  []*big.Rat // HorizonWeeks
	line     string
}

// forecast is what the run read and computed: its days and the plants'
// forecasts by code.
type forecast struct {
	day, first, start string // the run's day, the first day read, the horizon's first day
	unit              string
	plants            []*plantForecast
	byID              map[string]*plantForecast
	factors           map[string]*big.Rat // base units per unit
}

// read reads the material's goods issues of the weeks read and its
// confirmed sales orders in the company, and computes each plant's
// forecast.
func (a *Agent) read(ctx context.Context, le string, m material, day string) (*forecast, error) {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return nil, fmt.Errorf("demandagent: the day %q: %w", day, err)
	}
	f := &forecast{day: day, first: d.AddDate(0, 0, 1-WeekDays*(HistoryWeeks+AccuracyWeeks)).Format(time.DateOnly),
		start: d.AddDate(0, 0, 1).Format(time.DateOnly), unit: m.BaseUnit, byID: map[string]*plantForecast{},
		factors: map[string]*big.Rat{m.BaseUnit: big.NewRat(1, 1)}}
	for _, u := range m.AlternativeUnits {
		if u.Numerator > 0 && u.Denominator > 0 {
			f.factors[u.Unit] = big.NewRat(u.Numerator, u.Denominator)
		}
	}
	for _, p := range m.Plants {
		pf := &plantForecast{id: p.PlantID, code: p.PlantCode, issued: zeros(HistoryWeeks + AccuracyWeeks), ordered: zeros(HorizonWeeks)}
		f.plants, f.byID[p.PlantID] = append(f.plants, pf), pf
	}
	slices.SortFunc(f.plants, func(x, y *plantForecast) int { return cmp.Compare(x.code, y.code) })
	if err := f.documents(ctx, a.API, m.ID, d); err != nil {
		return nil, fmt.Errorf("demandagent: the material documents of material %s: %w", m.Number, err)
	}
	if err := f.orders(ctx, a.API, le, m.ID, d); err != nil {
		return nil, fmt.Errorf("demandagent: the sales orders of material %s: %w", m.Number, err)
	}
	for _, pf := range f.plants {
		pf.line = f.text(m.Number, pf)
	}
	return f, nil
}

func zeros(n int) []*big.Rat {
	out := make([]*big.Rat, n)
	for i := range out {
		out[i] = new(big.Rat)
	}
	return out
}

// documents reads the material's documents and adds each issue line at
// one of its plants, posted in the weeks read, to its week. Every page is
// read: the list is in the order documents were created, which is no
// order of their posting dates.
func (f *forecast) documents(ctx context.Context, c *apitools.Client, materialID string, day time.Time) error {
	var bad error
	err := apitools.Each(ctx, c, "/v1/material-documents", url.Values{"material_id": {materialID}}, func(doc document) bool {
		if !issues[doc.MovementType] || doc.PostingDate < f.first || doc.PostingDate > f.day {
			return true
		}
		posted, err := time.Parse(time.DateOnly, doc.PostingDate)
		if err != nil {
			bad = fmt.Errorf("demandagent: a posting date %q: %w", doc.PostingDate, err)
			return false
		}
		week := int(day.Sub(posted).Hours()) / 24 / WeekDays
		for _, l := range doc.Lines {
			pf := f.byID[l.PlantID]
			if pf == nil || l.MaterialID != materialID || l.InTransit {
				continue
			}
			q, err := f.base(l.Quantity)
			if err != nil {
				bad = err
				return false
			}
			pf.issued[week].Sub(pf.issued[week], q)
		}
		return true
	})
	if err != nil {
		return err
	}
	return bad
}

// orders reads the company's confirmed orders of the material and adds
// what each standing item of it at one of its plants has not delivered to
// the week of the horizon it is due in: on the day the customer asked
// for, else the order's date; one due before the horizon in its first
// week, one due after it left out.
func (f *forecast) orders(ctx context.Context, c *apitools.Client, le, materialID string, day time.Time) error {
	var bad error
	q := url.Values{"legal_entity_id": {le}, "status": {"confirmed"}, "material_id": {materialID}}
	err := apitools.Each(ctx, c, "/v1/sales-orders", q, func(o order) bool {
		pf := f.byID[o.PlantID]
		if pf == nil || o.Status != "confirmed" {
			return true
		}
		due, err := time.Parse(time.DateOnly, cmp.Or(o.RequestedDeliveryDate, o.OrderDate))
		if err != nil {
			bad = fmt.Errorf("demandagent: order %s's day: %w", o.DocumentNumber, err)
			return false
		}
		week := max(0, int(due.Sub(day).Hours())/24-1) / WeekDays
		if week >= HorizonWeeks {
			return true
		}
		for _, it := range o.Items {
			if it.MaterialID != materialID || it.RemovedInChange != nil {
				continue
			}
			open, err := f.open(it.Quantity, it.DeliveredQuantity)
			if err != nil {
				bad = fmt.Errorf("demandagent: order %s: %w", o.DocumentNumber, err)
				return false
			}
			pf.ordered[week].Add(pf.ordered[week], open)
		}
		return true
	})
	if err != nil {
		return err
	}
	return bad
}

// open is what an item ordered and has not delivered, in the base unit;
// none when it delivered it all.
func (f *forecast) open(ordered quantity, delivered *quantity) (*big.Rat, error) {
	o, err := f.base(ordered)
	if err != nil || delivered == nil {
		return o, err
	}
	d, err := f.base(*delivered)
	if err != nil {
		return nil, err
	}
	return money.MaxRat(new(big.Rat).Sub(o, d), new(big.Rat)), nil
}

// base is a quantity in the material's base unit, by its unit's
// conversion; a unit the material has not is refused, never guessed.
func (f *forecast) base(q quantity) (*big.Rat, error) {
	factor, ok := f.factors[q.Unit]
	if !ok {
		return nil, fmt.Errorf("demandagent: a quantity in %s, which is no unit of the material", q.Unit)
	}
	v, err := money.ParseDecimalRat(q.Value)
	if err != nil {
		return nil, fmt.Errorf("demandagent: a quantity %q: %w", q.Value, err)
	}
	return v.Mul(v, factor), nil
}

// empty reports whether nothing was issued in the weeks read and nothing
// is open at any plant.
func (f *forecast) empty() bool {
	for _, pf := range f.plants {
		for _, q := range slices.Concat(pf.issued, pf.ordered) {
			if q.Sign() != 0 {
				return false
			}
		}
	}
	return true
}

// lines are the plants' lines the run lists, the first MaxLines.
func (f *forecast) lines() []string {
	var out []string
	for _, pf := range f.plants[:min(len(f.plants), MaxLines)] {
		out = append(out, pf.line)
	}
	return out
}

// average is the moving average of the HistoryWeeks weeks from the one at
// from back, never below nothing.
func (pf *plantForecast) average(from int) *big.Rat {
	sum := total(pf.issued[from : from+HistoryWeeks])
	return money.MaxRat(sum.Quo(sum, big.NewRat(HistoryWeeks, 1)), new(big.Rat))
}

// total is the weeks' quantities added, exactly.
func total(weeks []*big.Rat) *big.Rat {
	sum := new(big.Rat)
	for _, q := range weeks {
		sum.Add(sum, q)
	}
	return sum
}

// weeks are the forecast of each week of the horizon: the greater of the
// moving average and the open orders due that week.
func (pf *plantForecast) weeks() []*big.Rat {
	avg := pf.average(0)
	out := make([]*big.Rat, HorizonWeeks)
	for h, o := range pf.ordered {
		out[h] = money.MaxRat(o, avg)
	}
	return out
}

// accuracy is 100 less the WAPE of the last AccuracyWeeks weeks, each
// forecast by the moving average of the weeks before it: Σ|issued −
// forecast| / Σ|forecast|, rounded to two decimals half away from zero as
// money.AbsPercentOf rounds (ADR-0051 §11), then taken from 100; none when
// nothing was forecast.
func (pf *plantForecast) accuracy() (string, bool) {
	errSum, fcSum := new(big.Rat), new(big.Rat)
	for k := range AccuracyWeeks {
		fc := pf.average(k + 1)
		errSum.Add(errSum, new(big.Rat).Abs(new(big.Rat).Sub(pf.issued[k], fc)))
		fcSum.Add(fcSum, fc)
	}
	if fcSum.Sign() == 0 {
		return "", false
	}
	wape := money.ParseRat(new(big.Rat).Quo(errSum.Mul(errSum, big.NewRat(100, 1)), fcSum).FloatString(2))
	return new(big.Rat).Sub(big.NewRat(100, 1), wape).FloatString(2), true
}

// text is a plant's line.
func (f *forecast) text(number string, pf *plantForecast) string {
	var weeks []string
	for _, q := range pf.weeks() {
		weeks = append(weeks, plain(q))
	}
	issued, ordered := total(pf.issued[:HistoryWeeks]), total(pf.ordered)
	acc := fmt.Sprintf("accuracy none over %d weeks: nothing was forecast", AccuracyWeeks)
	if pct, ok := pf.accuracy(); ok {
		acc = fmt.Sprintf("accuracy %s%% over %d weeks", pct, AccuracyWeeks)
	}
	return fmt.Sprintf("Forecast %s at %s (plant %s), %s per week from %s: %s; issued %s in %d weeks, open orders %s; %s", number, pf.code, pf.id, f.unit,
		f.start, strings.Join(weeks, ", "), plain(issued), HistoryWeeks, plain(ordered), acc)
}

// plain is a quantity to three decimals, rounded half away from zero,
// without trailing zeros: "2.308", "30", "0" (a negative that rounds to
// nothing too).
func plain(q *big.Rat) string {
	s := strings.TrimSuffix(strings.TrimRight(q.FloatString(3), "0"), ".")
	if s == "-0" {
		return "0"
	}
	return s
}
