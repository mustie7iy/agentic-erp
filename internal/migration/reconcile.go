package migration

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Check is one reconciliation control: a figure computed independently from
// the SAP extract and from the new core, which must agree.
type Check struct {
	CompanyCode string `json:"companyCode"`
	Name        string `json:"name"`
	SAP         string `json:"sap"`
	Core        string `json:"core"`
	Pass        bool   `json:"pass"`
}

type Diff struct {
	CompanyCode string      `json:"companyCode"`
	Key         string      `json:"key"`
	SAP         money.Minor `json:"sap"`
	Core        money.Minor `json:"core"`
}

type Report struct {
	Scope  sap.Scope `json:"scope"`
	Checks []Check   `json:"checks"`
	Diffs  []Diff    `json:"diffs"`
	Passed bool      `json:"passed"`
}

// Reconcile proves the migration. The SAP side is recomputed straight from
// the raw extract (not from the transformed dataset), so a transform bug
// shows up as a difference instead of being reproduced on both sides.
//
// The core side is limited to the scope's fiscal year, so earlier years
// already in the store do not count against this one. Fiscal years are
// assumed to be calendar years.
func Reconcile(e *sap.Extract, s *core.Store, scope sap.Scope, m Mapping, mode Mode) *Report {
	r := &Report{Scope: scope}
	currency := map[string]string{}
	for _, c := range e.T001 {
		currency[c.BUKRS] = c.WAERS
	}
	recon := map[string]bool{}
	for _, a := range e.SKB1 {
		if a.MITKZ == "K" {
			recon[a.BUKRS+"/"+a.SAKNR] = true
		}
	}

	inYear := func(date string) bool {
		return scope.FiscalYear == "" || strings.HasPrefix(date, scope.FiscalYear+"-")
	}

	for _, cc := range scope.CompanyCodes {
		cur := currency[cc]
		dec := m.decimals(cur)
		fmtAmt := func(v money.Minor) string { return v.Format(dec) + " " + cur }

		// 1. Trial balance per account.
		sapTB := map[string]money.Minor{}
		sapOpen := map[string]money.Minor{}
		var sapDebits money.Minor
		for _, l := range e.ACDOCA {
			if l.RBUKRS != cc {
				continue
			}
			amt, err := m.parseAmount(l.HSL, cur)
			if err != nil {
				continue // already reported by CheckQuality
			}
			sapTB[alpha(l.RACCT)] += amt
			if amt > 0 {
				sapDebits += amt
			}
			if l.LIFNR != "" && l.AUGBL == "" && recon[cc+"/"+l.RACCT] {
				sapOpen[alpha(l.LIFNR)] += amt
			}
		}
		coreTB := map[string]money.Minor{}
		var coreDebits money.Minor
		for _, je := range s.Journal {
			if je.CompanyCode != cc || !inYear(je.PostingDate) {
				continue
			}
			for _, l := range je.Lines {
				coreTB[l.Account] += l.Amount
				if l.Amount > 0 {
					coreDebits += l.Amount
				}
			}
		}
		tbDiffs := diffMaps(cc, sapTB, coreTB)
		r.Diffs = append(r.Diffs, tbDiffs...)
		r.add(cc, "Trial balance: accounts with differences", "0", strconv.Itoa(len(tbDiffs)), len(tbDiffs) == 0)

		// Total debits only match when every document is replayed.
		if mode == ModeDocuments {
			r.add(cc, "Total debits posted", fmtAmt(sapDebits), fmtAmt(coreDebits), sapDebits == coreDebits)
		}

		// 2. Open payables per vendor.
		coreOpen := map[string]money.Minor{}
		for vendor, items := range s.VendorOpenItems(cc) {
			for _, it := range items {
				if inYear(it.PostingDate) {
					coreOpen[vendor] += it.Amount
				}
			}
		}
		apDiffs := diffMaps(cc, sapOpen, coreOpen)
		for i := range apDiffs {
			apDiffs[i].Key = "vendor " + apDiffs[i].Key
		}
		r.Diffs = append(r.Diffs, apDiffs...)
		r.add(cc, "Open payables total", fmtAmt(sum(sapOpen)), fmtAmt(sum(coreOpen)), sum(sapOpen) == sum(coreOpen))
		r.add(cc, "Open payables: vendors with differences", "0", strconv.Itoa(len(apDiffs)), len(apDiffs) == 0)

		// 3. Vendors assigned to the company.
		sapVendors := 0
		for _, v := range e.LFB1 {
			if v.BUKRS == cc {
				sapVendors++
			}
		}
		coreVendors := 0
		for _, v := range s.Vendors {
			if _, ok := v.APAccount[cc]; ok {
				coreVendors++
			}
		}
		r.add(cc, "Vendors", strconv.Itoa(sapVendors), strconv.Itoa(coreVendors), sapVendors == coreVendors)

		// 4. Purchase orders: count and total value.
		sapPOs, sapPOValue := 0, money.Minor(0)
		poCC := map[string]bool{}
		for _, h := range e.EKKO {
			if h.BUKRS == cc {
				sapPOs++
				poCC[h.EBELN] = true
			}
		}
		for _, i := range e.EKPO {
			if !poCC[i.EBELN] {
				continue
			}
			qty, _ := money.ParseQty(i.MENGE)
			price, _ := m.parseAmount(i.NETPR, cur)
			peinh, _ := strconv.ParseInt(i.PEINH, 10, 64)
			sapPOValue += money.Extend(qty, price, peinh)
		}
		corePOs, corePOValue := 0, money.Minor(0)
		for _, po := range s.PurchaseOrders {
			if po.CompanyCode != cc || !inYear(po.OrderDate) {
				continue
			}
			corePOs++
			for _, l := range po.Lines {
				corePOValue += money.Extend(l.Quantity, l.UnitPrice, l.PriceUnit)
			}
		}
		r.add(cc, "Purchase orders", strconv.Itoa(sapPOs), strconv.Itoa(corePOs), sapPOs == corePOs)
		r.add(cc, "Purchase order value", fmtAmt(sapPOValue), fmtAmt(corePOValue), sapPOValue == corePOValue)

		// 5. Supplier invoices: everything migrated, and what SAP itself posted.
		// Status is not compared beyond that: after go-live the platform may
		// have processed invoices SAP still shows as parked. Invoices SAP
		// posted carry no platform journal entry, which tells them apart.
		sapAll, sapPosted, coreAll, corePosted := 0, 0, 0, 0
		for _, h := range e.RBKP {
			if h.BUKRS == cc && (h.RBSTAT == "A" || h.RBSTAT == "5") {
				sapAll++
				if h.RBSTAT == "5" {
					sapPosted++
				}
			}
		}
		for _, inv := range s.Invoices {
			if inv.CompanyCode != cc || !strings.HasPrefix(inv.Source, "sap:") ||
				(scope.FiscalYear != "" && !strings.HasSuffix(inv.Source, "/"+scope.FiscalYear)) {
				continue
			}
			coreAll++
			if inv.Status == core.InvoicePosted && inv.JournalEntryID == "" {
				corePosted++
			}
		}
		r.add(cc, "Supplier invoices migrated", strconv.Itoa(sapAll), strconv.Itoa(coreAll), sapAll == coreAll)
		r.add(cc, "Supplier invoices posted in SAP", strconv.Itoa(sapPosted), strconv.Itoa(corePosted), sapPosted == corePosted)
	}
	r.Passed = !slices.ContainsFunc(r.Checks, func(c Check) bool { return !c.Pass })
	return r
}

