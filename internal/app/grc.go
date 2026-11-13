package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/identity"
)

// GRC is the governance, risk and compliance context as the API wires it
// (ADR-0078 §1): a leaf that reads the identity context through its
// ports, by types of its own.
func GRC(cfg Config) *grc.Endpoints {
	return &grc.Endpoints{Pages: cfg.Pages, People: grcPeople{}}
}

// grcPeople gives the grc context principals' names, kinds and statuses.
type grcPeople struct{}

func (grcPeople) Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]grc.Person, error) {
	ns, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]grc.Person, len(ns))
	for id, n := range ns {
		out[id] = grc.Person{Name: n.Name, Kind: n.Kind, Status: n.Status}
	}
	return out, err
}
