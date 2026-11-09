package sap

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// LeadingLedger is SAP's standard leading ledger (ACDOCA RLDNR "0L"), as the
// SAP specialist gave it (PO 2026-11-09, high confidence), to be confirmed
// on the pilot system in FINSC_LEDGER, whose "Leading ledger" flag marks
// it. The other ledgers (local GAAP such as 2L or L1, extension ledgers
// such as E1, which hold delta lines only) are never added to it.
const LeadingLedger = "0L"

// Scope selects what to migrate. Migrating one company code and fiscal year
// at a time is what makes cutover incremental and reversible. Ledger is the
// leading ledger whose journal lines are migrated, LeadingLedger when
// empty: a system whose FINSC_LEDGER flags another sets it.
type Scope struct {
	CompanyCodes []string `json:"companyCodes"`
	FiscalYear   string   `json:"fiscalYear"`
	Ledger       string   `json:"ledger,omitempty"`
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
// fiscal year, and of ACDOCA only the scope's leading ledger's lines. It
// counts the scope's lines of each other ledger in OtherLedgers, which the
// export should have left out (the SAP specialist: WHERE RLDNR = '0L'), so
// that the migration's quality gate refuses them as a safety net rather
// than the run proceeding as if the extract were whole (review:T56h4).
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
	out.VendorsWithoutPartner = keep(all.VendorsWithoutPartner, func(lifnr string) bool { return vendors[lifnr] })
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
	filterLedger(all, out, scope, inCC)
	filterCenters(all, out, inCC)
	// Customizing: company-specific tables by company code; client-wide
	// tables (payment terms, payment methods per country, roles) unchanged.
	out.T052 = all.T052
	out.T007A = keep(all.T007A, func(r T007A) bool { return inCC(r.BUKRS) })
	out.T030 = keep(all.T030, func(r T030) bool { return inCC(r.BUKRS) })
	out.T169G = keep(all.T169G, func(r T169G) bool { return inCC(r.BUKRS) })
	out.T012K = keep(all.T012K, func(r T012K) bool { return inCC(r.BUKRS) })
	out.T074 = keep(all.T074, func(r T074) bool { return inCC(r.BUKRS) })
	out.T042Z = all.T042Z // per country, client-wide
	out.T042E = keep(all.T042E, func(r T042E) bool { return inCC(r.ZBUKR) })
	out.T042A = keep(all.T042A, func(r T042A) bool { return inCC(r.ZBUKR) })
	out.T042I = keep(all.T042I, func(r T042I) bool { return inCC(r.ZBUKR) })
	out.TKA3A = keep(all.TKA3A, func(r TKA3A) bool { return inCC(r.BUKRS) })
	out.TKA3C = keep(all.TKA3C, func(r TKA3C) bool { return inCC(r.BUKRS) })
	out.TKA3P = keep(all.TKA3P, func(r TKA3P) bool { return inCC(r.BUKRS) })
	out.AGR_DEFINE, out.AGR_TCODES, out.AGR_USERS = all.AGR_DEFINE, all.AGR_TCODES, all.AGR_USERS
	return out
}

// filterLedger keeps the scope's journal lines of its leading ledger and
// counts those of the others.
func filterLedger(all, out *Extract, scope Scope, inCC func(string) bool) {
	out.Ledger = cmp.Or(scope.Ledger, LeadingLedger)
	for _, r := range all.ACDOCA {
		switch {
		case !inCC(r.RBUKRS) || r.GJAHR != scope.FiscalYear:
		case r.RLDNR == out.Ledger:
			out.ACDOCA = append(out.ACDOCA, r)
		default:
			if out.OtherLedgers == nil {
				out.OtherLedgers = map[string]int{}
			}
			out.OtherLedgers[r.RLDNR]++
		}
	}
}

// filterCenters keeps the controlling master data of the scope's company
// codes: their cost centers (CSKS) with every slice of each, also one in
// another company code, which the quality gate refuses rather than load a
// truncated center, the profit centers assigned to them
// (CEPC_BUKRS) with every company assignment of those, so a run sees
// what a profit center shares with other companies, the segments those
// profit centers name, and the texts of all of them.
func filterCenters(all, out *Extract, inCC func(string) bool) {
	costCenters := map[string]bool{}
	for _, r := range all.CSKS {
		if inCC(r.BUKRS) {
			costCenters[r.KOKRS+"/"+r.KOSTL] = true
		}
	}
	out.CSKS = keep(all.CSKS, func(r CSKS) bool { return costCenters[r.KOKRS+"/"+r.KOSTL] })
	out.CSKT = keep(all.CSKT, func(r CSKT) bool { return costCenters[r.KOKRS+"/"+r.KOSTL] })
	profitCenters := map[string]bool{}
	for _, r := range all.CEPC_BUKRS {
		if inCC(r.BUKRS) {
			profitCenters[r.KOKRS+"/"+r.PRCTR] = true
		}
	}
	out.CEPC_BUKRS = keep(all.CEPC_BUKRS, func(r CEPCBukrs) bool { return profitCenters[r.KOKRS+"/"+r.PRCTR] })
	out.CEPC = keep(all.CEPC, func(r CEPC) bool { return profitCenters[r.KOKRS+"/"+r.PRCTR] })
	out.CEPCT = keep(all.CEPCT, func(r CEPCT) bool { return profitCenters[r.KOKRS+"/"+r.PRCTR] })
	segments := map[string]bool{}
	for _, r := range out.CEPC {
		segments[r.SEGMENT] = true
	}
	out.FAGL_SEGM = keep(all.FAGL_SEGM, func(r FAGLSegm) bool { return segments[r.SEGMENT] })
	out.FAGL_SEGMT = keep(all.FAGL_SEGMT, func(r FAGLSegmt) bool { return segments[r.SEGMENT] })
	filterAreas(all, out)
}

// filterAreas keeps what belongs to the controlling areas of the centers
// kept (T56h2): their standard hierarchies (TKA01), every cost and profit
// center group of theirs (sets 0101 and 0106 with their texts, subsets and
// values), which the load expands against the centers, and their cost
// elements (CSKB). The extract does not say which controlling area a
// company code without centers is assigned to (TKA02, not read), so its
// scope keeps none of them.
func filterAreas(all, out *Extract) {
	areas := map[string]bool{}
	for _, r := range out.CSKS {
		areas[r.KOKRS] = true
	}
	for _, r := range out.CEPC {
		areas[r.KOKRS] = true
	}
	group := func(class, area string) bool {
		return (class == SetClassCostCenters || class == SetClassProfitCenters) && areas[area]
	}
	out.TKA01 = keep(all.TKA01, func(r TKA01) bool { return areas[r.KOKRS] })
	out.SETHEADER = keep(all.SETHEADER, func(r SETHEADER) bool { return group(r.SETCLASS, r.SUBCLASS) })
	out.SETHEADERT = keep(all.SETHEADERT, func(r SETHEADERT) bool { return group(r.SETCLASS, r.SUBCLASS) })
	out.SETNODE = keep(all.SETNODE, func(r SETNODE) bool { return group(r.SETCLASS, r.SUBCLASS) })
	out.SETLEAF = keep(all.SETLEAF, func(r SETLEAF) bool { return group(r.SETCLASS, r.SUBCLASS) })
	out.CSKB = keep(all.CSKB, func(r CSKB) bool { return areas[r.KOKRS] })
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
