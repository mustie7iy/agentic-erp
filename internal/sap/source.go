package sap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Scope selects what to migrate. Migrating one company code and fiscal year
// at a time is what makes cutover incremental and reversible.
type Scope struct {
	CompanyCodes []string `json:"companyCodes"`
	FiscalYear   string   `json:"fiscalYear"`
}

// Source delivers SAP data for a scope: a file extract, the mock, or live
// OData APIs. Migration code depends only on this interface.
type Source interface {
	Name() string
	Extract(ctx context.Context, scope Scope) (*Extract, error)
}

// FileSource reads a JSON extract from disk.
type FileSource struct{ Path string }

func (f FileSource) Name() string { return "file:" + f.Path }

func (f FileSource) Extract(_ context.Context, scope Scope) (*Extract, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, fmt.Errorf("sap: read extract %s: %w", f.Path, err)
	}
	var all Extract
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("sap: parse extract %s: %w", f.Path, err)
	}
	return FilterScope(&all, scope), nil
}

// WriteExtract saves an extract as JSON.
func WriteExtract(path string, e *Extract) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("sap: create directory of extract %s: %w", path, err)
	}
	data, _ := json.MarshalIndent(e, "", "  ") // plain data: cannot fail
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("sap: write extract %s: %w", path, err)
	}
	return nil
}

// FilterScope keeps only rows belonging to the scope's company codes and
// fiscal year (leading ledger only for ACDOCA).
func FilterScope(all *Extract, scope Scope) *Extract {
	inCC := func(cc string) bool { return slices.Contains(scope.CompanyCodes, cc) }
	out := &Extract{}
	out.T001 = keep(all.T001, func(r T001) bool { return inCC(r.BUKRS) })
	out.SKB1 = keep(all.SKB1, func(r SKB1) bool { return inCC(r.BUKRS) })
	out.LFB1 = keep(all.LFB1, func(r LFB1) bool { return inCC(r.BUKRS) })
	vendors := map[string]bool{}
	for _, r := range out.LFB1 {
		vendors[r.LIFNR] = true
	}
	out.LFA1 = keep(all.LFA1, func(r LFA1) bool { return vendors[r.LIFNR] })
	out.LFBK = keep(all.LFBK, func(r LFBK) bool { return vendors[r.LIFNR] })
	out.EKKO = keep(all.EKKO, func(r EKKO) bool { return inCC(r.BUKRS) })
	pos := map[string]bool{}
	for _, r := range out.EKKO {
		pos[r.EBELN] = true
	}
	out.EKPO = keep(all.EKPO, func(r EKPO) bool { return pos[r.EBELN] })
	out.MSEG = keep(all.MSEG, func(r MSEG) bool { return pos[r.EBELN] })
	out.RBKP = keep(all.RBKP, func(r RBKP) bool { return inCC(r.BUKRS) && r.GJAHR == scope.FiscalYear })
	invs := map[string]bool{}
	for _, r := range out.RBKP {
		invs[r.BELNR+"/"+r.GJAHR] = true
	}
	out.RSEG = keep(all.RSEG, func(r RSEG) bool { return invs[r.BELNR+"/"+r.GJAHR] })
	out.ACDOCA = keep(all.ACDOCA, func(r ACDOCA) bool { return r.RLDNR == "0L" && inCC(r.RBUKRS) && r.GJAHR == scope.FiscalYear })
	// Customizing: company-specific tables by company code; client-wide
	// tables (payment terms, roles) unchanged.
	out.T052 = all.T052
	out.T007A = keep(all.T007A, func(r T007A) bool { return inCC(r.BUKRS) })
	out.T030 = keep(all.T030, func(r T030) bool { return inCC(r.BUKRS) })
	out.T169G = keep(all.T169G, func(r T169G) bool { return inCC(r.BUKRS) })
	out.T012K = keep(all.T012K, func(r T012K) bool { return inCC(r.BUKRS) })
	out.AGR_DEFINE, out.AGR_TCODES, out.AGR_USERS = all.AGR_DEFINE, all.AGR_TCODES, all.AGR_USERS
	return out
}

// keep returns the rows that match, in order. It returns nil, not an empty
// slice, when none do, so an empty table is written as null in an extract.
func keep[T any](rows []T, match func(T) bool) []T {
	var out []T
	for _, r := range rows {
		if match(r) {
			out = append(out, r)
		}
	}
	return out
}
