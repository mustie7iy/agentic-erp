package cash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The group liquidity forecast (ADR-0051 §9, §10, §15; T53e5) is the
// forecast across the companies the caller may act in, summed in the
// tenant's group currency. It is built from each company's own forecast,
// read and converted exactly as getLiquidityForecast reads and converts it
// (readForecastOf, byCurrency, convertForecast), only placed in the
// group's buckets: so the group's converted figures are the sum of what
// each company's forecast converted into the group currency gives, and no
// second conversion path exists.

// GroupForecastQuery is what a group forecast covers: as ForecastQuery,
// without a legal entity (every one the caller may act in) or a display
// currency (the group's).
type GroupForecastQuery struct {
	MinCertainty   string
	IncludeBlocked bool
	Grain          string
	To             *time.Time
}

// GroupForecast is the forecast across the caller's companies: each
// currency summed over them, never converted, and the group currency's
// total.
type GroupForecast struct {
	ReadDate       time.Time // the earliest of the companies' read dates
	MinCertainty   string
	IncludeBlocked bool
	Companies      []GroupCompany // by company code
	Buckets        []Bucket
	// Currencies are the companies' figures per currency, by currency
	// code, without rows by liquidity item (those stay per company).
	Currencies []ForecastCurrency
	// Converted is the companies' converted figures summed: its Rates are
	// empty, each company's being in Companies, and Incomplete lists the
	// currencies any company has no rate for.
	Converted ForecastConverted
}

// GroupCompany is a company the group forecast sums: its read date and
// zone, and the rates its currencies were converted at and those it has
// none for.
type GroupCompany struct {
	LegalEntity uuid.UUID
	Code, Name  string
	ReadDate    time.Time
	TimeZone    string
	Rates       []CurrencyRate
	Incomplete  []string
}

// member is a legal entity the caller may act in, with its read date.
type member struct {
	Entity
	read time.Time
}

// GroupForecast computes the liquidity forecast across the legal entities
// the caller may act in. One outside its access is never read, so nothing
// of it reaches the answer; with none, the forecast is empty from today's
// date in UTC.
func (e *Endpoints) GroupForecast(ctx context.Context, q GroupForecastQuery) (GroupForecast, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return GroupForecast{}, err
	}
	cut, _, err := certaintyCut(q.MinCertainty)
	if err != nil {
		return GroupForecast{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return GroupForecast{}, fmt.Errorf("cash: configuration: %w", err)
	}
	members, err := e.members(ctx, doc)
	if err != nil {
		return GroupForecast{}, err
	}
	g, err := e.groupOf(ctx, tx, doc, members, q)
	if err != nil {
		return GroupForecast{}, err
	}
	g.forecast.MinCertainty = cut
	for _, m := range members {
		if err := e.addCompany(ctx, tx, &g, m, q); err != nil {
			return GroupForecast{}, err
		}
	}
	return g.finish()
}

// members are the legal entities the caller may act in, by company code,
// each with its read date.
func (e *Endpoints) members(ctx context.Context, doc *config.Config) ([]member, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	all, err := e.LegalEntities.Entities(ctx)
	if err != nil {
		return nil, fmt.Errorf("cash: legal entities: %w", err)
	}
	var out []member
	for _, le := range all {
		if !access.Allows(le.ID) {
			continue
		}
		read, _, err := e.readDate(doc, le.Code)
		if err != nil {
			return nil, err
		}
		out = append(out, member{Entity: le, read: read})
	}
	slices.SortFunc(out, func(a, b member) int { return cmp.Compare(a.Code, b.Code) })
	return out, nil
}

// group is a group forecast being summed: the forecast, its currencies by
// code, and its sums.
type group struct {
	forecast GroupForecast
	at       map[string]int
	sums     *sums
}

// groupOf is the empty group forecast: in the group currency, its buckets
// from the earliest of the members' read dates (today in UTC without a
// member), the default buckets or one grain over a range. A company's
// flows are never before its read date, so none is before the first
// bucket.
func (e *Endpoints) groupOf(ctx context.Context, tx pgx.Tx, doc *config.Config, members []member, q GroupForecastQuery) (group, error) {
	code, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return group{}, fmt.Errorf("cash: group currency: %w", err)
	}
	// The tenant's group currency references ref.currency, so it is known.
	exp, err := currency.Exponent(ctx, tx, code, nil)
	if err != nil {
		return group{}, err
	}
	start := clock.Day(clock.Now(e.Now))
	if len(members) > 0 {
		start = slices.MinFunc(members, func(a, b member) int { return a.read.Compare(b.read) }).read
	}
	bs, err := forecastBuckets(reader{doc: doc, read: start}, q.Grain, q.To)
	if err != nil {
		return group{}, err
	}
	return group{at: map[string]int{}, sums: &sums{}, forecast: GroupForecast{ReadDate: start, IncludeBlocked: q.IncludeBlocked, Companies: []GroupCompany{},
		Buckets: bs, Currencies: []ForecastCurrency{}, Converted: ForecastConverted{Currency: code, Exponent: exp, Rates: []CurrencyRate{}, Incomplete: []string{},
			Totals: make([]money.Minor, len(bs)), Closing: make([]money.Minor, len(bs))}}}, nil
}

