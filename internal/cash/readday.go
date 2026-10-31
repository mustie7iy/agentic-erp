package cash

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// A memo record's status is judged on its legal entity's date at the read,
// in its company's time zone (ADR-0051 as built, T72n2), as the cash
// position's read date is (T72n1): a record whose last day is 17 April is
// still active at 22:00 that day in Toronto, when UTC is on the 18th, and
// the position counting it and the list showing it agree.

// readDay is a company's date at the read and the IANA zone it is in.
type readDay struct {
	Date time.Time
	Zone string
}

// companyDay is company code's date at instant now in its time zone: UTC
// when it has none, or the document has no such company. A stored zone the
// binary cannot load fails rather than reading the date in UTC.
func companyDay(doc *config.Config, code string, now time.Time) (readDay, error) {
	loc := time.UTC
	if co := doc.Company(code); co != nil {
		var err error
		if loc, err = co.Location(); err != nil {
			return readDay{}, fmt.Errorf("cash: company %s: %w", code, err)
		}
	}
	return readDay{Date: clock.DayIn(now, loc), Zone: loc.String()}, nil
}

// readDays are the legal entities' dates at one read: those whose company
// has a time zone by legal entity, every other one UTC's.
type readDays struct {
	byEntity map[uuid.UUID]readDay
	utc      readDay
}

// of is the legal entity's date at the read.
func (r readDays) of(legalEntity uuid.UUID) readDay {
	if d, ok := r.byEntity[legalEntity]; ok {
		return d
	}
	return r.utc
}

// args are the legal entities with a date of their own and those dates,
// in the same order, for a query to judge each record by its entity's
// date and the rest by UTC's.
func (r readDays) args() ([]uuid.UUID, []time.Time) {
	ids, dates := make([]uuid.UUID, 0, len(r.byEntity)), make([]time.Time, 0, len(r.byEntity))
	for id, d := range r.byEntity {
		ids, dates = append(ids, id), append(dates, d.Date)
	}
	return ids, dates
}

// readDaysOf reads the dates, at one instant, of the legal entities a read
// may show: only's when it is given, else those of every company of the
// configuration with a time zone, found by code (one with no legal entity
// yet has no records). A tenant without zones reads no legal entity.
func (e *Endpoints) readDaysOf(ctx context.Context, only *uuid.UUID) (readDays, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return readDays{}, fmt.Errorf("cash: configuration: %w", err)
	}
	now := clock.Now(e.Now)
	r := readDays{byEntity: map[uuid.UUID]readDay{}, utc: readDay{Date: clock.Day(now), Zone: time.UTC.String()}}
	if only != nil {
		// One that does not exist has no code, so no company and UTC's
		// date; it has no records to judge either.
		co, _, err := e.LegalEntities.Company(ctx, *only)
		if err != nil {
			return readDays{}, fmt.Errorf("cash: legal entity %s: %w", *only, err)
		}
		if r.byEntity[*only], err = companyDay(doc, co.Code, now); err != nil {
			return readDays{}, err
		}
		return r, nil
	}
	for i := range doc.Companies {
		co := &doc.Companies[i]
		if co.TimeZone == "" {
			continue
		}
		id, ok, err := e.LegalEntities.ByCode(ctx, co.Code)
		if err != nil {
			return readDays{}, fmt.Errorf("cash: legal entity of company %s: %w", co.Code, err)
		}
		if !ok {
			continue
		}
		if r.byEntity[id], err = companyDay(doc, co.Code, now); err != nil {
			return readDays{}, err
		}
	}
	return r, nil
}
