package sap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestODataValue(t *testing.T) {
	cases := map[any]string{
		true:                         "X",
		false:                        "",
		nil:                          "",
		"/Date(1767225600000)/":      "20260101",
		"/Date(1767225600000+0000)/": "20260101",
		"1234.50":                    "1234.50",
		float64(3):                   "3",
	}
	for in, want := range cases {
		if got := odataValue(in); got != want {
			t.Errorf("odataValue(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestODataSourcePagesAndFilters(t *testing.T) {
	const total = 5
	var filters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "user" || p != "pw" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/sap/opu/odata/sap/API_PURCHASEORDER_PROCESS_SRV/A_PurchaseOrder" {
			http.NotFound(w, r)
			return
		}
		filters = append(filters, r.URL.Query().Get("$filter"))
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		var results []map[string]any
		for i := skip; i < min(skip+top, total); i++ {
			results = append(results, map[string]any{
				"PurchaseOrder": "45" + strconv.Itoa(1000000+i), "CompanyCode": "1000", "Supplier": "100001",
				"DocumentCurrency": "USD", "PurchaseOrderDate": "/Date(1767225600000)/",
			})
		}
		// A failed write reaches the source as a broken answer, which the test asserts on.
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{"results": results}})
	}))
	defer srv.Close()

	src := &ODataSource{
		BaseURL: srv.URL, User: "user", Password: "pw", PageSize: 2,
		Entities: []EntityMapping{DefaultEntities[5]}, // EKKO
	}
	e, err := src.Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.EKKO) != total {
		t.Fatalf("want %d POs across pages, got %d", total, len(e.EKKO))
	}
	if e.EKKO[0].BEDAT != "20260101" || e.EKKO[0].WAERS != "USD" {
		t.Fatalf("field mapping wrong: %+v", e.EKKO[0])
	}
	if len(filters) != 3 || filters[0] != "CompanyCode eq '1000'" {
		t.Fatalf("unexpected paging/filters: %v", filters)
	}
}

// Scope values are OData string literals: a quote inside one is doubled, so
// it cannot end the literal and change the filter.
func TestODataFilterEscapesQuotes(t *testing.T) {
	var filter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter = r.URL.Query().Get("$filter")
		w.Write([]byte(`{"d":{"results":[]}}`))
	}))
	defer srv.Close()
	src := &ODataSource{BaseURL: srv.URL, Entities: []EntityMapping{{
		Table: "BKPF", Service: "S", Entity: "E", CompanyField: "CompanyCode", YearField: "FiscalYear",
	}}}
	cases := map[string]struct{ company, year, want string }{
		"plain values unchanged": {"1000", "2026", "CompanyCode eq '1000' and FiscalYear eq '2026'"},
		"quote in company code": {"10' or CompanyCode ne '", "2026",
			"CompanyCode eq '10'' or CompanyCode ne ''' and FiscalYear eq '2026'"},
		"quote in fiscal year": {"1000", "'", "CompanyCode eq '1000' and FiscalYear eq ''''"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := src.Extract(context.Background(), Scope{CompanyCodes: []string{tc.company}, FiscalYear: tc.year})
			if err != nil {
				t.Fatal(err)
			}
			if filter != tc.want {
				t.Errorf("got $filter %q, want %q", filter, tc.want)
			}
		})
	}
}

func TestMockIsBalancedAndDeterministic(t *testing.T) {
	a, b := GenerateMock(7, "2026"), GenerateMock(7, "2026")
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("same seed must produce identical extracts")
	}
}