// addCompany reads the member's forecast as its own forecast reads it, in
// the group's buckets, converts it into the group currency as its own
// forecast converts it, and adds both to the group.
func (e *Endpoints) addCompany(ctx context.Context, tx pgx.Tx, g *group, m member, q GroupForecastQuery) error {
	bs := g.forecast.Buckets
	last := bs[len(bs)-1].End
	fr, err := e.readForecastOf(ctx, tx, m.Company, m.ID, q.MinCertainty, func(reader) (time.Time, error) { return last, nil })
	if err != nil {
		return err
	}
	cs, err := fr.r.byCurrency(ctx, tx, bs, fr.flows, fr.banks, q.IncludeBlocked)
	if err != nil {
		return err
	}
	c, err := e.convertForecast(ctx, tx, fr.r, cs, g.forecast.Converted.Currency, len(bs))
	if err != nil {
		return err
	}
	if fr.r.sums.overflow {
		return errForecastTooLarge()
	}
	g.forecast.Companies = append(g.forecast.Companies, GroupCompany{LegalEntity: m.ID, Code: m.Code, Name: m.Name, ReadDate: fr.r.read, TimeZone: fr.zone,
		Rates: c.Rates, Incomplete: c.Incomplete})
	for _, fc := range cs {
		g.addCurrency(fc)
	}
	g.addConverted(c)
	return nil
}

// addCurrency adds a company's currency to the group's of its code.
func (g *group) addCurrency(c ForecastCurrency) {
	i, ok := g.at[c.Currency]
	if !ok {
		i, g.at[c.Currency] = len(g.forecast.Currencies), len(g.forecast.Currencies)
		into := ForecastCurrency{Currency: c.Currency, Exponent: c.Exponent, Buckets: make([]BucketFlows, len(c.Buckets))}
		for b := range into.Buckets {
			into.Buckets[b].Flows = []FlowTotal{}
		}
		g.forecast.Currencies = append(g.forecast.Currencies, into)
	}
	s, into := g.sums, &g.forecast.Currencies[i]
	op := &into.Opening
	op.Amount, op.Banks, op.Stale, op.Gap = s.add(op.Amount, c.Opening.Amount), op.Banks+c.Opening.Banks, op.Stale || c.Opening.Stale, op.Gap || c.Opening.Gap
	for b, bf := range c.Buckets {
		for _, t := range bf.Flows {
			into.Buckets[b].Flows = s.addTotal(into.Buckets[b].Flows, t)
		}
		into.Buckets[b].Total = s.add(into.Buckets[b].Total, bf.Total)
	}
	into.Total = s.add(into.Total, c.Total)
	into.Beyond = Beyond{Amount: s.add(into.Beyond.Amount, c.Beyond.Amount), Count: into.Beyond.Count + c.Beyond.Count}
	into.Blocked = Beyond{Amount: s.add(into.Blocked.Amount, c.Blocked.Amount), Count: into.Blocked.Count + c.Blocked.Count}
}

// addConverted adds a company's converted opening, buckets' totals and
// beyond to the group's, and the currencies it has no rate for.
func (g *group) addConverted(c ForecastConverted) {
	s, into := g.sums, &g.forecast.Converted
	into.Opening, into.Beyond = s.add(into.Opening, c.Opening), s.add(into.Beyond, c.Beyond)
	for b, t := range c.Totals {
		into.Totals[b] = s.add(into.Totals[b], t)
	}
	into.Incomplete = append(into.Incomplete, c.Incomplete...)
}

// finish orders the currencies by code and closes the buckets, each
// currency's and the converted ones, at the opening plus the totals so
// far.
func (g *group) finish() (GroupForecast, error) {
	f := g.forecast
	slices.SortFunc(f.Currencies, func(a, b ForecastCurrency) int { return cmp.Compare(a.Currency, b.Currency) })
	for i := range f.Currencies {
		g.sums.closings(&f.Currencies[i])
	}
	slices.Sort(f.Converted.Incomplete)
	f.Converted.Incomplete = slices.Compact(f.Converted.Incomplete)
	g.sums.close(&f.Converted)
	if g.sums.overflow {
		return GroupForecast{}, errForecastTooLarge()
	}
	return f, nil
}
