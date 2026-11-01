package taxfiling

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TaxItems reads the tax items a return reports (the ledger's port,
// ledger.TaxItems; ADR-0049 Alternatives).
type TaxItems interface {
	Unreported(ctx context.Context, legalEntities []uuid.UUID, start, end time.Time) ([]ledger.TaxItemGroup, error)
	UnreportedItems(ctx context.Context, legalEntities []uuid.UUID, end time.Time, sel ledger.TaxItemSelection, after *ledger.TaxItemKey,
		limit int) ([]ledger.ReturnItem, error)
}

// returnScope is what a draft return of a registration's period is
// computed from: the registration, the period, the companies it covers
// (registrant first) by code, their one local currency and the country
// pack's definition of its return type.
type returnScope struct {
	reg      Registration
	period   config.TaxPeriod
	entities []uuid.UUID
	codes    map[uuid.UUID]string
	currency string
	exp      int
	pack     *countrypack.Pack
	def      *countrypack.ReturnDefinition
}

// scope reads and checks what a draft of the registration's period is
// computed from (ADR-0049 §11): a registration the caller sees, covering
// only companies they may act in, a period of it that has ended on the
// registrant's date in its time zone, companies of one local currency,
// and the registrant's adopted pack defining its return type.
func (e *Endpoints) scope(ctx context.Context, id uuid.UUID, start time.Time) (*returnScope, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, registrationSelect+` WHERE `+visible+` AND id = $3`, access.All, access.IDs, id)
	r, err := pgx.CollectExactlyOneRow(rows, scanRegistration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem.NotFoundOf("tax registration")
	}
	if err != nil {
		return nil, fmt.Errorf("taxfiling: tax registration %s: %w", id, err)
	}
	s := &returnScope{reg: r, entities: append([]uuid.UUID{r.LegalEntityID}, r.Members...), codes: map[uuid.UUID]string{}}
	if slices.ContainsFunc(s.entities, func(le uuid.UUID) bool { return !access.Allows(le) }) {
		return nil, problem.Forbidden("tax.registration_companies",
			"A return of registration "+r.Code+" reads every company it covers, and the caller may not act in all of them.")
	}
	periods := config.TaxPeriods(r.FirstPeriodStart, r.Frequency, r.LastPeriodEnd, start)
	if len(periods) == 0 || !periods[len(periods)-1].Start.Equal(start) {
		return nil, problem.Unprocessable("tax.not_a_period", "No period of registration "+r.Code+" starts on "+start.Format(time.DateOnly)+".",
			problem.FieldError{Field: "period_start", Code: "not_a_period", Message: "the first day of one of the registration's periods (listTaxPeriods)"})
	}
	s.period = periods[len(periods)-1]
	if err := e.companies(ctx, s); err != nil {
		return nil, err
	}
	if err := e.definition(ctx, s); err != nil {
		return nil, err
	}
	if s.exp, err = currency.Exponent(ctx, tx, s.currency, nil); err != nil {
		return nil, fmt.Errorf("taxfiling: return of %s: %w", r.Code, err)
	}
	return s, nil
}

// companies names the companies the registration covers by code and
// takes their local currency, which must be one.
func (e *Endpoints) companies(ctx context.Context, s *returnScope) error {
	for _, id := range s.entities {
		le, ok, err := e.LegalEntities.Entity(ctx, id)
		if err != nil {
			return err
		}
		if !ok { // legal entities are never deleted, and Setup names only existing ones
			return fmt.Errorf("taxfiling: no legal entity %s for tax registration %s", id, s.reg.Code)
		}
		s.codes[id] = le.Code
		switch {
		case s.currency == "":
			s.currency = le.LocalCurrency
		case le.LocalCurrency != s.currency:
			return problem.Conflict("tax.return_currency_mixed", fmt.Sprintf("Registration %s covers companies in %s and %s: a return is in one currency.",
				s.reg.Code, s.currency, le.LocalCurrency))
		}
	}
	return nil
}

// definition checks the period has ended on the registrant's date and
// finds the return's definition in the pack its company adopted.
func (e *Endpoints) definition(ctx context.Context, s *returnScope) error {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return err
	}
	code := s.codes[s.reg.LegalEntityID]
	day, err := doc.CompanyDay(code, e.Now())
	if err != nil {
		return fmt.Errorf("taxfiling: return of %s: %w", s.reg.Code, err)
	}
	if !s.period.End.Before(day.Date) {
		return problem.Conflict("tax.period_not_ended", fmt.Sprintf("Period %s to %s of registration %s has not ended: today is %s in %s.",
			s.period.Start.Format(time.DateOnly), s.period.End.Format(time.DateOnly), s.reg.Code, day.Date.Format(time.DateOnly), day.Zone))
	}
	if a := doc.Pack(code); a != nil {
		if s.pack = e.Packs.Get(a.Country, a.Version); s.pack != nil {
			s.def = s.pack.Return(s.reg.ReturnType)
		}
	}
	if s.def == nil {
		return problem.Conflict("tax.return_undefined", fmt.Sprintf("The country pack company %s adopted does not define return %s (returns.yaml), "+
			"so its boxes cannot be computed.", code, s.reg.ReturnType))
	}
	return nil
}

// selection is the ledger's selection of a box's items.
func selection(s *countrypack.Selection) ledger.TaxItemSelection {
	return ledger.TaxItemSelection{Direction: s.Direction, Kinds: s.Kinds, Components: s.Components, Jurisdictions: s.Jurisdictions,
		AccountKeys: s.AccountKeys}
}

// measure is what a group of items (or one item) adds to a box: the box's
// measure of it, negated when the box shows credits positive.
func measure(g ledger.TaxItemGroup, s *countrypack.Selection) (money.Minor, error) {
	var v money.Minor
	switch s.Measure {
	case countrypack.MeasureBase:
		v = g.BaseLC
	case countrypack.MeasureTax:
		v = g.TaxLC
	case countrypack.MeasureRecoverable:
		v = g.RecoverableLC
	default: // validated: non_recoverable
		v = g.NonRecoverableLC()
	}
	if !s.Negate {
		return v, nil
	}
	return money.Diff(0, v)
}
