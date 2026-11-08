package consolidation

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// maxVersions is the most (tenant, version) pairs versionCache keeps
// parsed in the process, whatever the tenant: a run reads one, and a
// run's reads name the versions of a page of runs, most often the same
// few. With more tenants consolidating at once they push each other's
// out, and a step parses its version again: a cost, never a wrong figure.
const maxVersions = 4

// versionKey is a configuration version of a tenant.
type versionKey struct {
	tenant uuid.UUID
	number int32
}

// versionCache keeps the documents of the configuration versions runs
// read, parsed. A run reads its version in every step and every read of
// it (the mapping, the group chart's names), and a group's document with
// 100 companies of 1,000 accounts each takes seconds to parse (ADR-0059
// §14's budget). An activated version never changes (config_version is
// insert-only, 000003), so a parsed document stays right; it is keyed by
// tenant, so no tenant reads another's, and fetched in the request's
// transaction under row-level security the first time. The least
// recently used pair is dropped beyond maxVersions. Callers only read it.
type versionCache struct {
	mu    sync.Mutex
	docs  map[versionKey]*config.Config
	order []versionKey // least recently used first
}

// version is the document of the configuration version a run recorded,
// read through Settings once per tenant and version while it is kept.
func (e *Endpoints) version(ctx context.Context, number int32) (*config.Config, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	return e.versions.get(versionKey{tenant: p.TenantID, number: number}, func() (*config.Config, error) { return e.Settings.Version(ctx, number) })
}

// get is the kept document of the key, or the one load reads, kept.
func (c *versionCache) get(k versionKey, load func() (*config.Config, error)) (*config.Config, error) {
	c.mu.Lock()
	doc, ok := c.docs[k]
	if ok {
		c.used(k)
	}
	c.mu.Unlock()
	if ok {
		return doc, nil
	}
	doc, err := load()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.docs == nil {
		c.docs = map[versionKey]*config.Config{}
	}
	if _, ok := c.docs[k]; !ok && len(c.order) == maxVersions {
		delete(c.docs, c.order[0])
		c.order = c.order[1:]
	}
	c.docs[k] = doc
	c.used(k)
	return doc, nil
}

// used moves the key to the most recently used end; the caller holds mu.
func (c *versionCache) used(k versionKey) {
	c.order = append(slices.DeleteFunc(c.order, func(x versionKey) bool { return x == k }), k)
}
