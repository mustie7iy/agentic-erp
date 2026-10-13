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
	pos, invs, vendors := map[string]bool{}, map[string]bool{}, map[string]bool{}
	out := &Extract{}
	for _, r := range all.T001 {
		if inCC(r.BUKRS) {
			out.T001 = append(out.T001, r)
		}
	}
	for _, r := range all.SKB1 {
		if inCC(r.BUKRS) {
			out.SKB1 = append(out.SKB1, r)
		}
	}
	for _, r := range all.LFB1 {
		if inCC(r.BUKRS) {
			out.LFB1 = append(out.LFB1, r)
			vendors[r.LIFNR] = true
		}
	}
	for _, r := range all.LFA1 {
		if vendors[r.LIFNR] {
			out.LFA1 = append(out.LFA1, r)
		}
	}
	for _, r := range all.LFBK {
		if vendors[r.LIFNR] {
			out.LFBK = append(out.LFBK, r)
		}
	}
	for _, r := range all.EKKO {
		if inCC(r.BUKRS) {
			out.EKKO = append(out.EKKO, r)
			pos[r.EBELN] = true
		}
	}
	for _, r := range all.EKPO {
		if pos[r.EBELN] {
			out.EKPO = append(out.EKPO, r)
		}
	}
	for _, r := range all.MSEG {
		if pos[r.EBELN] {
			out.MSEG = append(out.MSEG, r)
		}
	}
	for _, r := range all.RBKP {
		if inCC(r.BUKRS) && r.GJAHR == scope.FiscalYear {
			out.RBKP = append(out.RBKP, r)
			invs[r.BELNR+"/"+r.GJAHR] = true
		}
	}
	for _, r := range all.RSEG {
		if invs[r.BELNR+"/"+r.GJAHR] {
			out.RSEG = append(out.RSEG, r)
		}
	}
	for _, r := range all.ACDOCA {
		if r.RLDNR == "0L" && inCC(r.RBUKRS) && r.GJAHR == scope.FiscalYear {
			out.ACDOCA = append(out.ACDOCA, r)
		}
	}
	// Customizing: company-specific tables by company code; client-wide
	// tables (payment terms, roles) unchanged.
	out.T052 = all.T052
	for _, r := range all.T007A {
		if inCC(r.BUKRS) {
			out.T007A = append(out.T007A, r)
		}
	}
	for _, r := range all.T030 {
		if inCC(r.BUKRS) {
			out.T030 = append(out.T030, r)
		}
	}
	for _, r := range all.T169G {
		if inCC(r.BUKRS) {
			out.T169G = append(out.T169G, r)
		}
	}
	for _, r := range all.T012K {
		if inCC(r.BUKRS) {
			out.T012K = append(out.T012K, r)
		}
	}
	out.AGR_DEFINE, out.AGR_TCODES, out.AGR_USERS = all.AGR_DEFINE, all.AGR_TCODES, all.AGR_USERS
	return out
}
