package sap

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSourceAndWriteExtract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.json")
	if err := WriteExtract(path, GenerateMock(42, "2026")); err != nil {
		t.Fatal(err)
	}
	src := FileSource{Path: path}
	if src.Name() != "file:"+path {
		t.Fatal("name")
	}
	e, err := src.Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	if err != nil || len(e.T001) != 1 {
		t.Fatalf("extract: %v", err)
	}
	if _, err := (FileSource{Path: filepath.Join(dir, "missing.json")}).Extract(context.Background(), Scope{}); err == nil {
		t.Fatal("missing file")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileSource{Path: filepath.Join(dir, "bad.json")}).Extract(context.Background(), Scope{}); err == nil {
		t.Fatal("bad json")
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteExtract(filepath.Join(blocker, "a", "x.json"), &Extract{}); err == nil {
		t.Fatal("directory creation failure")
	}
	if err := WriteExtract(dir, &Extract{}); err == nil {
		t.Fatal("write failure")
	}
}

func TestFilterScopeKeepsOnlyTheScope(t *testing.T) {
	all := GenerateMock(42, "2026")
	us := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	for _, r := range us.SKB1 {
		if r.BUKRS != "1000" {
			t.Fatalf("SKB1 leak: got company %q, want 1000", r.BUKRS)
		}
	}
	for _, r := range us.T007A {
		if r.BUKRS != "1000" {
			t.Fatalf("T007A leak: got company %q, want 1000", r.BUKRS)
		}
	}
	if len(us.T030) != 2 {
		t.Errorf("T030 rows in scope: got %d, want 2", len(us.T030))
	}
	if len(us.T169G) != 1 {
		t.Errorf("T169G rows in scope: got %d, want 1", len(us.T169G))
	}
	if len(us.T012K) != 1 {
		t.Errorf("T012K rows in scope: got %d, want 1", len(us.T012K))
	}
	if len(us.T052) != len(all.T052) {
		t.Errorf("T052 rows in scope: got %d, want all %d", len(us.T052), len(all.T052))
	}
	if len(us.AGR_USERS) != len(all.AGR_USERS) {
		t.Errorf("AGR_USERS rows in scope: got %d, want all %d", len(us.AGR_USERS), len(all.AGR_USERS))
	}
	if len(us.EKPO) == 0 {
		t.Error("EKPO rows in scope: got 0, want some")
	}
	if len(us.MSEG) == 0 {
		t.Error("MSEG rows in scope: got 0, want some")
	}
	if len(us.RSEG) == 0 {
		t.Error("RSEG rows in scope: got 0, want some")
	}
	if len(us.LFBK) == 0 {
		t.Error("LFBK rows in scope: got 0, want some")
	}
	if len(us.ACDOCA) == 0 {
		t.Error("ACDOCA rows in scope: got 0, want some")
	}
	wrongYear := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2025"})
	if len(wrongYear.RBKP) != 0 {
		t.Errorf("RBKP rows for another year: got %d, want 0", len(wrongYear.RBKP))
	}
	if len(wrongYear.ACDOCA) != 0 {
		t.Errorf("ACDOCA rows for another year: got %d, want 0", len(wrongYear.ACDOCA))
	}
	c := us.Counts()
	if c["T001"] != 1 {
		t.Errorf("T001 count: got %d, want 1 (%v)", c["T001"], c)
	}
	if c["AGR_TCODES"] == 0 {
		t.Errorf("AGR_TCODES count: got 0, want some (%v)", c)
	}
}

func TestMockInternals(t *testing.T) {
	if mockCurrency("9999") != "" {
		t.Fatal("unknown company currency")
	}
	g := &mockGen{e: &Extract{}, gj: "2026", fi: map[string]int{}}
	defer func() {
		if recover() == nil {
			t.Fatal("an unbalanced mock document must panic")
		}
	}()
	g.fiDoc("1000", "20260101", "SA", "X", []fiLine{{acct: "1", amount: 5}})
}

func TestODataOptionsAndErrors(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		// A failed write reaches the source as a broken answer, which the test asserts on.
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{"results": []map[string]any{
			{"AccountingDocument": "1", "CompanyCode": "1000", "FiscalYear": "2026", "Extra": map[string]any{"x": 1}},
		}}})
	}))
	defer srv.Close()
	em := DefaultEntities[len(DefaultEntities)-1] // ACDOCA: company + year filters, static RLDNR
	em.Fields = map[string]string{"BELNR": "AccountingDocument", "RBUKRS": "CompanyCode", "GJAHR": "FiscalYear", "XBLNR": "Extra"}
	src := &ODataSource{BaseURL: srv.URL, Client: "100", Entities: []EntityMapping{em}}
	if src.Name() != "odata:"+srv.URL {
		t.Fatal("name")
	}
	e, err := src.Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(e.ACDOCA) != 1 {
		t.Fatalf("ACDOCA rows: got %d, want 1", len(e.ACDOCA))
	}
	if got := e.ACDOCA[0].RLDNR; got != "0L" {
		t.Errorf("RLDNR: got %q, want %q", got, "0L")
	}
	if got := e.ACDOCA[0].XBLNR; got != "map[x:1]" {
		t.Errorf("XBLNR: got %q, want %q", got, "map[x:1]")
	}
	q := seen[0]
	for _, want := range []string{"sap-client=100", "%24top=1000", "FiscalYear+eq+%272026%27"} {
		if !strings.Contains(q, want) {
			t.Errorf("query: got %s, want it to contain %s", q, want)
		}
	}
	// Entities without a company field are fetched once, unfiltered.
	if got := companyFilter(EntityMapping{}, Scope{CompanyCodes: []string{"1000", "2000"}}); len(got) != 1 || got[0] != "" {
		t.Fatal("company filter")
	}

	fail := func(name string, h http.HandlerFunc) {
		t.Helper()
		s := httptest.NewServer(h)
		defer s.Close()
		src := &ODataSource{BaseURL: s.URL, PageSize: 10, Entities: []EntityMapping{DefaultEntities[0]}}
		if _, err := src.Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	fail("non-200", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusForbidden) })
	fail("bad JSON", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) })
	fail("truncated body", func(w http.ResponseWriter, r *http.Request) {
		conn, buf, _ := w.(http.Hijacker).Hijack()
		// The answer is meant to break off: a write or close that fails breaks it off sooner, the same failure to the source.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"d\":")
		_ = buf.Flush()
		_ = conn.Close()
	})
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := (&ODataSource{BaseURL: closed.URL, Entities: []EntityMapping{DefaultEntities[0]}}).Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}}); err == nil {
		t.Fatal("transport error")
	}
	if _, err := (&ODataSource{BaseURL: "http://bad host", Entities: []EntityMapping{DefaultEntities[0]}}).Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}}); err == nil {
		t.Fatal("bad URL")
	}
	// No explicit entities: the default catalogue is used.
	def := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"d":{"results":[]}}`))
	}))
	defer def.Close()
	if _, err := (&ODataSource{BaseURL: def.URL}).Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"}); err != nil {
		t.Fatalf("default entities: %v", err)
	}
	_ = bufio.NewReader
}
