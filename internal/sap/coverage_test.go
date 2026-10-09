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
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644)
	if _, err := (FileSource{Path: filepath.Join(dir, "bad.json")}).Extract(context.Background(), Scope{}); err == nil {
		t.Fatal("bad json")
	}
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o644)
	if err := WriteExtract(filepath.Join(blocker, "a", "x.json"), &Extract{}); err == nil {
		t.Fatal("directory creation failure")
	}
}

func TestFilterScopeKeepsOnlyTheScope(t *testing.T) {
	all := GenerateMock(42, "2026")
	us := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	for _, r := range us.SKB1 {
		if r.BUKRS != "1000" {
			t.Fatal("SKB1 leak")
		}
	}
	for _, r := range us.T007A {
		if r.BUKRS != "1000" {
			t.Fatal("T007A leak")
		}
	}
	if len(us.T030) != 2 || len(us.T169G) != 1 || len(us.T012K) != 1 || len(us.T052) != len(all.T052) || len(us.AGR_USERS) != len(all.AGR_USERS) {
		t.Fatal("customizing scope")
	}
	if len(us.EKPO) == 0 || len(us.MSEG) == 0 || len(us.RSEG) == 0 || len(us.LFBK) == 0 || len(us.ACDOCA) == 0 {
		t.Fatal("dependent tables filtered away")
	}
	if wrongYear := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2025"}); len(wrongYear.RBKP) != 0 || len(wrongYear.ACDOCA) != 0 {
		t.Fatal("year filter")
	}
	c := us.Counts()
	if c["T001"] != 1 || c["AGR_TCODES"] == 0 {
		t.Fatalf("counts: %v", c)
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
		json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{"results": []map[string]any{
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
	if err != nil || len(e.ACDOCA) != 1 || e.ACDOCA[0].RLDNR != "0L" || e.ACDOCA[0].XBLNR != "map[x:1]" {
		t.Fatalf("extract: %v %+v", err, e)
	}
	q := seen[0]
	for _, want := range []string{"sap-client=100", "%24top=1000", "FiscalYear+eq+%272026%27"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s: %s", want, q)
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
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"d\":")
		buf.Flush()
		conn.Close()
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
