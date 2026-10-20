package reader

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/analytics/datasets"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
)

func TestCompileRefuses(t *testing.T) {
	contract, err := datasets.V1()
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Plan{
		"unknown dataset":   {Dataset: "nope", Aggregate: "sum"},
		"dimension dataset": {Dataset: "accounts", Aggregate: "sum"},
		"aggregate":         {Dataset: "gl_lines", Aggregate: "avg"},
		"unknown column":    {Dataset: "gl_lines", Aggregate: "sum", By: []string{"nope"}},
		"a key":             {Dataset: "gl_lines", Aggregate: "sum", By: []string{"document_id"}},
		"filter column":     {Dataset: "gl_lines", Aggregate: "sum", Where: []Filter{{Column: "amount_minor", Op: "in", Values: []string{"1"}}}},
		"range off date":    {Dataset: "gl_lines", Aggregate: "sum", Where: []Filter{{Column: "company", Op: ">=", Values: []string{"2026-01-01"}}}},
		"range not a date":  {Dataset: "gl_lines", Aggregate: "sum", Where: []Filter{{Column: "posting_date", Op: "<=", Values: []string{"x"}}}},
		"range count":       {Dataset: "gl_lines", Aggregate: "sum", Where: []Filter{{Column: "posting_date", Op: "between", Values: []string{"2026-01-01"}}}},
		"op":                {Dataset: "gl_lines", Aggregate: "sum", Where: []Filter{{Column: "company", Op: "like", Values: []string{"1%"}}}},
		"empty in":          {Dataset: "gl_lines", Aggregate: "sum", Where: []Filter{{Column: "company", Op: "in"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compile(contract, everything, p)
			refusedErr(t, err)
		})
	}
}

func TestGuardRefuses(t *testing.T) {
	tenant := uuid.New()
	good := extract.File{Key: extract.Prefix + "/tenant=" + tenant.String() + "/accounts/part-" + strings.Repeat("a", 64) + ".parquet", SHA256: strings.Repeat("a", 64)}
	if err := checkFileKey(tenant, "accounts", good); err != nil {
		t.Fatalf("good key: %v", err)
	}
	for name, tc := range map[string]struct {
		dataset string
		f       extract.File
	}{
		"dataset name": {"../accounts", good},
		"digest":       {"accounts", extract.File{Key: good.Key, SHA256: "zz"}},
		"size":         {"accounts", extract.File{Key: good.Key, SHA256: good.SHA256, Size: -1}},
	} {
		t.Run(name, func(t *testing.T) { refusedErr(t, checkFileKey(tenant, tc.dataset, tc.f)) })
	}
}

// FuzzManifestPath: whatever key a manifest names, the guard accepts it only
// inside the tenant's dataset directory, as a part the extractor writes.
func FuzzManifestPath(f *testing.F) {
	tenant, other := uuid.MustParse("11111111-1111-4111-8111-111111111111"), uuid.MustParse("22222222-2222-4222-8222-222222222222")
	base := extract.Prefix + "/tenant=" + tenant.String() + "/gl_lines/"
	for _, s := range []string{
		base + "month=2026-03/part-" + strings.Repeat("0", 20) + "-" + strings.Repeat("1", 20) + ".parquet",
		base + "part-" + strings.Repeat("a", 64) + ".parquet",
		base + "../../tenant=" + other.String() + "/gl_lines/part-" + strings.Repeat("a", 64) + ".parquet",
		extract.Prefix + "/tenant=" + other.String() + "/gl_lines/part-" + strings.Repeat("a", 64) + ".parquet",
		base + "month=2026-03/*.parquet", "s3://bucket/" + base + "part-x.parquet", base + "month=2026-13/part-" + strings.Repeat("a", 64) + ".parquet",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		err := checkFileKey(tenant, "gl_lines", extract.File{Key: key, SHA256: strings.Repeat("a", 64)})
		if err != nil {
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("%q: got %v, want ErrRefused", key, err)
			}
			return
		}
		if !strings.HasPrefix(key, base) || path.Clean(key) != key || strings.ContainsAny(key, `*?[]{}\:`+"\x00") || strings.Contains(key, "..") ||
			strings.Count(key, "tenant=") != 1 {
			t.Fatalf("%q: accepted, want it refused", key)
		}
		if err := checkManifestKey(tenant, key); err == nil {
			t.Fatalf("%q: accepted as a manifest too, want parts and manifests apart", key)
		}
	})
}

func TestNewRefuses(t *testing.T) {
	if _, err := New(blobstore.Dir{}, "relative", nil); err == nil {
		t.Error("relative cache: got nil error, want one")
	}
	saved := contractV1
	t.Cleanup(func() { contractV1 = saved })
	contractV1 = func() (map[string]*datasets.Dataset, error) { return nil, errors.New("broken") }
	if _, err := New(blobstore.Dir{}, t.TempDir(), nil); err == nil {
		t.Error("broken contract: got nil error, want one")
	}
}

