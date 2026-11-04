package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/cash"
)

// AnalyticsCash gives the analytics context the cash actuals (ADR-0051 §6,
// T53j1) through the ports it declares (analytics.CashActuals,
// extract.CashActuals), so neither context imports the other and cash
// stays a leaf (ADR-0051 §2): the semantic layer's database path reads
// them as the caller, the store's extract in its run, which erp-server
// wires.
type AnalyticsCash struct{ e *cash.Endpoints }

// NewAnalyticsCash is the cash actuals of the cash context as cfg wires it.
func NewAnalyticsCash(cfg Config) AnalyticsCash { return AnalyticsCash{e: Cash(cfg)} }

// ActualShares implements analytics.CashActuals: the semantic layer's
// database path, as the caller.
func (c AnalyticsCash) ActualShares(ctx context.Context, legalEntity uuid.UUID) ([]analytics.CashShare, error) {
	shares, err := c.e.ActualShares(ctx, legalEntity)
	out := make([]analytics.CashShare, len(shares))
	for i, s := range shares {
		out[i] = analytics.CashShare(s)
	}
	return out, err
}

// ExtractActualShares implements extract.CashActuals: the store's extract,
// in its run.
func (c AnalyticsCash) ExtractActualShares(ctx context.Context, legalEntity uuid.UUID) ([]extract.CashShare, error) {
	shares, err := c.e.ExtractActualShares(ctx, legalEntity)
	out := make([]extract.CashShare, len(shares))
	for i, s := range shares {
		out[i] = extract.CashShare(s)
	}
	return out, err
}
