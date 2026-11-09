package sap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	if len(us.TCURX) != len(all.TCURX) || len(all.TCURX) == 0 {
		t.Errorf("TCURX rows in scope: got %d, want all %d, client-wide", len(us.TCURX), len(all.TCURX))
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
	if len(us.T074) != 0 {
		t.Errorf("T074 rows of company 1000: got %d, want 0", len(us.T074))
	}
	ca := FilterScope(all, Scope{CompanyCodes: []string{"3000"}, FiscalYear: "2026"})
	if len(ca.T074) != 1 || ca.Counts()["T074"] != 1 {
		t.Errorf("T074 rows of company 3000: got %d, want 1", len(ca.T074))
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

// TestFilterScopeKeepsTheScopesCenters: a scope keeps its company codes'
// cost centers with every slice, one in another company code too (which
// the gate refuses), the profit centers assigned to them with all their
// assignments, the segments those name, and the texts of each (T56h).
func TestFilterScopeKeepsTheScopesCenters(t *testing.T) {
	all := GenerateMock(42, "2026")
	all.CSKS = append(all.CSKS, CSKS{KOKRS: mockArea, KOSTL: "1100", BUKRS: "1000"})
	all.CSKT = append(all.CSKT, CSKT{KOKRS: mockArea, KOSTL: "1100", SPRAS: "E", KTEXT: "US"})
	all.CSKS = append(all.CSKS, CSKS{KOKRS: mockArea, KOSTL: "3300", BUKRS: "1000"}) // a slice of 3000's 3300 in 1000
	all.CEPC = append(all.CEPC, CEPC{KOKRS: mockArea, PRCTR: "P1100", SEGMENT: "SEG-US"})
	all.CEPC_BUKRS = append(all.CEPC_BUKRS, CEPCBukrs{KOKRS: mockArea, PRCTR: "P1100", BUKRS: "1000"}, CEPCBukrs{KOKRS: mockArea, PRCTR: "P3100", BUKRS: "1000"})
	all.FAGL_SEGM = append(all.FAGL_SEGM, FAGLSegm{SEGMENT: "SEG-US"})
	ca := FilterScope(all, Scope{CompanyCodes: []string{"3000"}, FiscalYear: "2026"})
	for table, want := range map[string]int{"CSKS": 6, "CSKT": 5, "CEPC": 3, "CEPCT": 2, "CEPC_BUKRS": 3, "FAGL_SEGM": 1, "FAGL_SEGMT": 1} {
		if got := ca.Counts()[table]; got != want {
			t.Errorf("%s rows of company 3000: got %d, want %d", table, got, want)
		}
	}
	us := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	for table, want := range map[string]int{"CSKS": 3, "CSKT": 2, "CEPC": 2, "CEPCT": 1, "CEPC_BUKRS": 3, "FAGL_SEGM": 2, "FAGL_SEGMT": 1} {
		if got := us.Counts()[table]; got != want {
			t.Errorf("%s rows of company 1000: got %d, want %d", table, got, want)
		}
	}
	de := FilterScope(all, Scope{CompanyCodes: []string{"2000"}, FiscalYear: "2026"})
	if n := len(de.CSKS) + len(de.CEPC) + len(de.FAGL_SEGM) + len(de.SETHEADER); n != 0 {
		t.Errorf("controlling rows of company 2000: got %d, want none", n)
	}
	if fmt.Sprint(de.TKA02) != "[{2000 2000}]" {
		t.Errorf("TKA02 of company 2000: got %v, want its own row alone", de.TKA02)
	}
}

// TestFilterScopeKeepsTheAssignedAreasCostElements: a company code
// without centers keeps the cost elements (CSKB) of the controlling area
// TKA02 assigns it to (A8), but not that area's groups, which place
// centers; without TKA02 it keeps none.
func TestFilterScopeKeepsTheAssignedAreasCostElements(t *testing.T) {
	all := GenerateMock(42, "2026")
	all.CSKB = append(all.CSKB, CSKB{KOKRS: mockAreaOther, KSTAR: "0000510000", DATBI: openEnded, KATYP: "01"})
	all.SETHEADER = append(all.SETHEADER, SETHEADER{SETCLASS: SetClassCostCenters, SUBCLASS: mockAreaOther, SETNAME: "EU"})
	us := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	if fmt.Sprint(us.CSKB) != "[{2000 0000510000 99991231 01}]" {
		t.Errorf("cost elements of company 1000: got %v, want area 2000's", us.CSKB)
	}
	if len(us.SETHEADER) != 0 {
		t.Errorf("groups of company 1000: got %v, want none", us.SETHEADER)
	}
	all.TKA02 = nil
	if us := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"}); len(us.CSKB) != 0 {
		t.Errorf("cost elements of company 1000 without TKA02: got %v, want none", us.CSKB)
	}
}

// TestMockCenters: the mock's company 3000 has a segment, two profit
// centers (P3200 locked from 1 October) and four cost centers (3120 moving
// to P3200 on 1 July), each slice ending the day before the next starts.
func TestMockCenters(t *testing.T) {
	e := GenerateMock(42, "2026")
	var got []string
	for _, c := range e.CSKS {
		got = append(got, c.KOSTL+" "+c.DATAB+"-"+c.DATBI+" "+c.PRCTR)
	}
	want := []string{"3100 20200101-99991231 P3100", "3120 20200101-20260630 P3100", "3120 20260701-99991231 P3200", "3200 20200101-99991231 P3200",
		"3300 20200101-99991231 P3100"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cost center slices: got %v, want %v", got, want)
	}
	if e.CEPC[1].DATBI != "20260930" || e.CEPC[2].DATAB != "20261001" || e.CEPC[2].LOCK_IND != "X" {
		t.Errorf("P3200's slices: got %v, want unlocked to 30 September, locked from 1 October", e.CEPC[1:])
	}
	if dayBefore("20260301") != "20260228" {
		t.Errorf("the day before 1 March 2026: got %s, want 20260228", dayBefore("20260301"))
	}
}

// TestMockLineCenters: company 3000's payroll lines name cost center 3100,
// its purchases 3120, both before 3120 moves on 1 July, each with its
// profit center and segment, and its sales revenue profit center P3100
// alone; the other companies' lines and 3000's balance sheet lines name
// none (T56h4).
func TestMockLineCenters(t *testing.T) {
	e := GenerateMock(42, "2026")
	got := map[string]map[string]bool{}
	for _, l := range e.ACDOCA {
		k := l.RBUKRS + " " + l.RACCT
		if got[k] == nil {
			got[k] = map[string]bool{}
		}
		got[k][l.RCNTR+"/"+l.PRCTR+"/"+l.SEGMENT] = true
		if l.RCNTR == "3120" && l.BUDAT >= "20260701" {
			t.Errorf("line %s/%s on 3120 dated %s: want every purchase before 1 July", l.BELNR, l.DOCLN, l.BUDAT)
		}
	}
	for k, want := range map[string]string{"3000 0000610000": "3100/P3100/SEG-CAN", "3000 0000520000": "3120/P3100/SEG-CAN",
		"3000 0000400000": "/P3100/SEG-CAN", "3000 0000100000": "//", "3000 0000211000": "//", "1000 0000610000": "//", "2000 0000520000": "//"} {
		if fmt.Sprint(got[k]) != fmt.Sprintf("map[%s:true]", want) {
			t.Errorf("centers of the lines on %s: got %v, want %s alone", k, got[k], want)
		}
	}
}

// TestFilterScopeKeepsTheLeadingLedger: a scope keeps the journal lines of
// its leading ledger, 0L unless it names another (review:T56h4), and counts
// its lines of the other ledgers, by RLDNR, leaving out those of other
// company codes and years.
func TestFilterScopeKeepsTheLeadingLedger(t *testing.T) {
	line := func(ledger, company, year string) ACDOCA { return ACDOCA{RLDNR: ledger, RBUKRS: company, GJAHR: year} }
	all := &Extract{ACDOCA: []ACDOCA{line("0L", "1000", "2026"), line("2L", "1000", "2026"), line("2L", "1000", "2026"), line("", "1000", "2026"),
		line("E1", "2000", "2026"), line("E1", "1000", "2025")}}
	for name, tc := range map[string]struct {
		ledger, want string
		others       map[string]int
	}{
		"the standard leading ledger": {"", "0L 1", map[string]int{"2L": 2, "": 1}},
		"a ledger the scope names":    {"2L", "2L 2", map[string]int{"0L": 1, "": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			got := FilterScope(all, Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026", Ledger: tc.ledger})
			if kept := fmt.Sprintf("%s %d", got.Ledger, len(got.ACDOCA)); kept != tc.want {
				t.Errorf("ledger and lines kept: got %s, want %s", kept, tc.want)
			}
			if fmt.Sprint(got.OtherLedgers) != fmt.Sprint(tc.others) {
				t.Errorf("other ledgers' lines: got %v, want %v", got.OtherLedgers, tc.others)
			}
		})
	}
}
