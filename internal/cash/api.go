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
	out := gen.CashPosition{LegalEntityID: p.LegalEntity, ReadDate: p.ReadDate, From: p.From, Days: int32(p.Days), Banks: make([]gen.CashPositionBank, len(p.Banks)),
		Currencies: make([]gen.CashPositionCurrency, len(p.Currencies)), Converted: convertedToAPI(p.Converted)}
	for i, b := range p.Banks {
		out.Banks[i] = bankToAPI(b)
	}
	for i, c := range p.Currencies {
		amount := amounts(c.Currency, c.Exponent)
		out.Currencies[i] = gen.CashPositionCurrency{Currency: c.Currency, CreditLine: optAmount(c.CreditLine, amount), Days: daysToAPI(c.Days, amount)}
	}
	return &gen.CashPositionHeaders{Response: out}, nil
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
	out := gen.CashPositionBank{HouseBankID: b.ID, Code: b.Code, AccountLast4: b.Last4, Currency: b.Currency,
		Opening:    gen.CashPositionOpening{Balance: amount(b.Opening.Balance), Stale: b.Opening.Stale, Gap: b.Opening.Gap},
		CreditLine: optAmount(b.CreditLine, amount), Days: daysToAPI(b.Days, amount)}
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
			Flows: make([]gen.CashFlowTotal, len(d.Flows))}
		for j, f := range d.Flows {
			out[i].Flows[j] = gen.CashFlowTotal{Source: f.Source, Certainty: f.Certainty, Late: f.Late, Amount: amount(f.Amount), Count: int32(f.Count)}
		}
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
