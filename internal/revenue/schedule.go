package revenue

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The recognition schedule (ADR-0068 §8, T81c): what an active contract's
// obligations give each fiscal period of the company. Nothing of it is
// stored. It is computed from the obligations as activation fixed them
// and from the delivery confirmations of the contract's sales order as
// they are at the read, to a day: what the schedule gives days up to and
// including that day is recognized, what it gives later days is planned,
// and a point-in-time obligation's share for a quantity not delivered
// yet is in no period. The recognition run (T81d) reads the same
// schedule to its period's last day.
//
// The arithmetic rests on ADR-0068 §2's statements 1 and 5 about IFRS 15,
// from memory and not yet confirmed by the accounting specialist; that
// the delivery confirmation is when a point-in-time obligation is
// satisfied is the product owner's planning default (2026-11-12), which
// the person overrules per obligation with SatisfiedOn.

// PeriodAmount is what a schedule gives one fiscal period: Amount, of
// which Recognized falls on days up to the schedule's day. Days are the
// days of an over-time obligation's service period in the fiscal period,
// 0 otherwise; Delivered is the quantity confirmed as delivered in it of
// a point-in-time obligation scheduled by its deliveries, nil otherwise.
type PeriodAmount struct {
	Period     ledger.FiscalPeriod
	Amount     money.Minor
	Recognized money.Minor
	Days       int32
	Delivered  *big.Rat
}

// ObligationSchedule is an obligation with its periods, earliest first.
// Recognized, Planned and Unscheduled add up to its allocated amount.
// Unit is its order item's unit of measure.
type ObligationSchedule struct {
	Obligation
	Unit        string
	Periods     []PeriodAmount
	Recognized  money.Minor
	Planned     money.Minor
	Unscheduled money.Minor
}

// Schedule is a contract's recognition schedule to the day AsOf. Periods
// are the obligations' periods added up, earliest first; Recognized,
// Planned and Unscheduled add up to the transaction price.
type Schedule struct {
	Contract    Contract
	AsOf        time.Time
	Obligations []ObligationSchedule
	Periods     []PeriodAmount
	Recognized  money.Minor
	Planned     money.Minor
	Unscheduled money.Minor
}

// notActive refuses the schedule of a contract that is still a draft: it
// has no allocated amounts to schedule.
func notActive() *problem.Problem {
	return problem.Conflict("revenue.invalid_status", "The revenue contract is a draft; a recognition schedule is of an active contract, whose transaction price is allocated.")
}

// calendar finds the fiscal periods of one company's days, reading each
// fiscal year from the ledger once.
type calendar struct {
	source  Calendar
	company tenancy.Entity
	periods []ledger.FiscalPeriod
}

// of is the regular posting period the day falls in. A day in no fiscal
// year set up for the company is refused for the obligation that needs
// it (422 revenue.no_fiscal_period); what names the day's part in the
// obligation.
func (c *calendar) of(ctx context.Context, day time.Time, o Obligation, what string) (ledger.FiscalPeriod, error) {
	for _, p := range c.periods {
		if !day.Before(p.Start) && !day.After(p.End) {
			return p, nil
		}
	}
	p, err := c.source.Period(ctx, c.company.ID, day)
	if err != nil {
		if problem.From(err).Code == "ledger.no_fiscal_period" {
			return ledger.FiscalPeriod{}, problem.Unprocessable("revenue.no_fiscal_period", fmt.Sprintf("Obligation %d (%s): %s, %s, lies in no fiscal year set up for company %s, "+
				"so its revenue has no fiscal period. A fiscal year is set up by a number range for it in the configuration.",
				o.No, o.Description, what, day.Format(time.DateOnly), c.company.Code))
		}
		return ledger.FiscalPeriod{}, fmt.Errorf("revenue: fiscal period of %s in company %s: %w", day.Format(time.DateOnly), c.company.Code, err)
	}
	year, err := c.source.Periods(ctx, c.company.ID, p.FiscalYear)
	if err != nil {
		return ledger.FiscalPeriod{}, fmt.Errorf("revenue: periods of fiscal year %d in company %s: %w", p.FiscalYear, c.company.Code, err)
	}
	c.periods = append(c.periods, year...)
	return p, nil
}

// daysBetween counts the days from the first to the last, both included;
// both are dates at midnight UTC.
func daysBetween(first, last time.Time) int64 {
	return int64(last.Sub(first)/(24*time.Hour)) + 1
}

// share is amount × part ÷ whole, exactly.
func share(amount money.Minor, part, whole int64) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(amount)), big.NewInt(part)), big.NewInt(whole))
}

