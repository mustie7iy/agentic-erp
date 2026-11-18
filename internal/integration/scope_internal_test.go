package integration

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestRunExtractsCompaniesInOrder: a run's company codes are extracted in
// sorted order, never a map's, so that two runs of a scope report their
// quality issues and opening entries alike (review:batch77).
func TestRunExtractsCompaniesInOrder(t *testing.T) {
	entities := map[string]tenancy.Entity{}
	for _, code := range []string{"7000", "1000", "5000", "3000", "2000", "8000", "4000", "6000"} {
		entities[code] = tenancy.Entity{}
	}
	for i := range 20 {
		s := extractScope(entities, 2026)
		if got, want := fmt.Sprint(s.CompanyCodes), "[1000 2000 3000 4000 5000 6000 7000 8000]"; got != want {
			t.Fatalf("company codes, call %d: got %s, want %s", i, got, want)
		}
		if s.FiscalYear != "2026" {
			t.Fatalf("fiscal year: got %q, want 2026", s.FiscalYear)
		}
	}
}

// TestBankDetailsID: the quality gate names an account by its bank
// details ID, or "none" for a file extract's, which has none
// (review:batch77).
func TestBankDetailsID(t *testing.T) {
	for want, b := range map[string]sap.LFBK{"0001": {BKVID: "0001"}, "none": {}} {
		if got := bankDetailsID(b); got != want {
			t.Errorf("bankDetailsID(%+v): got %q, want %q", b, got, want)
		}
	}
}
