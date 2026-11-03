package cash

import (
	"context"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// GetCashPosition implements GET /v1/cash-position.
func (e *Endpoints) GetCashPosition(ctx context.Context, params gen.GetCashPositionParams) (gen.GetCashPositionRes, error) {
	p, err := e.Position(ctx, Query{LegalEntity: params.LegalEntityID, From: apitypes.Ptr[time.Time](params.From), Days: int(params.Days.Or(DefaultDays)),
		Display: params.DisplayCurrency.Value})
	if err != nil {
		return nil, err
	}
	out := gen.CashPosition{LegalEntityID: p.LegalEntity, ReadDate: p.ReadDate, TimeZone: p.TimeZone, From: p.From, Days: int32(p.Days), Banks: make([]gen.CashPositionBank, len(p.Banks)),
		Currencies: make([]gen.CashPositionCurrency, len(p.Currencies)), Converted: convertedToAPI(p.Converted)}
	for i, b := range p.Banks {
		out.Banks[i] = bankToAPI(b)
	}
	for i, c := range p.Currencies {
		amount := amounts(c.Currency, c.Exponent)
		out.Currencies[i] = gen.CashPositionCurrency{Currency: c.Currency, CreditLine: optAmount(c.CreditLine, amount), Days: daysToAPI(c.Days, amount)}
	}
	return &out, nil
}

// amounts shows minor units of a currency.
func amounts(currency string, exp int) func(money.Minor) gen.Money {
	return func(m money.Minor) gen.Money { return apitypes.Money(m, currency, exp) }
}

func optAmount(m *money.Minor, amount func(money.Minor) gen.Money) gen.OptMoney {
	if m == nil {
		return gen.OptMoney{}
	}
	return gen.NewOptMoney(amount(*m))
}

func bankToAPI(b BankPosition) gen.CashPositionBank {
	amount := amounts(b.Currency, b.Exponent)
	out := gen.CashPositionBank{HouseBankID: b.ID, HouseBankCode: b.Code, AccountLast4: b.Last4, Currency: b.Currency,
		Opening:    gen.CashPositionOpening{Balance: amount(b.Opening.Balance), Stale: b.Opening.Stale, Gap: b.Opening.Gap},
		CreditLine: optAmount(b.CreditLine, amount), Days: daysToAPI(b.Days, amount)}
	// The contract deprecates code for house_bank_code; /v1 gives it until
	// its sunset (ADR-0038 As built). Both linters are told: the standalone
	// staticcheck of ci-local reads lint:ignore, golangci-lint reads nolint.
	//lint:ignore SA1019 code is given until its sunset
	out.Code = b.Code //nolint:staticcheck // code is given until its sunset (ADR-0038 As built)
	if b.Opening.Date != nil {
		out.Opening.StatementDate.SetTo(*b.Opening.Date)
	}
	if s := b.Statement; s != nil {
		f := gen.BankReconciliationFigures{AsOf: s.Date, ClosingBalance: amount(s.Closing), LedgerBalance: optAmount(s.Book.Ledger, amount),
			Excluded: amount(s.Book.Excluded), InTransit: amount(s.Book.NotPosted), PostedLater: amount(s.Book.PostedLater),
			Unexplained: optAmount(s.Book.Unexplained, amount)}
		out.BookBalance.SetTo(gen.CashPositionBookBalance{Figures: f, PaymentsInTransit: amount(b.InTransit)})
	}
	return out
}

func daysToAPI(days []Day, amount func(money.Minor) gen.Money) []gen.CashPositionDay {
	out := make([]gen.CashPositionDay, len(days))
	for i, d := range days {
		out[i] = gen.CashPositionDay{Date: d.Date, Opening: amount(d.Opening), Closing: amount(d.Closing), Available: amount(d.Available),
			Flows: flowsToAPI(d.Flows, amount)}
	}
	return out
}

func flowsToAPI(flows []FlowTotal, amount func(money.Minor) gen.Money) []gen.CashFlowTotal {
	out := make([]gen.CashFlowTotal, len(flows))
	for i, f := range flows {
		out[i] = gen.CashFlowTotal{Source: f.Source, Certainty: f.Certainty, Late: f.Late, Amount: amount(f.Amount), Count: int32(f.Count)}
	}
	return out
}

func convertedToAPI(c Converted) gen.CashPositionConverted {
	amount := amounts(c.Currency, c.Exponent)
	out := gen.CashPositionConverted{Currency: c.Currency, Rates: make([]gen.CashPositionRate, len(c.Rates)), IncompleteCurrencies: c.Incomplete,
		Days: make([]gen.CashPositionTotal, len(c.Days))}
	for i, r := range c.Rates {
		out.Rates[i] = gen.CashPositionRate{Currency: r.Currency, RateType: r.RateType, Rate: gen.Decimal(r.Rate), ValidFrom: r.ValidFrom, Stale: r.Stale}
	}
	for i, d := range c.Days {
		out.Days[i] = gen.CashPositionTotal{Date: d.Date, Opening: amount(d.Opening), Closing: amount(d.Closing), Available: amount(d.Available)}
	}
	return out
}

// GetLiquidityForecast implements GET /v1/liquidity-forecast.
func (e *Endpoints) GetLiquidityForecast(ctx context.Context, params gen.GetLiquidityForecastParams) (gen.GetLiquidityForecastRes, error) {
	f, err := e.Forecast(ctx, ForecastQuery{LegalEntity: params.LegalEntityID, MinCertainty: params.MinCertainty.Value,
		IncludeBlocked: params.IncludeBlocked.Value})
	if err != nil {
		return nil, err
	}
	out := gen.LiquidityForecast{LegalEntityID: f.LegalEntity, ReadDate: f.ReadDate, TimeZone: f.TimeZone, MinCertainty: f.MinCertainty, IncludeBlocked: f.IncludeBlocked,
		Buckets: make([]gen.LiquidityForecastBucket, len(f.Buckets)), Currencies: make([]gen.LiquidityForecastCurrency, len(f.Currencies))}
	for i, b := range f.Buckets {
		out.Buckets[i] = gen.LiquidityForecastBucket{Grain: b.Grain, Start: b.Start, End: b.End}
	}
	for i, c := range f.Currencies {
		amount := amounts(c.Currency, c.Exponent)
		cur := gen.LiquidityForecastCurrency{Currency: c.Currency, Buckets: make([]gen.LiquidityForecastBucketFlows, len(c.Buckets)), Total: amount(c.Total),
			Beyond:  gen.LiquidityForecastBeyond{Amount: amount(c.Beyond.Amount), Count: int32(c.Beyond.Count)},
			Blocked: gen.LiquidityForecastBlocked{Amount: amount(c.Blocked.Amount), Count: int32(c.Blocked.Count)}}
		for j, b := range c.Buckets {
			cur.Buckets[j] = gen.LiquidityForecastBucketFlows{Start: f.Buckets[j].Start, Flows: flowsToAPI(b.Flows, amount), Total: amount(b.Total)}
		}
		cur.Items = itemsToAPI(c.Items, amount)
		out.Currencies[i] = cur
	}
	return &out, nil
}

// itemsToAPI shows a currency's rows by liquidity item.
func itemsToAPI(rows []ItemRow, amount func(money.Minor) gen.Money) []gen.LiquidityForecastItem {
	each := func(ms []money.Minor) []gen.Money {
		out := make([]gen.Money, len(ms))
		for i, m := range ms {
			out[i] = amount(m)
		}
		return out
	}
	out := make([]gen.LiquidityForecastItem, len(rows))
	for i, r := range rows {
		out[i] = gen.LiquidityForecastItem{LiquidityItem: apitypes.OptText(r.Item), Name: apitypes.OptText(r.Name), Parent: apitypes.OptText(r.Parent),
			Level: int32(r.Level), Buckets: each(r.Buckets), Total: amount(r.Total), Beyond: amount(r.Beyond), Flows: make([]gen.LiquidityForecastItemFlows, len(r.Flows))}
		for j, fl := range r.Flows {
			out[i].Flows[j] = gen.LiquidityForecastItemFlows{Source: fl.Source, Certainty: fl.Certainty, Buckets: each(fl.Buckets), Total: amount(fl.Total),
				Beyond: amount(fl.Beyond)}
		}
	}
	return out
}
