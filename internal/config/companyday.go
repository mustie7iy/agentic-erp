package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A company's business dates are its date in its time zone (ADR-0051 as
// built, T72n1). Every context that judges or defaults a date for a legal
// entity tells it here, so the lookup and its rule for a legal entity the
// directory does not know are one (review:batch27): the cash position and
// memo records, a payment run's discount lapse, the ledger's center
// checks, procurement's document dates and contract expiry, and payables'
// posting dates. The configuration names companies by code and knows
// nothing of legal entities, so each context hands its tenancy port in as
// an EntityCode.

// CompanyDay is a company's date at an instant, at midnight UTC as
// clock.DayIn gives it, and the zone it is the company's date in, which a
// response or refusal names beside it so a consumer knows which day
// "today" was.
type CompanyDay struct {
	Date time.Time
	Zone *time.Location
}

// EntityCode finds legal entity le's company code: found is false when
// the directory has no such legal entity. Each context adapts its own
// legal-entity port (tenancy context) to it.
type EntityCode func(ctx context.Context, le uuid.UUID) (code string, found bool, err error)

// ErrNoLegalEntity is the cause of EntityDay's refusal for a legal entity
// the directory does not know, for a caller with a different answer to it
// (errors.Is).
var ErrNoLegalEntity = errors.New("no such legal entity")

// Documents reads the active configuration (the configuration context's
// Settings, which every context's port of it is).
type Documents interface {
	Document(ctx context.Context) (*Config, int32, error)
}

// CompanyDay is company code's date at instant now in its time zone
// (CompanyLocation: UTC when the document has no such company or the
// company sets none). A stored zone the binary cannot load fails rather
// than giving UTC's date.
func (c *Config) CompanyDay(code string, now time.Time) (CompanyDay, error) {
	loc, err := c.CompanyLocation(code)
	if err != nil {
		return CompanyDay{}, fmt.Errorf("company %s: %w", code, err)
	}
	return CompanyDay{Date: clock.DayIn(now, loc), Zone: loc}, nil
}

// EntityDay is legal entity le's date at instant now in its company's
// time zone: its company code (codeOf), then CompanyDay in the active
// configuration (docs), read only once the legal entity is known.
//
// A legal entity the directory does not know is refused, never dated in
// UTC: its company, and so its zone, cannot be told, and UTC's date may
// be another day. Legal entities are never deleted and every caller
// passes one that a request or a stored row names, so the refusal reports
// broken data: 422 request.reference_not_found naming the legal entity,
// with ErrNoLegalEntity as its cause. It has no field error: the reference
// is a stored document's, not one the request gives.
func EntityDay(ctx context.Context, docs Documents, codeOf EntityCode, le uuid.UUID, now time.Time) (CompanyDay, error) {
	code, found, err := codeOf(ctx, le)
	if err == nil && !found {
		err = problem.MissingReference("Legal entity " + le.String() +
			" does not exist, so its company's date, which this is judged on, cannot be told.").WithCause(ErrNoLegalEntity)
	}
	if err != nil {
		return CompanyDay{}, fmt.Errorf("legal entity %s: %w", le, err)
	}
	return DocumentDay(ctx, docs, code, now)
}

// DocumentDay is company code's date at instant now in the active
// configuration (docs), CompanyDay: for a caller that knows the company's
// code, such as the tenancy context's date of a legal entity
// (getLegalEntityToday).
func DocumentDay(ctx context.Context, docs Documents, code string, now time.Time) (CompanyDay, error) {
	doc, _, err := docs.Document(ctx)
	if err != nil {
		return CompanyDay{}, fmt.Errorf("time zone of company %s: %w", code, err)
	}
	return doc.CompanyDay(code, now)
}

// EntityDay is config.EntityDay with c as the active configuration, for a
// caller that has read it already.
func (c *Config) EntityDay(ctx context.Context, codeOf EntityCode, le uuid.UUID, now time.Time) (CompanyDay, error) {
	return EntityDay(ctx, held{c}, codeOf, le, now)
}

// held is a document already read, as Documents.
type held struct{ doc *Config }

func (h held) Document(context.Context) (*Config, int32, error) { return h.doc, 0, nil }
