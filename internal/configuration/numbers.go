package configuration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// Numbers issues document numbers from the tenant's number ranges to other
// contexts, in the request's transaction. A gapless range stays locked
// until the transaction ends, so a rolled-back request gives its number back.
type Numbers struct{}

// Next allocates the next number of the legal entity's range for the
// document type and fiscal year. Without a range the answer is 422
// config.no_number_range; an exhausted range is 409
// config.number_range_exhausted.
func (Numbers) Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	var n string
	err = tx.QueryRow(ctx, `SELECT config.next_document_number($1, $2, $3, $4)`, p.TenantID, legalEntity, documentType, fiscalYear).Scan(&n)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "P0002": // no_data_found
		// No field error: the many operations that draw a number (an
		// invoice's posting, a payment run's) have no document_type field,
		// so the detail names the document type and fiscal year instead
		// (REVIEW.md, API-consumer classes: a field error names the
		// request's own field).
		return "", problem.Unprocessable("config.no_number_range",
			fmt.Sprintf("The configuration has no %s number range for this legal entity in fiscal year %d; add one through a change request.", documentType, fiscalYear))
	case errors.As(err, &pg) && pg.Code == "23514": // the function's exhausted range
		return "", problem.Conflict("config.number_range_exhausted",
			fmt.Sprintf("The %s number range of fiscal year %d is exhausted; raise its last number through a change request.", documentType, fiscalYear))
	}
	if err != nil {
		return "", fmt.Errorf("configuration: next %s number for legal entity %s in fiscal year %d: %w", documentType, legalEntity, fiscalYear, err)
	}
	return n, nil
}

// Settings reads the tenant's active configuration for other contexts:
// approval policies, tolerances and limits.
type Settings struct{}

// Document returns the active configuration and its version number; a
// tenant without one is 409 config.not_bootstrapped.
func (Settings) Document(ctx context.Context) (*config.Config, int32, error) {
	c, n, err := active(ctx)
	if err == nil && c == nil {
		err = problem.Conflict("config.not_bootstrapped", "The tenant has no configuration yet.")
	}
	return c, n, err
}

// CompanyDay is company code's date at instant now in its time zone by
// the active configuration (config.DocumentDay: UTC when the document has
// no such company or it sets none), and the zone: the tenancy context's
// port of a legal entity's date today (getLegalEntityToday). Without an
// active configuration it is 409 config.not_bootstrapped, as Document is.
func (s Settings) CompanyDay(ctx context.Context, code string, now time.Time) (time.Time, *time.Location, error) {
	d, err := config.DocumentDay(ctx, s, code, now)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("configuration: date of company %s: %w", code, err)
	}
	return d.Date, d.Zone, nil
}

// Hold keeps the active configuration from changing until the request's
// transaction ends: it takes the tenant's activation lock shared, which an
// activation takes exclusively before it locks number ranges or applies a
// section (activate). A long transaction that takes locks an activation
// also takes, in another order, holds it first so the two never wait on
// each other in a cycle (ADR-0008): a migration load gives suppliers
// payment methods under the partner context's method lock, then draws
// numbers from ranges, where an activation locks the ranges, then the
// method lock. Only activation is held back: drafting a change request,
// a country pack or a regression suite does not take the lock.
func (Settings) Hold(ctx context.Context) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1 || platform.require_tenant()::text, 0))`, activationLock); err != nil {
		return fmt.Errorf("configuration: hold the active configuration: %w", err)
	}
	return nil
}

// activationLock names the tenant's activation lock (Settings.Hold).
const activationLock = "config.activation."

// Schema returns the entity's custom field schema in the active
// configuration (extensions.Schemas), nil when the tenant declares none,
// a tenant without a configuration included: it has no custom fields.
func (Settings) Schema(ctx context.Context, entity string) (*extensions.Schema, error) {
	c, _, err := active(ctx)
	if err != nil || c == nil {
		return nil, err
	}
	s, ok := c.Extensions[entity]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

// Rules returns the entity's validation rules in the active configuration
// (rules.Source), with the entity's custom field schema they read and the
// configuration's version, which names them; a tenant without a
// configuration has none.
func (Settings) Rules(ctx context.Context, entity string) (rules.Active, error) {
	c, n, err := active(ctx)
	if err != nil || c == nil {
		return rules.Active{}, err
	}
	return activeRules(c, n, entity), nil
}

// active reads the active configuration and its version number, nil for a
// tenant without one.
func active(ctx context.Context) (*config.Config, int32, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, 0, err
	}
	var doc []byte
	var n int32
	err = tx.QueryRow(ctx, `SELECT v.document, v.number FROM config.active_version a JOIN config.config_version v ON v.tenant_id = a.tenant_id AND v.number = a.number`).
		Scan(&doc, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("configuration: read the active configuration: %w", err)
	}
	c, err := activeConfig(ctx, doc)
	if err != nil {
		return nil, 0, fmt.Errorf("configuration: read the active configuration %d: %w", n, err)
	}
	return c, n, nil
}

// Version returns the document of an activated configuration version, for
// a reading under a past version: an audit's analytics query answered with
// the metric definitions of the version a report was made under
// (ADR-0044). An unknown number is 422 config.unknown_version.
func (Settings) Version(ctx context.Context, number int32) (*config.Config, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := versionDocument(ctx, tx, number)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem.Unprocessable("config.unknown_version", fmt.Sprintf("There is no configuration version %d.", number),
			problem.FieldError{Field: "config_version", Code: "not_found", Message: "an activated configuration version"})
	}
	return c, err
}
