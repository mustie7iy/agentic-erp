package cash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// currencies sums the banks' positions per currency, by currency code:
// each day's balances and flows, and the credit lines of those that have
// one.
func (s *sums) currencies(banks []BankPosition) []CurrencyPosition {
	var out []CurrencyPosition
	at := map[string]int{}
	for _, b := range banks {
		i, ok := at[b.Currency]
		if !ok {
			i, at[b.Currency] = len(out), len(out)
			out = append(out, CurrencyPosition{Currency: b.Currency, Exponent: b.Exponent, Days: make([]Day, len(b.Days))})
			for d := range b.Days {
				out[i].Days[d] = Day{Date: b.Days[d].Date, Flows: []FlowTotal{}}
			}
		}
		c := &out[i]
		if b.CreditLine != nil {
			if c.CreditLine == nil {
				c.CreditLine = new(money.Minor)
			}
			*c.CreditLine = s.add(*c.CreditLine, *b.CreditLine)
		}
		for d, day := range b.Days {
			s.into(&c.Days[d], day)
			for _, t := range day.Flows {
				c.Days[d].Flows = s.addTotal(c.Days[d].Flows, t)
			}
		}
	}
	slices.SortFunc(out, func(a, b CurrencyPosition) int { return cmp.Compare(a.Currency, b.Currency) })
	return out
}

// into adds a day's balances to another's.
func (s *sums) into(to *Day, d Day) {
	to.Opening, to.Closing, to.Available = s.add(to.Opening, d.Opening), s.add(to.Closing, d.Closing), s.add(to.Available, d.Available)
}

// convert converts every currency's days into the display currency at
// one rate for every day and sums them: the latest rate of the company's
// rate type valid on the read date, planning by default, the average rate
// when there is no planning rate (ADR-0051 §10). A rate valid from more
// than the company's maximum age before the read date is used and marked
// stale; a currency without a rate is left out and listed incomplete,
// since its own figures stand without it.
func (e *Endpoints) convert(ctx context.Context, r reader, cs []CurrencyPosition, display string, exp int, from time.Time, days int) (Converted, error) {
	out := Converted{Currency: display, Exponent: exp, Rates: []CurrencyRate{}, Incomplete: []string{}, Days: make([]Day, days)}
	for i := range out.Days {
		out.Days[i].Date = from.AddDate(0, 0, i)
	}
	for _, c := range cs {
		convert, rate, err := e.converter(ctx, r, c.Currency, display)
		switch {
		case err != nil:
			return Converted{}, err
		case convert == nil:
			out.Incomplete = append(out.Incomplete, c.Currency)
			continue
		case rate != nil:
			out.Rates = append(out.Rates, *rate)
		}
		r.sums.converted(out.Days, c, convert)
	}
	return out, nil
}

// converter converts amounts of a currency into the display currency
// (ADR-0051 §10): as they are in the display currency itself (no rate),
// else at the latest rate of the company's rate type valid on the read
// date, each amount rounded once to the display currency's minor unit,
// half away from zero (ledger.Conversion.Convert, the rate service
// revaluation reads too), with the rate used, stale beyond the company's
// maximum age. A currency without a rate has no converter (nil): it is
// left out and listed incomplete, never counted as zero.
func (e *Endpoints) converter(ctx context.Context, r reader, from, display string) (func(money.Minor) (money.Minor, error), *CurrencyRate, error) {
	if from == display {
		return func(m money.Minor) (money.Minor, error) { return m, nil }, nil, nil
	}
	rate, rateType, found, err := e.rate(ctx, r, from, display)
	if err != nil || !found {
		return nil, nil, err
	}
	return rate.Convert, &CurrencyRate{Currency: from, RateType: rateType, Rate: rate.Rate, ValidFrom: rate.ValidFrom,
		Stale: rate.ValidFrom.Before(r.read.AddDate(0, 0, -r.settings.RateMaxAge()))}, nil
}

// ForecastConverted is a forecast's currencies in the display currency
// (ADR-0051 §10, T53e4): the rates used, the currencies without one (left
// out), and the openings, the buckets' totals and what is beyond, each
// converted and summed over the currencies; each bucket's closing is the
// converted opening plus the converted totals so far, so the converted
// figures add up as each currency's do.
type ForecastConverted struct {
	Currency        string
	Exponent        int
	Rates           []CurrencyRate
	Incomplete      []string
	Opening         money.Minor
	Totals, Closing []money.Minor // one per bucket
	Total, Beyond   money.Minor
}

// convertForecast converts the forecast's currencies into the display
// currency, at one rate per currency for every bucket. A display currency
// the reference data does not know is refused (422 cash.unknown_currency).
func (e *Endpoints) convertForecast(ctx context.Context, tx pgx.Tx, r reader, cs []ForecastCurrency, display string, buckets int) (ForecastConverted, error) {
	exp, err := currency.Exponent(ctx, tx, display, problem.Unprocessable("cash.unknown_currency", "The display currency is not known.",
		problem.FieldError{Field: "display_currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"}))
	if err != nil {
		return ForecastConverted{}, err
	}
	out := ForecastConverted{Currency: display, Exponent: exp, Rates: []CurrencyRate{}, Incomplete: []string{}, Totals: make([]money.Minor, buckets),
		Closing: make([]money.Minor, buckets)}
	for _, c := range cs {
		convert, rate, err := e.converter(ctx, r, c.Currency, display)
		switch {
		case err != nil:
			return ForecastConverted{}, err
		case convert == nil:
			out.Incomplete = append(out.Incomplete, c.Currency)
			continue
		case rate != nil:
			out.Rates = append(out.Rates, *rate)
		}
		at := r.sums.at(convert)
		out.Opening, out.Beyond = r.sums.add(out.Opening, at(c.Opening.Amount)), r.sums.add(out.Beyond, at(c.Beyond.Amount))
		for b, bf := range c.Buckets {
			out.Totals[b] = r.sums.add(out.Totals[b], at(bf.Total))
		}
	}
	closing := out.Opening
	for b, t := range out.Totals {
		out.Total, closing = r.sums.add(out.Total, t), r.sums.add(closing, t)
		out.Closing[b] = closing
	}
	return out, nil
}

// at is the converter remembering a conversion out of the range of an
// amount as an overflow.
func (s *sums) at(convert func(money.Minor) (money.Minor, error)) func(money.Minor) money.Minor {
	return func(m money.Minor) money.Minor {
		v, err := convert(m)
		s.overflow = s.overflow || err != nil
		return v
	}
}

// converted adds a currency's days, converted, to the totals.
func (s *sums) converted(totals []Day, c CurrencyPosition, convert func(money.Minor) (money.Minor, error)) {
	at := s.at(convert)
	for i, d := range c.Days {
		s.into(&totals[i], Day{Opening: at(d.Opening), Closing: at(d.Closing), Available: at(d.Available)})
	}
}

// rate is the rate from a currency into the display currency and its
// type: the company's rate type, or average where it is planning and no
// planning rate is recorded.
func (e *Endpoints) rate(ctx context.Context, r reader, from, to string) (Rate, string, bool, error) {
	rateType := r.settings.RateTypeOrPlanning()
	rate, found, err := e.Ledger.Rate(ctx, rateType, from, to, r.read)
	if err == nil && !found && rateType == config.DefaultCashRateType {
		rateType = "average"
		rate, found, err = e.Ledger.Rate(ctx, rateType, from, to, r.read)
	}
	if err != nil {
		return Rate{}, "", false, fmt.Errorf("cash: %s rate %s/%s: %w", rateType, from, to, err)
	}
	return rate, rateType, found, nil
}
