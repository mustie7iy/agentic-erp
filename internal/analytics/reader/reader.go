// Package reader is erp-analytics' side of the analytical store
// (ADR-0044): it answers a semantic query plan for one tenant from the
// Parquet files that tenant's manifest lists, in DuckDB, and returns
// pre-aggregated rows. It is the one package linked with cgo (ADR-0032,
// amended by ADR-0044); erp-server, erp-router and erp never import it.
//
// Isolation is in three layers: a fresh DuckDB database per query whose
// file access is limited to the files it registered and whose
// configuration is then locked; views created only over files a path
// guard accepted under the tenant's prefix and whose content matches the
// manifest's SHA-256; and a tenant_id filter in every view, so a file
// that holds another tenant's rows still returns none. The principal's
// company access and fact types are applied in the views and the plan,
// not after; the views select the contract's columns alone.
package reader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/analytics/datasets"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
)

// The wire types, named here as the reader has always named them.
type (
	Request = wire.Request
	Scope   = wire.Scope
	Result  = wire.Result
	Row     = wire.Row
	Plan    = wire.Plan
	Filter  = wire.Filter
)

// Reader reads the store.
type Reader struct {
	Store blobstore.Store // read-only access to the cell bucket's analytics/ prefix
	Cache string          // a local directory of verified files, named by their SHA-256
	// Exponents are the currencies' minor-unit digits (currency.Seeded);
	// a row whose exponent disagrees, or whose currency is missing, is
	// refused (ADR-0005).
	Exponents map[string]int

	limits   Limits
	slots    chan struct{} // one per query running
	contract map[string]*datasets.Dataset
}

// Limits bound what queries take of the machine: each query's DuckDB
// threads and memory, and how many run at once. A query beyond the cap is
// refused with ErrBusy rather than queued, so a burst never holds
// erp-server's requests behind it; the semantic layer answers it from the
// database.
type Limits struct {
	Threads     int    // per query; default 2
	MemoryLimit string // per query, DuckDB's notation ("512MB", "2GB"); default 1GB
	MaxQueries  int    // at once; default 4
}

