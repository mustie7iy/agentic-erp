package apitools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Days is the date an agent's run takes as today, the one port every
// agent reads it through (review:batch77): a run of one company takes the
// company's date in its time zone, as the API judges every date of the
// company (getLegalEntityToday, over config.DocumentDay; ADR-0051 As
// built), and any other run UTC's (coordinator 2026-11-17, ERP practice).
// The instant is Now's, so a test fixes the day; the API gives the zone.
type Days struct {
	API *Client
	// Now is the clock; nil reads the wall clock (clock.Now).
	Now func() time.Time
}

// UTC is today's date in UTC: a run of no one company, such as the
// tenant's access (product owner 2026-11-16).
func (d Days) UTC() string { return clock.Now(d.Now).UTC().Format(time.DateOnly) }

// Of is today's date in the time zone of the legal entity's company.
func (d Days) Of(ctx context.Context, legalEntity string) (string, error) {
	resp, err := d.API.Do(ctx, http.MethodGet, "/v1/legal-entities/"+url.PathEscape(legalEntity)+"/today", nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("apitools: today of legal entity %s: %w", legalEntity, err)
	}
	var today struct {
		TimeZone string `json:"time_zone"`
	}
	if err := resp.Decode(&today); err != nil {
		return "", fmt.Errorf("apitools: today of legal entity %s: %w", legalEntity, err)
	}
	loc, err := time.LoadLocation(today.TimeZone)
	if err != nil {
		return "", fmt.Errorf("apitools: today of legal entity %s: time zone %q: %w", legalEntity, today.TimeZone, err)
	}
	return clock.Now(d.Now).In(loc).Format(time.DateOnly), nil
}

// OfCompanies is today's date of a run over the companies, by their codes:
// the one company's when there is exactly one, else UTC's, as there is
// no one company's day to take.
func (d Days) OfCompanies(ctx context.Context, codes []string) (string, error) {
	if len(codes) != 1 || d.API == nil {
		return d.UTC(), nil
	}
	le, err := LegalEntity(ctx, d.API, codes[0])
	if err != nil {
		return "", fmt.Errorf("apitools: today of company %s: %w", codes[0], err)
	}
	return d.Of(ctx, le)
}
