package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/identity"
)

// GRC is the governance, risk and compliance context as the API wires it
// (ADR-0078 §1): a leaf that reads the identity context and the active
// configuration through its ports, by types of its own.
func GRC(cfg Config) *grc.Endpoints {
	return &grc.Endpoints{Pages: cfg.Pages, People: grcPeople{}, Access: grcAccess{}, Settings: grcSettings{}}
}

// grcAccess gives the grc context the permission catalog's descriptions
// and the tenant's segregation-of-duties rules (ADR-0078 §8).
type grcAccess struct{}

func (grcAccess) Permissions(ctx context.Context, codes []string) (map[string]string, error) {
	return identity.Catalog{}.Permissions(ctx, codes)
}

func (grcAccess) SodRules(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]grc.SodRule, error) {
	rules, err := identity.Catalog{}.SodRules(ctx, ids)
	out := make(map[uuid.UUID]grc.SodRule, len(rules))
	for id, r := range rules {
		out[id] = grc.SodRule{PermissionA: r.PermissionA, PermissionB: r.PermissionB, Description: r.Description, ValidFrom: r.ValidFrom, ValidTo: r.ValidTo}
	}
	return out, err
}

// grcSettings gives the grc context the active configuration version's
// number and which of the paths have a value in it; a tenant without a
// configuration is 409 config.not_bootstrapped.
type grcSettings struct{}

func (grcSettings) Present(ctx context.Context, paths []string) (int32, map[string]bool, error) {
	doc, version, err := configuration.Settings{}.Document(ctx)
	if err != nil {
		return 0, nil, err
	}
	return version, doc.Present(paths), nil
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
