package configuration

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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
		return "", problem.Unprocessable("config.no_number_range",
			fmt.Sprintf("The configuration has no %s number range for this legal entity in fiscal year %d.", documentType, fiscalYear),
			problem.FieldError{Field: "document_type", Code: "no_number_range", Message: "add a number range for " + documentType + " through a change request"})
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
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, 0, err
	}
	var doc []byte
	var n int32
	err = tx.QueryRow(ctx, `SELECT v.document, v.number FROM config.active_version a JOIN config.config_version v ON v.tenant_id = a.tenant_id AND v.number = a.number`).
		Scan(&doc, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, problem.Conflict("config.not_bootstrapped", "The tenant has no configuration yet.")
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
