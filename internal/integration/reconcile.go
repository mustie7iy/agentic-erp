package integration

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Check is one reconciliation control: a figure computed independently
// from the SAP extract and from the platform, which must agree.
type Check struct {
	CompanyCode string `json:"company_code"`
	Name        string `json:"name"`
	SAP         string `json:"sap"`
	Platform    string `json:"platform"`
	Pass        bool   `json:"pass"`
}

// Diff is a difference behind a failed check, in minor units.
type Diff struct {
	CompanyCode string `json:"company_code"`
	Key         string `json:"key"` // account number, or "vendor <LIFNR>"
	SAP         string `json:"sap"`
	Platform    string `json:"platform"`
}

// Reconciliation is the reconciliation part of a run's report.
type Reconciliation struct {
	Checks  []Check  `json:"checks"`
	Diffs   []Diff   `json:"diffs"`
	Missing []string `json:"missing_documents,omitempty"` // SAP documents of the scope not on the platform
	Passed  bool     `json:"passed"`
}

// reconcile proves a load. The SAP side is recomputed from the raw extract,
// not from what was loaded, so a load bug shows up as a difference instead
// of being reproduced on both sides. Journal totals are the legal entity's
// for the fiscal year; counts cover the records this or earlier runs
// loaded for the scope's SAP keys.
func (e *Endpoints) reconcile(ctx context.Context, ext *sap.Extract, t target, r refs) (Reconciliation, error) {
	var rec Reconciliation
	add := func(cc, name, sapValue, platformValue string, pass bool) {
		rec.Checks = append(rec.Checks, Check{CompanyCode: cc, Name: name, SAP: sapValue, Platform: platformValue, Pass: pass})
	}
	vendorOf := map[uuid.UUID]string{}
	for key, id := range r {
		if lifnr, ok := strings.CutPrefix(key, "LFA1/"); ok {
			vendorOf[id] = alpha(lifnr)
		}
	}
	recon := map[string]bool{}
	for _, a := range ext.SKB1 {
		recon[a.BUKRS+"/"+a.SAKNR] = a.MITKZ == "K"
	}
	codes := make([]string, 0, len(ext.T001))
	currency := map[string]string{}
	for _, c := range ext.T001 {
		codes, currency[c.BUKRS] = append(codes, c.BUKRS), c.WAERS
	}
	slices.Sort(codes)
	for _, cc := range codes {
		le, cur := t.entities[cc], currency[cc]
		format := func(v int64) string { return money.Minor(v).Plain(t.exponents[cur]) + " " + cur }

		// 1. Trial balance per account, and debits when every document came over.
		sapTB, sapOpen := map[string]int64{}, map[string]int64{}
		var sapDebits int64
		documents := map[string]bool{}
		for _, l := range ext.ACDOCA {
			if l.RBUKRS != cc {
				continue
			}
			documents[documentKey(l)] = true
			amt, _ := t.amount(l.HSL, cur) // checked by the quality gate
			sapTB[alpha(l.RACCT)] += amt
			sapDebits += max(amt, 0)
			if l.LIFNR != "" && l.AUGBL == "" && recon[cc+"/"+l.RACCT] {
				sapOpen["vendor "+alpha(l.LIFNR)] += amt
			}
		}
		totals, err := e.Reports.Totals(ctx, le.ID, int16(t.year))
		if err != nil {
			return rec, err
		}
		tb := diffs(cc, sapTB, totals.ByAccount, format)
		rec.Diffs = append(rec.Diffs, tb...)
		add(cc, "Trial balance: accounts with differences", "0", strconv.Itoa(len(tb)), len(tb) == 0)
		if !t.balances {
			add(cc, "Total debits posted", format(sapDebits), format(totals.Debits), sapDebits == totals.Debits)
			for _, doc := range sortedKeys(documents) {
				if _, ok := r["ACDOCA/"+doc]; !ok && len(rec.Missing) < 100 {
					rec.Missing = append(rec.Missing, doc)
				}
			}
		}

		// 2. Open payables per vendor.
		open, err := e.Reports.OpenPayables(ctx, le.ID, int16(t.year))
		if err != nil {
			return rec, err
		}
		platformOpen := map[string]int64{}
		for id, amt := range open {
			key := "partner " + id.String()
			if lifnr, ok := vendorOf[id]; ok {
				key = "vendor " + lifnr
			}
			platformOpen[key] += amt
		}
		ap := diffs(cc, sapOpen, platformOpen, format)
		rec.Diffs = append(rec.Diffs, ap...)
		add(cc, "Open payables total", format(sum(sapOpen)), format(sum(platformOpen)), sum(sapOpen) == sum(platformOpen))
		add(cc, "Open payables: vendors with differences", "0", strconv.Itoa(len(ap)), len(ap) == 0)

		// 3. Vendors set up in the legal entity.
		sapVendors, platformVendors := 0, 0
		for _, v := range ext.LFB1 {
			if v.BUKRS != cc {
				continue
			}
			sapVendors++
			if id, ok := r["LFA1/"+v.LIFNR]; ok {
				s, _, err := e.Suppliers.Supplier(ctx, id, le.ID)
				if err != nil {
					return rec, err
				}
				if s.SetUp {
					platformVendors++
				}
			}
		}
		add(cc, "Vendors", strconv.Itoa(sapVendors), strconv.Itoa(platformVendors), sapVendors == platformVendors)

		// 4. Purchase orders: count and value.
		sapPOs, sapPOValue := 0, int64(0)
		var orders []uuid.UUID
		inCC := map[string]bool{}
		for _, h := range ext.EKKO {
			if h.BUKRS == cc {
				sapPOs++
				inCC[h.EBELN] = true
				if id, ok := r["EKKO/"+h.EBELN]; ok {
					orders = append(orders, id)
				}
			}
		}
		for _, i := range ext.EKPO {
			if inCC[i.EBELN] {
				sapPOValue += extend(i.MENGE, i.NETPR, i.PEINH, t, cur)
			}
		}
		n, value, err := e.OrderValues.Value(ctx, orders)
		if err != nil {
			return rec, err
		}
		add(cc, "Purchase orders", strconv.Itoa(sapPOs), strconv.Itoa(n), sapPOs == n)
		add(cc, "Purchase order value", format(sapPOValue), format(value), sapPOValue == value)

		// 5. Parked supplier invoices (posted ones are carried by their journal entries).
		dups := duplicateInvoices(ext)
		sapParked := 0
		var invoices []uuid.UUID
		for _, h := range ext.RBKP {
			key := h.BELNR + "/" + h.GJAHR
			if h.BUKRS != cc || h.RBSTAT != "A" || dups[key] != "" {
				continue
			}
			sapParked++
			if id, ok := r["RBKP/"+key]; ok {
				invoices = append(invoices, id)
			}
		}
		parked, err := e.InvoiceCounts.Count(ctx, invoices)
		if err != nil {
			return rec, err
		}
		add(cc, "Parked supplier invoices", strconv.Itoa(sapParked), strconv.Itoa(parked), sapParked == parked)
	}
	rec.Passed = !slices.ContainsFunc(rec.Checks, func(c Check) bool { return !c.Pass })
	return rec, nil
}

// extend is an item's net value: quantity times price per price unit,
// rounded half away from zero (procurement's rule).
func extend(qty, price, priceUnit string, t target, currency string) int64 {
	q, _ := money.ParseQty(qty)
	p, _ := t.amount(price, currency)
	unit, err := strconv.ParseInt(priceUnit, 10, 64)
	if err != nil || unit < 1 {
		unit = 1
	}
	return int64(money.Extend(q, money.Minor(p), unit))
}

func diffs(cc string, sapM, platformM map[string]int64, format func(int64) string) []Diff {
	keys := map[string]bool{}
	for k := range sapM {
		keys[k] = true
	}
	for k := range platformM {
		keys[k] = true
	}
	var out []Diff
	for _, k := range sortedKeys(keys) {
		if sapM[k] != platformM[k] {
			out = append(out, Diff{CompanyCode: cc, Key: k, SAP: format(sapM[k]), Platform: format(platformM[k])})
		}
	}
	return out
}

func sum(m map[string]int64) int64 {
	var t int64
	for _, v := range m {
		t += v
	}
	return t
}
