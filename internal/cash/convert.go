package cash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
		convert := func(m money.Minor) (money.Minor, error) { return m, nil }
		if c.Currency != display {
			rate, rateType, found, err := e.rate(ctx, r, c.Currency, display)
			if err != nil {
				return Converted{}, err
			}
			if !found {
				out.Incomplete = append(out.Incomplete, c.Currency)
				continue
			}
			convert = rate.Convert
			out.Rates = append(out.Rates, CurrencyRate{Currency: c.Currency, RateType: rateType, Rate: rate.Rate, ValidFrom: rate.ValidFrom,
				Stale: rate.ValidFrom.Before(r.read.AddDate(0, 0, -r.settings.RateMaxAge()))})
		}
		r.sums.converted(out.Days, c, convert)
	}
	return out, nil
}

// converted adds a currency's days, converted, to the totals.
func (s *sums) converted(totals []Day, c CurrencyPosition, convert func(money.Minor) (money.Minor, error)) {
	at := func(m money.Minor) money.Minor {
		v, err := convert(m)
		s.overflow = s.overflow || err != nil
		return v
	}
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