// getFails is a store whose Get of keys containing part fails.
type getFails struct {
	blobstore.Store
	part string
}

func (g getFails) Get(ctx context.Context, key string) ([]byte, string, error) {
	if strings.Contains(key, g.part) {
		return nil, "", errors.New("unavailable")
	}
	return g.Store.Get(ctx, key)
}

func TestManifestAndFileFailures(t *testing.T) {
	e := setup(t)
	p := Plan{Dataset: "gl_lines", Aggregate: "sum"}
	put := func(t *testing.T, b []byte) string {
		t.Helper()
		key := extract.ManifestKey(e.a, 9, strings.Repeat("e", 16))
		if err := e.store.Put(context.Background(), key, b, blobstore.PutOptions{}); err != nil && !errors.Is(err, blobstore.ErrExists) {
			t.Fatal(err)
		}
		return key
	}
	mixed := e.manifs[e.a]
	mixed.Datasets = map[string]extract.Dataset{"gl_lines": mixed.Datasets["gl_lines"], "accounts": {Version: 1, AsOf: asOf.Add(time.Second)}}
	noGL := e.manifs[e.a]
	noGL.Datasets = map[string]extract.Dataset{"accounts": noGL.Datasets["accounts"]}
	for name, tc := range map[string]struct {
		key     func(t *testing.T) string
		store   blobstore.Store
		refused bool
	}{
		"missing manifest": {key: func(*testing.T) string { return extract.ManifestKey(e.a, 1, "") }},
		"not json":         {key: func(t *testing.T) string { t.Helper(); return put(t, []byte("{")) }, refused: true},
		"mixed runs":       {key: func(*testing.T) string { return e.publish(e.a, mixed) }, refused: true},
		"no dataset":       {key: func(*testing.T) string { return e.publish(e.a, noGL) }, refused: true},
		"file unavailable": {key: func(*testing.T) string { return e.keys[e.a] }, store: getFails{e.store, "/gl_lines/"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := *e.r
			if tc.store != nil {
				r.Store = tc.store
			}
			req := e.request(e.a, p)
			req.ManifestKey = tc.key(t)
			_, err := r.Query(context.Background(), req)
			if err == nil || errors.Is(err, ErrRefused) != tc.refused {
				t.Errorf("error: got %v, want an error (refused %v)", err, tc.refused)
			}
		})
	}
}

func TestCacheFailures(t *testing.T) {
	e := setup(t)
	f := e.manifs[e.a].Datasets["gl_lines"].Files[0]
	t.Run("rename", func(t *testing.T) {
		r := *e.r
		r.Cache = t.TempDir()
		if err := os.Mkdir(filepath.Join(r.Cache, f.SHA256+".parquet"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := r.fetch(context.Background(), f); err == nil {
			t.Error("fetch onto a directory: got nil error, want one")
		}
	})
	t.Run("write", func(t *testing.T) {
		r := *e.r
		r.Cache = filepath.Join(t.TempDir(), "missing")
		if _, err := r.fetch(context.Background(), f); err == nil {
			t.Error("fetch into a missing directory: got nil error, want one")
		}
	})
}

func TestDuckFailures(t *testing.T) {
	e := setup(t)
	p := Plan{Dataset: "gl_lines", Aggregate: "sum"}
	t.Run("connect", func(t *testing.T) {
		saved := connect
		t.Cleanup(func() { connect = saved })
		connect = func() (driver.Connector, error) { return nil, errors.New("no memory") }
		if _, err := e.r.Query(context.Background(), e.request(e.a, p)); err == nil {
			t.Error("query: got nil error, want the connector's")
		}
	})
	t.Run("duplicate file", func(t *testing.T) {
		m := e.manifs[e.a]
		f := m.Datasets["gl_lines"].Files[0]
		m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(f, f)}
		req := e.request(e.a, p)
		req.ManifestKey = e.publish(e.a, m)
		_, err := e.r.Query(context.Background(), req)
		refusedErr(t, err)
	})
	t.Run("duplicate content", func(t *testing.T) {
		m := e.manifs[e.a]
		f := m.Datasets["gl_lines"].Files[0]
		g := f
		g.Key = strings.Replace(f.Key, "month=2026-03", "month=2026-04", 1)
		m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(f, g)}
		req := e.request(e.a, p)
		req.ManifestKey = e.publish(e.a, m)
		_, err := e.r.Query(context.Background(), req)
		refusedErr(t, err)
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := e.r.Query(ctx, e.request(e.a, p)); err == nil {
			t.Error("query: got nil error, want the context's")
		}
	})
	forged := func(t *testing.T, b []byte) Request {
		t.Helper()
		m := e.manifs[e.a]
		m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(e.put(part(e.a, "gl_lines", "2026-05", b), b))}
		req := e.request(e.a, p)
		req.ManifestKey = e.publish(e.a, m)
		return req
	}
	type textAmount struct {
		TenantID    uuid.UUID `parquet:"tenant_id,uuid"`
		AmountMinor string    `parquet:"amount_minor"`
		Currency    string    `parquet:"currency"`
		Exponent    string    `parquet:"exponent"`
	}
	// Every column of the contract, the amount as text: the views bind, the
	// sum does not.
	type textGL struct {
		TenantID     uuid.UUID `parquet:"tenant_id,uuid"`
		DocumentID   uuid.UUID `parquet:"document_id,uuid"`
		LineNo       int32     `parquet:"line_no"`
		LedgerID     uuid.UUID `parquet:"ledger_id,uuid"`
		Company      string    `parquet:"company"`
		PostingDate  int32     `parquet:"posting_date,date"`
		Account      string    `parquet:"account"`
		DocumentType string    `parquet:"document_type"`
		Vendor       *string   `parquet:"vendor,optional"`
		AmountMinor  string    `parquet:"amount_minor"`
		Currency     string    `parquet:"currency"`
		Exponent     int32     `parquet:"exponent"`
		PostedXID    uint64    `parquet:"posted_xid"`
	}
	for name, req := range map[string]Request{
		"amount as text":  forged(t, encode(t, []textGL{{TenantID: e.a, AmountMinor: "x"}})),
		"not parquet":     forged(t, []byte("not parquet")),
		"missing columns": forged(t, encode(t, []textAmount{{TenantID: e.a}})),
	} {
		t.Run(name, func(t *testing.T) {
			opened, closed := countConnections(t)
			if _, err := e.r.Query(context.Background(), req); err == nil {
				t.Error("query: got nil error, want one")
			}
			if o, c := opened.Load(), closed.Load(); o == 0 || o != c {
				t.Errorf("connections: got %d opened and %d closed, want every one closed", o, c)
			}
		})
	}
}

