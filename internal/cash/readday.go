package cash

import (
	"context"
	"errors"
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

// companyDay is company code's date at instant now in its time zone
// (config.CompanyDay, the lookup every company date shares): UTC when it
// has none, or the document has no such company. A stored zone the binary
// cannot load fails rather than reading the date in UTC.
func companyDay(doc *config.Config, code string, now time.Time) (readDay, error) {
	d, err := doc.CompanyDay(code, now)
	if err != nil {
		return readDay{}, fmt.Errorf("cash: %w", err)
	}
	return readDay{Date: d.Date, Zone: d.Zone.String()}, nil
}

// entityDay is legal entity le's date at instant now in its company's time
// zone (config.EntityDay): a legal entity the directory does not know is
// refused, as every company date refuses it, never read on UTC's date.
func (e *Endpoints) entityDay(ctx context.Context, doc *config.Config, le uuid.UUID, now time.Time) (readDay, error) {
	d, err := doc.EntityDay(ctx, func(ctx context.Context, id uuid.UUID) (string, bool, error) {
		co, found, err := e.LegalEntities.Company(ctx, id)
		return co.Code, found, err
	}, le, now)
	if err != nil {
		return readDay{}, fmt.Errorf("cash: %w", err)
	}
	return readDay{Date: d.Date, Zone: d.Zone.String()}, nil
}

// recordDay is the date a memo record of legal entity le is judged on at
// a read of it alone: a stored record's legal entity the directory does
// not know is refused (entityDay).
func (e *Endpoints) recordDay(ctx context.Context, le uuid.UUID) (readDay, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return readDay{}, fmt.Errorf("cash: configuration: %w", err)
	}
	return e.entityDay(ctx, doc, le, clock.Now(e.Now))
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

// readDaysOf reads the dates, at one instant, of the legal entities a list
// may show: only's when the list is filtered by it, else those of every
// company of the configuration with a time zone, found by code (one with
// no legal entity yet has no records). A tenant without zones reads no
// legal entity. A filter naming a legal entity the directory does not
// know reads no date for it: it matches no record, so no record is judged
// on a date, and the list answers an empty page, as it does for a legal
// entity the caller cannot access, so whether one exists does not leak
// (tenancy.Accessible's rule). Every read that judges a record on its
// legal entity's date refuses such a legal entity (entityDay).
func (e *Endpoints) readDaysOf(ctx context.Context, only *uuid.UUID) (readDays, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return readDays{}, fmt.Errorf("cash: configuration: %w", err)
	}
	now := clock.Now(e.Now)
	r := readDays{byEntity: map[uuid.UUID]readDay{}, utc: readDay{Date: clock.Day(now), Zone: time.UTC.String()}}
	if only != nil {
		d, err := e.entityDay(ctx, doc, *only, now)
		if errors.Is(err, config.ErrNoLegalEntity) {
			return r, nil
		}
		if err != nil {
			return readDays{}, err
		}
		r.byEntity[*only] = d
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
