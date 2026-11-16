package partner

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// Settings is the configuration context as partner reads it: the tenant's
// custom field schemas (ADR-0046), and the active configuration, whose
// companies' time zones a business partner's day is judged in.
type Settings interface {
	extensions.Schemas
	config.Documents
}

// partnerDay is partner's day (Day) in the active configuration (docs) at
// the instant now gives, or, with no clock injected (production), at the
// transaction's start: one request judges every date on one day, as the
// database's current_date (UTC) it replaces did, even across midnight: a
// migration run dates what it loads by Migration.Today and partner checks
// it on the same day.
func partnerDay(ctx context.Context, tx pgx.Tx, docs config.Documents, now func() time.Time) (time.Time, error) {
	doc, _, err := docs.Document(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("partner: the companies' day: %w", err)
	}
	if now != nil {
		return Day(doc, now())
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("partner: read the transaction's start: %w", err)
	}
	return Day(doc, at)
}

// Day is the business partners' day at instant now: the earliest of the
// tenant's companies' days in configuration doc, each in its time zone
// (config.CompanyDay; PO 2026-10-31, ADR-0051 As built), or UTC's when
// doc lists no company. A business partner, its addresses and its bank
// accounts are the tenant's, not one company's: every company may pay to
// an account, so a first day is accepted when it is today for one of
// them, and an account approved on a Toronto evening starts on Toronto's
// day, the day a payment run of the company is dated, not on UTC's, which
// is already tomorrow. A migration run dates what it loads by it too.
func Day(doc *config.Config, now time.Time) (time.Time, error) {
	if len(doc.Companies) == 0 {
		return clock.Day(now), nil
	}
	var day time.Time
	for i, co := range doc.Companies {
		d, err := doc.CompanyDay(co.Code, now)
		if err != nil {
			return time.Time{}, fmt.Errorf("partner: the companies' day: %w", err)
		}
		if i == 0 || d.Date.Before(day) {
			day = d.Date
		}
	}
	return day, nil
}
