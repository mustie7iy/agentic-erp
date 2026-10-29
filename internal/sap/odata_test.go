package sap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
		json.Number("3"):             "3",
		json.Number("0.00"):          "0",
	}
	for in, want := range cases {
		t.Run(fmt.Sprintf("%T(%v)", in, in), func(t *testing.T) {
			if got := odataValue(in); got != want {
				t.Errorf("odataValue(%v): got %q, want %q", in, got, want)
			}
		})
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
	if got := e.EKKO[0].BEDAT; got != "20260101" {
		t.Errorf("BEDAT: got %q, want %q", got, "20260101")
	}
	if got := e.EKKO[0].WAERS; got != "USD" {
		t.Errorf("WAERS: got %q, want %q", got, "USD")
	}
	if len(filters) != 3 || filters[0] != "CompanyCode eq '1000'" {
		t.Fatalf("unexpected paging/filters: %v", filters)
	}
}

// TestODataInvoicesHaveNoKindIndicator: the OData mapping does not carry
// RBKP-XRECH, so an extracted invoice's indicator is absent (nil), never a
// blank that would read as a credit memo; the quality gate refuses it
// (T72b).
func TestODataInvoicesHaveNoKindIndicator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := map[string]any{"SupplierInvoice": "5100000001", "FiscalYear": "2026", "CompanyCode": "1000", "SupplierInvoiceStatus": "A"}
		// A failed write reaches the source as a broken answer, which the test asserts on.
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{"results": []map[string]any{row}}})
	}))
	defer srv.Close()
	var rbkp EntityMapping
	for _, em := range DefaultEntities {
		if em.Table == "RBKP" {
			rbkp = em
		}
	}
	src := &ODataSource{BaseURL: srv.URL, Entities: []EntityMapping{rbkp}}
	e, err := src.Extract(context.Background(), Scope{CompanyCodes: []string{"1000"}, FiscalYear: "2026"})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.RBKP) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(e.RBKP))
	}
	if e.RBKP[0].XRECH != nil {
		t.Errorf("XRECH: got %q, want absent (nil)", *e.RBKP[0].XRECH)
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

// A number the service sends unquoted keeps every digit: amounts beyond a
// float64's 15-17 significant digits are not rounded on the way in.
func TestODataNumbersKeepEveryDigit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"d":{"results":[{"Amount": 12345678901234567.89, "Count": 9007199254740993, "Small": 0.1, "Exp": 1.5e3, "Huge": 1e999999}]}}`))
	}))
	defer srv.Close()
	results, err := (&ODataSource{}).page(context.Background(), srv.Client(), srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := odataRows(EntityMapping{Fields: map[string]string{"HSL": "Amount", "N": "Count", "S": "Small", "E": "Exp", "H": "Huge"}}, results)
	for field, want := range map[string]string{"HSL": "12345678901234567.89", "N": "9007199254740993", "S": "0.1", "E": "1500", "H": "1e999999"} {
		if got := rows[0][field]; got != want {
			t.Errorf("%s: got %q, want %q", field, got, want)
		}
	}
}

// Numbers that cannot be spelled out cheaply and exactly keep their literal:
// one past what big.Float holds (it would read as +Inf or 0), and one longer
// than any amount, whose spelling would cost time growing with the square of
// its length.
func TestODataNumbersOutOfRangeKeepTheirLiteral(t *testing.T) {
	long := strings.Repeat("1", 100) + "e-50"
	for name, in := range map[string]string{"overflow": "1e646456993", "underflow": "1e-646457000", "too long": long} {
		t.Run(name, func(t *testing.T) {
			if got := decimalText(json.Number(in)); got != in {
				t.Errorf("got %.40q, want the literal %.40q", got, in)
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