// memoryPattern is DuckDB's memory notation: the limit is written into a
// SET, so nothing else may reach it.
var memoryPattern = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,3})?(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$`)

// Defaults is l with its unset limits filled, or an error for one out of
// range.
func (l Limits) Defaults() (Limits, error) {
	if l.Threads == 0 {
		l.Threads = 2
	}
	if l.MemoryLimit == "" {
		l.MemoryLimit = "1GB"
	}
	if l.MaxQueries == 0 {
		l.MaxQueries = 4
	}
	if l.Threads < 1 || l.MaxQueries < 1 || !memoryPattern.MatchString(l.MemoryLimit) {
		return l, fmt.Errorf("reader: limits %+v: threads and queries at least 1, memory like 512MB", l)
	}
	return l, nil
}

// ErrBusy means the reader is running its cap of queries; the request may
// be retried, and the semantic layer answers it from the database.
var ErrBusy = errors.New("reader: busy")

// contractV1 is the dataset contract; a variable, so a test can fail it.
var contractV1 = datasets.V1

// New is a reader over store, caching files in cache, within limits.
func New(store blobstore.Store, cache string, exponents map[string]int, limits Limits) (*Reader, error) {
	contract, err := contractV1()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cache) {
		return nil, fmt.Errorf("reader: cache %q must be an absolute directory", cache)
	}
	if limits, err = limits.Defaults(); err != nil {
		return nil, err
	}
	return &Reader{Store: store, Cache: cache, Exponents: exponents, limits: limits, slots: make(chan struct{}, limits.MaxQueries), contract: contract}, nil
}

// Query answers a plan for the request's tenant.
func (r *Reader) Query(ctx context.Context, req Request) (*Result, error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return nil, fmt.Errorf("%w: %d queries running", ErrBusy, r.limits.MaxQueries)
	}
	if !req.Scope.AllCompanies && len(req.Scope.Companies) == 0 {
		return nil, refused("the scope grants no company")
	}
	c, err := compile(r.contract, req.Scope, req.Plan)
	if err != nil {
		return nil, err
	}
	m, err := r.manifest(ctx, req.TenantID, req.ManifestKey)
	if err != nil {
		return nil, err
	}
	files := map[string][]string{}
	for _, name := range c.datasets {
		if files[name], err = r.files(ctx, req.TenantID, name, m); err != nil {
			return nil, err
		}
	}
	rows, through, err := r.run(ctx, req, c, files)
	if err != nil {
		return nil, err
	}
	return &Result{ManifestKey: req.ManifestKey, AsOf: m.Datasets[c.datasets[0]].AsOf, DataThrough: through, Rows: rows}, nil
}

// manifest reads the tenant's manifest and refuses one of another tenant
// or one whose datasets are not of one run.
func (r *Reader) manifest(ctx context.Context, tenant uuid.UUID, key string) (extract.Manifest, error) {
	var m extract.Manifest
	if err := checkManifestKey(tenant, key); err != nil {
		return m, err
	}
	b, _, err := r.Store.Get(ctx, key)
	if err != nil {
		return m, fmt.Errorf("reader: manifest %s: %w", key, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, refused("manifest %s: %v", key, err)
	}
	if m.TenantID != tenant {
		return m, refused("manifest %s is of tenant %s", key, m.TenantID)
	}
	var asOf time.Time
	for name, d := range m.Datasets {
		if asOf.IsZero() {
			asOf = d.AsOf
		}
		if !d.AsOf.Equal(asOf) || d.Version != 1 {
			return m, refused("manifest %s: dataset %s is version %d as of %s, want version 1 as of %s", key, name, d.Version, d.AsOf, asOf)
		}
	}
	return m, nil
}

// files is the local paths of a dataset's files, each guarded and checked
// against the manifest's digest and size.
func (r *Reader) files(ctx context.Context, tenant uuid.UUID, name string, m extract.Manifest) ([]string, error) {
	d, ok := m.Datasets[name]
	if !ok {
		return nil, refused("manifest lists no dataset %s", name)
	}
	out := make([]string, 0, len(d.Files))
	seen := map[string]bool{}
	for _, f := range d.Files {
		if err := checkFileKey(tenant, name, f); err != nil {
			return nil, err
		}
		if seen[f.Key] || seen[f.SHA256] {
			return nil, refused("manifest lists %s twice", f.Key) // read twice, its rows would count twice
		}
		seen[f.Key], seen[f.SHA256] = true, true
		p, err := r.fetch(ctx, f)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// fetch is a file's verified local copy, named by its digest: a copy
// already there is used when it still matches, and replaced otherwise.
func (r *Reader) fetch(ctx context.Context, f extract.File) (string, error) {
	p := filepath.Join(r.Cache, f.SHA256+".parquet")
	if b, err := os.ReadFile(p); err == nil && matches(b, f) {
		return p, nil
	}
	b, _, err := r.Store.Get(ctx, f.Key)
	if err != nil {
		return "", fmt.Errorf("reader: file %s: %w", f.Key, err)
	}
	if !matches(b, f) {
		return "", refused("file %s does not match the manifest's size and SHA-256", f.Key)
	}
	// Written aside and renamed, so a concurrent query never reads half a file.
	tmp := p + "." + uuid.NewString()
	err = os.WriteFile(tmp, b, 0o600)
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp) // a failed write's leftover, if any; the error reported is the write's
		return "", fmt.Errorf("reader: cache %s: %w", p, err)
	}
	return p, nil
}

func matches(b []byte, f extract.File) bool {
	sum := sha256.Sum256(b)
	want, err := hex.DecodeString(f.SHA256)
	return err == nil && int64(len(b)) == f.Size && bytes.Equal(sum[:], want)
}