// straightLine spreads an over-time obligation's allocated amount over
// the days of its service period, both included (ADR-0068 §8; statement
// 5): each fiscal period takes the amount × its days of the service
// period ÷ the service period's days, and money.Allocate rounds the
// shares so that they add up to the amount to the minor unit, a unit
// left going to the largest remainder, the earlier period first. Of a
// period under way on asOf, the recognized part is the period's amount ×
// its service days so far ÷ its service days, rounded the same way
// against the rest.
func (c *calendar) straightLine(ctx context.Context, o Obligation, asOf time.Time) ([]PeriodAmount, error) {
	var rows []PeriodAmount
	var starts []time.Time
	total := daysBetween(*o.ServiceFrom, *o.ServiceTo)
	var exact []*big.Rat
	for day := *o.ServiceFrom; !day.After(*o.ServiceTo); {
		p, err := c.of(ctx, day, o, "a day of its service period")
		if err != nil {
			return nil, err
		}
		last := p.End
		if last.After(*o.ServiceTo) {
			last = *o.ServiceTo
		}
		n := daysBetween(day, last)
		rows, starts = append(rows, PeriodAmount{Period: p, Days: int32(n)}), append(starts, day)
		exact = append(exact, share(*o.Allocated, n, total))
		day = last.AddDate(0, 0, 1)
	}
	// The exact shares add up to the allocated amount, an amount: cannot fail.
	amounts, _ := money.Allocate(exact)
	for i := range rows {
		rows[i].Amount = amounts[i]
		n := int64(rows[i].Days)
		elapsed := min(max(daysBetween(starts[i], asOf), 0), n)
		// The two parts add up to the period's amount, an amount: cannot fail.
		parts, _ := money.Allocate([]*big.Rat{share(amounts[i], elapsed, n), share(amounts[i], n-elapsed, n)})
		rows[i].Recognized = parts[0]
	}
	return rows, nil
}

// byDeliveries schedules a point-in-time obligation's allocated amount in
// the fiscal periods of its order item's delivery confirmations, in
// proportion to the quantity delivered (ADR-0068 §8; statement 1, with
// the delivery confirmation as the default evidence that control
// passed): after each delivery what is scheduled so far is the amount ×
// the quantity delivered so far ÷ the quantity ordered, rounded to the
// minor unit, and the delivery takes what that adds, as billing shares
// out an item's value (sales, ADR-0067 §9). The delivery that completes
// the item so takes the remainder, and what is not delivered is in no
// period.
func (c *calendar) byDeliveries(ctx context.Context, o Obligation, item sales.OrderItem, asOf time.Time) ([]PeriodAmount, error) {
	var rows []PeriodAmount
	ordered, delivered, scheduled := money.ParseRat(item.Quantity), new(big.Rat), money.Minor(0)
	for _, d := range item.Deliveries {
		p, err := c.of(ctx, d.Date, o, "a delivery confirmation of its order item")
		if err != nil {
			return nil, err
		}
		quantity := money.ParseRat(d.Quantity)
		delivered.Add(delivered, quantity)
		part := new(big.Rat).Quo(delivered, ordered)
		// What is delivered is at most what was ordered (sales_order_item_quantities), so this is at most the allocated amount: in range.
		upTo, _ := money.Round(part.Mul(part, new(big.Rat).SetInt64(int64(*o.Allocated))), 0)
		amount := upTo - scheduled
		scheduled = upTo
		if n := len(rows); n == 0 || rows[n-1].Period != p {
			rows = append(rows, PeriodAmount{Period: p, Delivered: new(big.Rat)})
		}
		row := &rows[len(rows)-1]
		row.Amount += amount
		row.Delivered.Add(row.Delivered, quantity)
		if !d.Date.After(asOf) {
			row.Recognized += amount
		}
	}
	return rows, nil
}

// onDay schedules a point-in-time obligation's whole allocated amount on
// the day the person named on it (SatisfiedOn), whatever was delivered.
func (c *calendar) onDay(ctx context.Context, o Obligation, asOf time.Time) ([]PeriodAmount, error) {
	p, err := c.of(ctx, *o.SatisfiedOn, o, "the day it is satisfied on")
	if err != nil {
		return nil, err
	}
	row := PeriodAmount{Period: p, Amount: *o.Allocated}
	if !o.SatisfiedOn.After(asOf) {
		row.Recognized = row.Amount
	}
	return []PeriodAmount{row}, nil
}

