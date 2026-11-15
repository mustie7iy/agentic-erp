package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/analytics/watchers"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Watchers is the analytics context's metric watchers as the API wires
// them (ADR-0077): evaluated through the semantic layer's endpoint, as the
// caller, with principals' names from the identity context and companies'
// codes from tenancy.
func Watchers(cfg Config, queries *endpoint.Endpoints) *watchers.Endpoints {
	return &watchers.Endpoints{Pages: cfg.Pages, Queries: queries, People: watcherPeople{}, Companies: companyCodes{}, Now: cfg.Now}
}

// WatchEvaluator evaluates the metric watchers of a tenant whose
// analytical store was refreshed (ADR-0077 §4), as their creators, from the
// store alone: its semantic layer reads no API, so a store that cannot
// answer leaves a watcher for the next refresh.
func WatchEvaluator(cfg Config) *watchers.Worker {
	// A tenant no longer active is not evaluated for (ADR-0015 As built).
	begin := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		return database.BeginActive(ctx, cfg.Pool, nil, s)
	}
	return &watchers.Worker{Watchers: Watchers(cfg, analyticsEndpoints(cfg, storeOnly{}).Init()), Begin: begin, AppRole: cfg.AppRole,
		Actors: identity.Actors{}, Now: cfg.Now, Log: cfg.Log}
}

// errStoreOnly is the database path's answer in the watchers' worker.
var errStoreOnly = errors.New("app: the metric watchers' worker reads the analytical store alone (ADR-0077 §4)")

// storeOnly refuses every read of the API: the transport of the semantic
// layer's database path in the watchers' worker.
type storeOnly struct{}

func (storeOnly) RoundTrip(*http.Request) (*http.Response, error) { return nil, errStoreOnly }

// watcherPeople gives the watchers principals' names.
type watcherPeople struct{}

func (watcherPeople) Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	ns, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]string, len(ns))
	for id, n := range ns {
		out[id] = n.Name
	}
	return out, err
}
