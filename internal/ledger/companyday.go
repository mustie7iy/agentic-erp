package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Settings reads the active configuration (configuration context): the
// time zone each company's date is judged in (companies[].time_zone,
// ADR-0051 as built T72n1).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// companyDay is a company's date at an instant, in its time zone, and that
// zone: the day a center change is judged on (T72n4).
type companyDay struct {
	day  time.Time
	zone *time.Location
	// hidden: the caller cannot access the company, so a refusal does not
	// name its date or zone (visible).
	hidden bool
}

// dayOfCompany is the company's date at instant now in the zone the
// document gives it (UTC when it sets none), as config.(*Company).Location
// reads it.
func dayOfCompany(now time.Time, co config.Company) (companyDay, error) {
	loc, err := co.Location()
	if err != nil {
		return companyDay{}, fmt.Errorf("ledger: company %s: %w", co.Code, err)
	}
	return companyDay{day: clock.DayIn(now, loc), zone: loc}, nil
}

// text names the date and the zone it is the company's date in, as a
// refusal or finding gives them: the consumer sees which day "today" was.
func (d companyDay) text() string {
	if d.hidden {
		return "the date of a company you cannot access"
	}
	return dateText(d.day) + " (the company's date in " + d.zone.String() + ")"
}

// companyDays tells the date now of the tenant's companies in their time
// zones (product owner 2026-10-31, T72n4): a center change takes effect
// from the company's date, not UTC's, so whether a default center stays
// usable is judged on it. A stored zone the binary cannot load fails the
// change rather than judging it in UTC.
type companyDays struct {
	now      func() time.Time
	entities LegalEntities
	settings Settings
}

// of is legal entity le's date now in its company's time zone.
func (d companyDays) of(ctx context.Context, le uuid.UUID) (companyDay, error) {
	if d.entities == nil || d.settings == nil {
		return companyDay{}, errors.New("ledger: no legal entities or configuration to read a company's time zone from")
	}
	e, found, err := d.entities.Entity(ctx, le)
	if err != nil {
		return companyDay{}, fmt.Errorf("ledger: legal entity %s: %w", le, err)
	}
	if !found {
		return companyDay{}, fmt.Errorf("ledger: legal entity %s not found", le)
	}
	doc, _, err := d.settings.Document(ctx)
	if err != nil {
		return companyDay{}, fmt.Errorf("ledger: time zone of company %s: %w", e.Code, err)
	}
	loc, err := doc.CompanyLocation(e.Code)
	if err != nil {
		return companyDay{}, fmt.Errorf("ledger: company %s: %w", e.Code, err)
	}
	return companyDay{day: clock.DayIn(clock.Now(d.now), loc), zone: loc}, nil
}

// visible is legal entity le's date now as a refusal to the caller may
// give it: hidden when the caller cannot access the company, as when a
// profit center is the default of a company the requester has no access
// to, or an approver decides a change of a cost center in such a company
// (the zone and date are that company's attributes).
func (d companyDays) visible(ctx context.Context, le uuid.UUID) (companyDay, error) {
	day, err := d.of(ctx, le)
	if err != nil {
		return day, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return day, fmt.Errorf("ledger: company access: %w", err)
	}
	day.hidden = !access.Allows(le)
	return day, nil
}
