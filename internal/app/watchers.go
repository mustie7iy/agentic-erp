package app

import (
	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/analytics/watchers"
)

// Watchers is the analytics context's metric watchers as the API wires
// them (ADR-0077): evaluated through the semantic layer's endpoint, as the
// caller.
func Watchers(cfg Config, queries *endpoint.Endpoints) *watchers.Endpoints {
	return &watchers.Endpoints{Pages: cfg.Pages, Queries: queries, Now: cfg.Now}
}