// duckConn is the part of DuckDB's connection database/sql uses.
type duckConn interface {
	driver.Conn
	driver.ExecerContext
	driver.QueryerContext
}

type countedConn struct {
	duckConn
	closed *atomic.Int32
}

func (c countedConn) Close() error { c.closed.Add(1); return c.duckConn.Close() }

type countingConnector struct {
	driver.Connector
	opened, closed *atomic.Int32
}

func (c countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c.opened.Add(1)
	return countedConn{conn.(duckConn), c.closed}, nil
}

// countConnections counts the connections queries open and close.
func countConnections(t *testing.T) (opened, closed *atomic.Int32) {
	t.Helper()
	saved := connect
	t.Cleanup(func() { connect = saved })
	opened, closed = new(atomic.Int32), new(atomic.Int32)
	connect = func() (driver.Connector, error) {
		c, err := saved()
		return countingConnector{c, opened, closed}, err
	}
	return opened, closed
}

// rowsFake replays a scanner's failures.
type rowsFake struct {
	next    int
	scanErr error
	err     error
	total   *big.Int
}

func (f *rowsFake) Next() bool { f.next--; return f.next >= 0 }
func (f *rowsFake) Err() error { return f.err }
func (f *rowsFake) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	*dest[0].(*string), *dest[1].(*int) = "EUR", 2
	*dest[2].(**big.Int) = f.total
	return nil
}

func TestScanFailures(t *testing.T) {
	r := &Reader{Exponents: map[string]int{"EUR": 2}}
	for name, f := range map[string]*rowsFake{
		"scan":    {next: 1, scanErr: errors.New("type")},
		"rows":    {err: errors.New("lost")},
		"no sum":  {next: 1},
		"too big": {next: 1, total: new(big.Int).Lsh(big.NewInt(1), 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.scan(f, 0, true); err == nil {
				t.Error("scan: got nil error, want one")
			}
		})
	}
}

func TestHandler(t *testing.T) {
	e := setup(t)
	h := Handler(e.r, slog.New(slog.DiscardHandler))
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, QueryPath, strings.NewReader(body)))
		return w
	}
	ok := `{"tenant_id":"` + e.a.String() + `","manifest_key":"` + e.keys[e.a] + `","scope":{"all_companies":true,"datasets":["gl_lines"]},"plan":{"dataset":"gl_lines","aggregate":"sum","by":["company"]}}`
	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"answer":  {ok, http.StatusOK, `"amount_minor":150`},
		"unknown": {`{"sql":"SELECT 1"}`, http.StatusBadRequest, "analytics.invalid_request"},
		"refused": {strings.Replace(ok, e.keys[e.a], e.keys[e.b], 1), http.StatusUnprocessableEntity, "analytics.refused"},
		"failed":  {strings.Replace(ok, e.keys[e.a], extract.ManifestKey(e.a, 1, ""), 1), http.StatusInternalServerError, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := post(tc.body)
			b, _ := io.ReadAll(w.Body)
			if w.Code != tc.code || !strings.Contains(string(b), tc.want) {
				t.Errorf("response: got %d %s, want %d with %q", w.Code, b, tc.code, tc.want)
			}
		})
	}
}
