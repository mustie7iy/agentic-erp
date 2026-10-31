package payables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// A posting date the caller leaves out is the company's date in its time
// zone (product owner 2026-10-31, ADR-0051 as built): an invoice posted or
// reversed at 22:00 on 31 March in Toronto (02:00 UTC on 1 April) is dated
// the 31st and falls in March's period, as the people posting it see the
// day. The date decides the period and the rates, so it is never UTC's for
// a company that sets a zone.

// companyDay is legal entity le's date now in its company's time zone
// (config.EntityDay: UTC when the company sets none). A stored zone the
// binary cannot load fails the post rather than dating it in UTC, and so
// does a stored invoice's legal entity the directory does not know (its
// company's date cannot be told, and UTC's is not it).
func (e *Endpoints) companyDay(ctx context.Context, le uuid.UUID) (time.Time, error) {
	d, err := config.EntityDay(ctx, e.Settings, tenancy.CompanyCode(e.LegalEntities), le, clock.Now(e.Now))
	if err != nil {
		return time.Time{}, fmt.Errorf("payables: %w", err)
	}
	return d.Date, nil
}
