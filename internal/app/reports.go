package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Reports is the analytics context's saved reports as the API wires them
// (ADR-0076): run through the semantic layer's endpoint, as the caller,
// with principals and roles from the identity context and companies from
// the tenancy context.
func Reports(cfg Config, queries *endpoint.Endpoints) *reports.Endpoints {
	return &reports.Endpoints{Pages: cfg.Pages, Queries: queries, People: reportPeople{}, Grantees: reportGrantees{}, Companies: reportCompanies{},
		Configs: configuration.Settings{}, Objects: cfg.Objects, Now: cfg.Now}
}

// ReportScheduler makes saved reports' scheduled runs (ADR-0076 §5) as
// their owners, reading the API in-process through cfg's loopback, which
// New gave the API's handler.
func ReportScheduler(cfg Config, workerRole string) *reports.Scheduler {
	return &reports.Scheduler{Reports: Reports(cfg, analyticsEndpoints(cfg, cfg.Loopback).Init()), Actors: identity.Actors{}, Agents: agents.Admit,
		Begin: cfg.Pool.Begin, AppRole: cfg.AppRole, WorkerRole: workerRole, Log: cfg.Log}
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

// reportGrantees gives the reports whom they may be shared with and the
// roles their readers hold (ADR-0076 §7).
type reportGrantees struct{}

func (reportGrantees) Kinds(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	ns, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]string, len(ns))
	for id, n := range ns {
		out[id] = n.Kind
	}
	return out, err
}

// RolesHeld are the active roles the principal holds today: a share with
// a role reaches its holders while the role is active, as authz grants its
// permissions only then.
func (g reportGrantees) RolesHeld(ctx context.Context, principal uuid.UUID) ([]uuid.UUID, error) {
	held, err := identity.Directory{}.RolesHeld(ctx, principal)
	if err != nil {
		return nil, err
	}
	roles, err := g.Roles(ctx, held)
	out := []uuid.UUID{}
	for _, id := range held {
		if roles[id].Active {
			out = append(out, id)
		}
	}
	return out, err
}

func (reportGrantees) Roles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]reports.Role, error) {
	rs, err := identity.Directory{}.Roles(ctx, ids)
	out := make(map[uuid.UUID]reports.Role, len(rs))
	for id, r := range rs {
		out[id] = reports.Role{Code: r.Code, Name: r.Name, Active: r.Status == "active"}
	}
	return out, err
}

// reportCompanies names the companies a reader's access reaches: the
// tenant's legal entities it allows, by code.
type reportCompanies struct{}

func (reportCompanies) Codes(ctx context.Context, access authz.Access) ([]string, error) {
	entities, err := tenancy.Directory{}.Entities(ctx)
	var out []string
	for _, le := range entities {
		if access.Allows(le.ID) {
			out = append(out, le.Code)
		}
	}
	return out, err
}