// obligation schedules one obligation of an active contract by its
// timing, with its sums.
func (c *calendar) obligation(ctx context.Context, o Obligation, item sales.OrderItem, asOf time.Time) (ObligationSchedule, error) {
	out := ObligationSchedule{Obligation: o, Unit: item.Unit}
	var err error
	switch {
	case o.Timing == OverTime:
		out.Periods, err = c.straightLine(ctx, o, asOf)
	case o.SatisfiedOn != nil:
		out.Periods, err = c.onDay(ctx, o, asOf)
	default:
		out.Periods, err = c.byDeliveries(ctx, o, item, asOf)
	}
	if err != nil {
		return ObligationSchedule{}, err
	}
	scheduled := money.Minor(0)
	for _, p := range out.Periods {
		scheduled += p.Amount
		out.Recognized += p.Recognized
	}
	out.Planned, out.Unscheduled = scheduled-out.Recognized, *o.Allocated-scheduled
	return out, nil
}

// scheduleOf is the active contract's schedule to the day asOf, from its
// sales order's deliveries: each obligation's periods, and the periods'
// and the contract's sums. The amounts are parts of the transaction
// price, an amount, so their sums are in range.
func (c *calendar) scheduleOf(ctx context.Context, k Contract, order sales.Order, asOf time.Time) (Schedule, error) {
	items := make(map[int32]sales.OrderItem, len(order.Items))
	for _, it := range order.Items {
		items[it.ItemNo] = it
	}
	out := Schedule{Contract: k, AsOf: asOf, Obligations: make([]ObligationSchedule, len(k.Obligations))}
	for i, o := range k.Obligations {
		s, err := c.obligation(ctx, o, items[o.OrderItemNo], asOf)
		if err != nil {
			return Schedule{}, err
		}
		out.Obligations[i] = s
		out.Recognized, out.Planned, out.Unscheduled = out.Recognized+s.Recognized, out.Planned+s.Planned, out.Unscheduled+s.Unscheduled
		out.Periods = added(out.Periods, s.Periods)
	}
	return out, nil
}

// added is the totals with the periods' amounts added, earliest first.
// Both lists are in the periods' order.
func added(totals, periods []PeriodAmount) []PeriodAmount {
	var out []PeriodAmount
	i, j := 0, 0
	for i < len(totals) || j < len(periods) {
		switch {
		case j == len(periods) || (i < len(totals) && totals[i].Period.Start.Before(periods[j].Period.Start)):
			out = append(out, totals[i])
			i++
		case i == len(totals) || periods[j].Period.Start.Before(totals[i].Period.Start):
			out = append(out, PeriodAmount{Period: periods[j].Period, Amount: periods[j].Amount, Recognized: periods[j].Recognized})
			j++
		default:
			t := totals[i]
			t.Amount, t.Recognized = t.Amount+periods[j].Amount, t.Recognized+periods[j].Recognized
			out = append(out, t)
			i, j = i+1, j+1
		}
	}
	return out
}

// today is the company's date now in its time zone (product owner,
// 2026-10-31), never UTC's for a company that sets one.
func (e *Endpoints) today(ctx context.Context, company tenancy.Entity) (time.Time, error) {
	day, err := config.DocumentDay(ctx, e.Settings, company.Code, clock.Now(e.Now))
	if err != nil {
		return time.Time{}, fmt.Errorf("revenue: today in company %s: %w", company.Code, err)
	}
	return day.Date, nil
}

// GetSchedule returns the recognition schedule of an active contract of a
// company the caller may act in, to the company's date today, with the
// contract's company (ADR-0068 §8). A draft is refused (409
// revenue.invalid_status).
func (e *Endpoints) GetSchedule(ctx context.Context, id uuid.UUID) (Schedule, error) {
	k, err := GetContract(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	if k.Status != Active {
		return Schedule{}, notActive()
	}
	le, _, err := e.LegalEntities.Entity(ctx, k.LegalEntityID) // a stored contract's legal entity exists
	if err != nil {
		return Schedule{}, fmt.Errorf("revenue: company of contract %s: %w", id, err)
	}
	asOf, err := e.today(ctx, le)
	if err != nil {
		return Schedule{}, err
	}
	order, _, err := e.SalesOrders.Order(ctx, k.SalesOrderID) // a stored contract's sales order exists
	if err != nil {
		return Schedule{}, fmt.Errorf("revenue: sales order of contract %s: %w", id, err)
	}
	cal := &calendar{source: e.Calendar, company: le}
	return cal.scheduleOf(ctx, k, order, asOf)
}