func (r *Report) add(cc, name, sapVal, coreVal string, pass bool) {
	r.Checks = append(r.Checks, Check{CompanyCode: cc, Name: name, SAP: sapVal, Core: coreVal, Pass: pass})
}

func diffMaps(cc string, sapM, coreM map[string]money.Minor) []Diff {
	keys := map[string]bool{}
	for k := range sapM {
		keys[k] = true
	}
	for k := range coreM {
		keys[k] = true
	}
	var out []Diff
	for _, k := range sortedKeys(keys) {
		if sapM[k] != coreM[k] {
			out = append(out, Diff{CompanyCode: cc, Key: k, SAP: sapM[k], Core: coreM[k]})
		}
	}
	return out
}

func sum(m map[string]money.Minor) money.Minor {
	var t money.Minor
	for _, v := range m {
		t += v
	}
	return t
}

// Print renders the report as a table.
func (r *Report) Print(w io.Writer) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMPANY\tCHECK\tSAP\tCORE\tRESULT")
	for _, c := range r.Checks {
		res := "PASS"
		if !c.Pass {
			res = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.CompanyCode, c.Name, c.SAP, c.Core, res)
	}
	tw.Flush()
	if len(r.Diffs) > 0 {
		fmt.Fprintln(w, "\nDifferences:")
		tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "COMPANY\tKEY\tSAP\tCORE\tDIFF")
		for _, d := range r.Diffs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.CompanyCode, d.Key, d.SAP, d.Core, d.Core-d.SAP)
		}
		tw.Flush()
	}
	verdict := "PASSED - core matches SAP for this scope"
	if !r.Passed {
		verdict = "FAILED - do not cut over this scope"
	}
	fmt.Fprintf(w, "\nReconciliation %s\n", verdict)
}
