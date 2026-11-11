package sap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFilterScopeKeepsEveryCompanyID: a vendor's trading partner is
// usually a company outside the scope, so the scope keeps the company
// codes of every company ID of the source (T001-RCOMP, without blanks and
// ALPHA zeros), each once and in order, a company ID two company codes
// share with both; a company code without one names none, and a source
// without any holds no map. What a source says of its trading partners is kept too.
func TestFilterScopeKeepsEveryCompanyID(t *testing.T) {
	all := &Extract{T001: []T001{{BUKRS: "3000", RCOMP: "003000"}, {BUKRS: "1000", RCOMP: " 001000 "}, {BUKRS: "2100", RCOMP: "2000"},
		{BUKRS: "2000", RCOMP: "002000"}, {BUKRS: "2000", RCOMP: "002000"}, {BUKRS: "4000"}, {BUKRS: "5000", RCOMP: "  "}}, NoTradingPartners: true}
	out := FilterScope(all, Scope{CompanyCodes: []string{"3000"}, FiscalYear: "2026"})
	if len(out.T001) != 1 || out.T001[0].BUKRS != "3000" {
		t.Errorf("company codes kept: got %v, want 3000 alone", out.T001)
	}
	if got, want := fmt.Sprint(out.CompanyCodes), "map[1000:[1000] 2000:[2000 2100] 3000:[3000]]"; got != want {
		t.Errorf("company codes by company ID: got %s, want %s", got, want)
	}
	if !out.NoTradingPartners {
		t.Error("a source without trading partners: got the scope saying it carries them")
	}
	if got := FilterScope(&Extract{T001: []T001{{BUKRS: "1000"}}}, Scope{CompanyCodes: []string{"1000"}}); got.CompanyCodes != nil || got.NoTradingPartners {
		t.Errorf("a source without company IDs: got %v and %t, want no map and false", got.CompanyCodes, got.NoTradingPartners)
	}
}

// TestMockTradingPartner: the mock's company codes each have a company ID,
// and company 3000's vendor Muskoka Office Supply names company 2000's as its
// trading partner; its vendor lines carry it and no other line does, as
// SAP leaves the lines invoice verification posts (po:2026-11-09, A2).
func TestMockTradingPartner(t *testing.T) {
	e := GenerateMock(42, "2026")
	for _, c := range e.T001 {
		if c.RCOMP != "00"+c.BUKRS {
			t.Errorf("company code %s: got company ID %q, want 00%s", c.BUKRS, c.RCOMP, c.BUKRS)
		}
	}
	for _, v := range e.LFA1 {
		want := ""
		if v.LIFNR == intercompanyVendor {
			want = intercompanyPartner
		}
		if v.VBUND != want {
			t.Errorf("vendor %s %s: got trading partner %q, want %q", v.LIFNR, v.NAME1, v.VBUND, want)
		}
	}
	vendorLines := 0
	for _, l := range e.ACDOCA {
		want := ""
		if l.LIFNR == intercompanyVendor {
			want = intercompanyPartner
			vendorLines++
		}
		if l.RASSC != want {
			t.Errorf("line %s/%s/%s of vendor %q: got trading partner %q, want %q", l.RBUKRS, l.BELNR, l.DOCLN, l.LIFNR, l.RASSC, want)
		}
	}
	if vendorLines == 0 {
		t.Error("the intercompany vendor's lines: got none, want some")
	}
}

// TestODataCarriesNoTradingPartner: the default OData mapping reads no
// trading partner (the SAP specialist named the tables' fields, not the
// APIs' properties), so its extract says it carries none; a mapping that
// reads LFA1-VBUND does not say so.
func TestODataCarriesNoTradingPartner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A failed write reaches the source as a broken answer, which the test asserts on.
		_, _ = w.Write([]byte(`{"d":{"results":[{"Supplier": "0000100001", "CompanyCode": "1000", "TradingPartner": "001000"}]}}`))
	}))
	defer srv.Close()
	scope := Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"}
	src := &ODataSource{BaseURL: srv.URL, Entities: []EntityMapping{entity(t, "LFA1"), entity(t, "LFB1")}}
	e, err := src.Extract(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if !e.NoTradingPartners || e.LFA1[0].VBUND != "" {
		t.Errorf("the default mapping: got NoTradingPartners %t and VBUND %q, want true and none", e.NoTradingPartners, e.LFA1[0].VBUND)
	}
	vendors := entity(t, "LFA1")
	vendors.Fields = map[string]string{"LIFNR": "Supplier", "VBUND": "TradingPartner"}
	src.Entities = []EntityMapping{vendors, entity(t, "LFB1")}
	if e, err = src.Extract(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if e.NoTradingPartners || e.LFA1[0].VBUND != "001000" {
		t.Errorf("a mapping that reads it: got NoTradingPartners %t and VBUND %q, want false and 001000", e.NoTradingPartners, e.LFA1[0].VBUND)
	}
}
