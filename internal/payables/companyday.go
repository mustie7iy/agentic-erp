package payables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// A posting date the caller leaves out is the company's date in its time
// zone (product owner 2026-10-31, ADR-0051 as built): an invoice posted or
// reversed at 22:00 on 31 March in Toronto (02:00 UTC on 1 April) is dated
// the 31st and falls in March's period, as the people posting it see the
// day. The date decides the period and the rates, so it is never UTC's for
// a company that sets a zone.

// companyDay is legal entity le's date now in its company's time zone
// (config.CompanyLocation: UTC when the company sets none). A stored zone
// the binary cannot load fails the post rather than dating it in UTC.
func (e *Endpoints) companyDay(ctx context.Context, le uuid.UUID) (time.Time, error) {
	ent, found, err := e.LegalEntities.Entity(ctx, le)
	if err == nil && !found {
		// A stored invoice's legal entity exists (its foreign key); were it
		// gone, its company's date could not be told, and UTC's is not it.
		err = errors.New("no such legal entity")
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("payables: legal entity %s: %w", le, err)
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("payables: time zone of company %s: %w", ent.Code, err)
	}
	loc, err := doc.CompanyLocation(ent.Code)
	if err != nil {
		return time.Time{}, fmt.Errorf("payables: company %s: %w", ent.Code, err)
	}
	return clock.DayIn(clock.Now(e.Now), loc), nil
}
