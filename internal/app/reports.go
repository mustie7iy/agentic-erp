package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/identity"
)

// Reports is the analytics context's saved reports as the API wires them
// (ADR-0076): run through the semantic layer's endpoint, as the caller,
// with owners' names from the identity context.
func Reports(cfg Config, queries *endpoint.Endpoints) *reports.Endpoints {
	return &reports.Endpoints{Pages: cfg.Pages, Queries: queries, People: reportPeople{}}
}

// reportPeople gives the reports principals' names.
type reportPeople struct{}

func (reportPeople) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	ns, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]string, len(ns))
	for id, n := range ns {
		out[id] = n.Name
	}
	return out, err
}
