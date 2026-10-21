package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Cash is the cash context as the API wires it (ADR-0051 §2): a leaf that
// reads the tenancy, payments and ledger contexts through the adapters
// below, so none of them imports it and it imports none of them.
func Cash(cfg Config) *cash.Endpoints {
	return &cash.Endpoints{LegalEntities: cashCompanies{}, Settings: configuration.Settings{},
		Payments: cashPayments{position: payments.CashPosition{Accounts: ledger.Accounts{}}}, Ledger: cashLedger{}, Now: cfg.Now}
}

// cashCompanies gives the cash context the legal entity's code and currency.
type cashCompanies struct{}

func (cashCompanies) Company(ctx context.Context, id uuid.UUID) (cash.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return cash.Company{Code: le.Code, Currency: le.LocalCurrency}, ok, err
}

// cashPayments gives the cash context the payments context's house banks,
// statements and payments.
type cashPayments struct{ position payments.CashPosition }

func (c cashPayments) Banks(ctx context.Context, legalEntity uuid.UUID, before time.Time) ([]cash.Bank, error) {
	banks, err := c.position.Banks(ctx, legalEntity, before)
	out := make([]cash.Bank, len(banks))
	for i, b := range banks {
		out[i] = cash.Bank{ID: b.ID, Code: b.Code, Last4: b.Last4, Currency: b.Currency, Exponent: b.Exponent, ClearingAccountID: b.ClearingAccountID}
		if s := b.Statement; s != nil {
			f := s.Figures
			out[i].Statement = &cash.Statement{Date: f.AsOf, Closing: s.Closing, Gap: s.Gap, Book: cash.Book{Ledger: f.Ledger, Unexplained: f.Unexplained(),
				NotPosted: f.InTransit, Excluded: f.Excluded, PostedLater: f.PostedLater}}
		}
	}
	return out, err
}

func (c cashPayments) Released(ctx context.Context, ids []uuid.UUID) ([]cash.Payment, error) {
	ps, err := c.position.Released(ctx, ids)
	return cashPaymentsOf(ps), err
}

func (c cashPayments) Pending(ctx context.Context, legalEntity uuid.UUID) ([]cash.Payment, error) {
	ps, err := c.position.Pending(ctx, legalEntity)
	return cashPaymentsOf(ps), err
}

func cashPaymentsOf(ps []payments.CashPayment) []cash.Payment {
	out := make([]cash.Payment, len(ps))
	for i, p := range ps {
		out[i] = cash.Payment{ID: p.ID, HouseBankID: p.HouseBankID, Method: p.Method, RunDate: p.RunDate, Amount: p.Amount, Currency: p.Currency,
			MatchedOn: p.MatchedOn}
	}
	return out
}

// cashLedger gives the cash context the ledger's open lines and rates.
type cashLedger struct{}

func (cashLedger) OpenLines(ctx context.Context, legalEntity uuid.UUID, accounts []uuid.UUID) ([]cash.OpenLine, error) {
	lines, err := ledger.Accounts{}.OpenLines(ctx, legalEntity, accounts)
	out := make([]cash.OpenLine, len(lines))
	for i, l := range lines {
		out[i] = cash.OpenLine{AccountID: l.AccountID, Currency: l.Currency, Amount: l.Amount, SourceContext: l.SourceContext, SourceID: l.SourceID}
	}
	return out, err
}

func (cashLedger) Rate(ctx context.Context, rateType, from, to string, date time.Time) (cash.Rate, bool, error) {
	c, found, err := ledger.Rates{}.OfType(ctx, rateType, from, to, date)
	return cash.Rate{Rate: c.Rate, ValidFrom: c.ValidFrom, Convert: c.Convert}, found, err
}
