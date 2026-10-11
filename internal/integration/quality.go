package integration

import (
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Issue severities.
const (
	SevError   = "error"   // blocks the load
	SevWarning = "warning" // migrates, but needs a business decision
)

// Issue is a finding about the source data or a record the load refused.
type Issue struct {
	Severity string `json:"severity"`
	Table    string `json:"table"`
	Key      string `json:"key"`
	Message  string `json:"message"`
}

func hasErrors(issues []Issue) bool {
	return slices.ContainsFunc(issues, func(i Issue) bool { return i.Severity == SevError })
}

// target is what the platform holds for a run's scope: the legal entity of
// each company code with its chart, the configured payment terms and
// number ranges, and the minor-unit digits of every currency.
type target struct {
	entities  map[string]tenancy.Entity                 // by company code
	charts    map[string]map[string]ledger.ChartAccount // by company code, then account number
	terms     map[string]bool
	ranges    map[string]bool // company/document type/fiscal year
	exponents map[string]int
	year      int32
	balances  bool // balances mode
}

func (t target) hasRange(company, documentType string) bool {
	return t.ranges[fmt.Sprintf("%s/%s/%d", company, documentType, t.year)]
}

// alpha strips SAP ALPHA-conversion leading zeros ("0000100001" -> "100001").
func alpha(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" && s != "" {
		return "0"
	}
	return t
}

// sapDate converts SAP YYYYMMDD to a date.
func sapDate(d string) (time.Time, error) {
	t, err := time.Parse("20060102", d)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid SAP date %q", d)
	}
	return t, nil
}

// amount parses an external-format SAP amount (as OData delivers it) in
// minor units of the currency.
func (t target) amount(s, currency string) (int64, error) {
	exp, ok := t.exponents[currency]
	if !ok {
		return 0, fmt.Errorf("currency %q is not known", currency)
	}
	m, err := money.ParseDecimal(s, exp)
	return int64(m), err
}

func quantity(s string) error {
	if q, ok := new(big.Rat).SetString(s); !ok || q.Sign() <= 0 {
		return fmt.Errorf("quantity %q is not a positive number", s)
	}
	return nil
}

// normalizeRef is platform.normalize_ref: duplicate supplier references
// differ only in case and punctuation.
func normalizeRef(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			return r
		}
		return -1
	}, s)
}

// duplicateInvoices are the parked invoices that repeat an earlier
// invoice's supplier reference for the same vendor. Payables refuses them,
// so they are not migrated, and reconciliation leaves them out on the SAP
// side too.
func duplicateInvoices(e *sap.Extract) map[string]string {
	seen := map[string]string{}
	out := map[string]string{}
	for _, h := range e.RBKP {
		if h.RBSTAT != "A" {
			continue
		}
		key, ref := h.LIFNR+"/"+normalizeRef(h.XBLNR), h.BELNR+"/"+h.GJAHR
		if first, ok := seen[key]; ok {
			out[ref] = first
			continue
		}
		seen[key] = ref
	}
	return out
}

// checkQuality profiles an extract against the platform before anything is
// loaded. Problems found here are cheap to fix in SAP or in the
// configuration; problems found after cutover are not.
func checkQuality(e *sap.Extract, t target) []Issue {
	var out []Issue
	add := func(sev, table, key, format string, args ...any) {
		out = append(out, Issue{Severity: sev, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
	}
	currency := map[string]string{}
	for _, c := range e.T001 {
		currency[c.BUKRS] = c.WAERS
		if le := t.entities[c.BUKRS]; c.WAERS != le.LocalCurrency {
			add(SevError, "T001", c.BUKRS, "company code currency %q is not the legal entity's local currency %s", c.WAERS, le.LocalCurrency)
		}
	}

	accounts := map[string]sap.SKB1{}
	for _, a := range e.SKB1 {
		accounts[a.BUKRS+"/"+a.SAKNR] = a
		got, ok := t.charts[a.BUKRS][alpha(a.SAKNR)]
		switch {
		case !ok:
			add(SevError, "SKB1", a.BUKRS+"/"+a.SAKNR, "account %s is not in the legal entity's chart of accounts; add it through a configuration change", alpha(a.SAKNR))
		case (a.MITKZ == "K") != (got.ReconciliationKind == "supplier"):
			add(SevError, "SKB1", a.BUKRS+"/"+a.SAKNR, "account %s is a vendor reconciliation account on one side only", alpha(a.SAKNR))
		}
	}

	vendors := map[string]sap.LFA1{}
	byName := map[string][]string{}
	for _, v := range e.LFA1 {
		vendors[v.LIFNR] = v
		if strings.TrimSpace(v.NAME1) == "" {
			add(SevError, "LFA1", v.LIFNR, "vendor has no name")
		}
		if strings.TrimSpace(v.STCD1) == "" {
			add(SevWarning, "LFA1", v.LIFNR, "vendor %q has no tax number", v.NAME1)
		}
		key := strings.ToUpper(strings.Join(strings.Fields(v.NAME1), " "))
		byName[key] = append(byName[key], v.LIFNR)
	}
	for _, name := range sortedKeys(byName) {
		if ids := byName[name]; len(ids) > 1 {
			add(SevWarning, "LFA1", strings.Join(ids, ","), "possible duplicate vendors named %q", name)
		}
	}
	for _, v := range e.LFB1 {
		if _, ok := vendors[v.LIFNR]; !ok {
			add(SevError, "LFB1", v.LIFNR, "company data without vendor master (LFA1)")
		}
		if a, ok := accounts[v.BUKRS+"/"+v.AKONT]; !ok || a.MITKZ != "K" {
			add(SevError, "LFB1", v.LIFNR, "reconciliation account %s is not a vendor reconciliation account of company %s", v.AKONT, v.BUKRS)
		}
		if !t.terms[v.ZTERM] {
			add(SevError, "LFB1", v.LIFNR, "payment terms %q are not in the configuration", v.ZTERM)
		}
	}

	poHeaders := map[string]sap.EKKO{}
	for _, h := range e.EKKO {
		poHeaders[h.EBELN] = h
		if _, ok := vendors[h.LIFNR]; !ok {
			add(SevError, "EKKO", h.EBELN, "vendor %s not in extract", h.LIFNR)
		}
		if _, err := sapDate(h.BEDAT); err != nil {
			add(SevError, "EKKO", h.EBELN, "%v", err)
		}
	}
	for _, i := range e.EKPO {
		key := i.EBELN + "/" + i.EBELP
		h := poHeaders[i.EBELN] // extracts keep the items of the scope's orders only
		if _, ok := t.charts[h.BUKRS][alpha(i.SAKTO)]; !ok {
			add(SevError, "EKPO", key, "G/L account %s is not in the chart of company %s", alpha(i.SAKTO), h.BUKRS)
		}
		if err := quantity(i.MENGE); err != nil {
			add(SevError, "EKPO", key, "%v", err)
		}
		if _, err := t.amount(i.NETPR, h.WAERS); err != nil {
			add(SevError, "EKPO", key, "%v", err)
		}
		if n, err := strconv.Atoi(i.PEINH); i.PEINH != "" && (err != nil || n < 1) {
			add(SevError, "EKPO", key, "price unit %q is not a positive whole number", i.PEINH)
		}
	}
	for _, m := range e.MSEG {
		if m.BWART != "101" {
			add(SevWarning, "MSEG", m.MBLNR+"/"+m.MJAHR+"/"+m.ZEILE, "movement type %s is not migrated; received quantities count receipts (101) only", m.BWART)
		}
	}

	// Universal Journal: every document must balance, hit known accounts
	// and have a number range for its document type.
	docTotals := map[string]int64{}
	openByVendor, noRange := map[string]bool{}, map[string]bool{}
	for _, l := range e.ACDOCA {
		key := l.RBUKRS + "/" + l.GJAHR + "/" + l.BELNR
		amt, err := t.amount(l.HSL, currency[l.RBUKRS])
		if err != nil {
			add(SevError, "ACDOCA", key+"/"+l.DOCLN, "%v", err)
			continue
		}
		docTotals[key] += amt
		a, ok := accounts[l.RBUKRS+"/"+l.RACCT]
		if !ok {
			add(SevError, "ACDOCA", key+"/"+l.DOCLN, "account %s missing from chart of accounts", l.RACCT)
		} else if a.MITKZ == "K" && l.LIFNR == "" {
			add(SevError, "ACDOCA", key+"/"+l.DOCLN, "posting to vendor reconciliation account %s without vendor", l.RACCT)
		}
		if l.LIFNR != "" {
			if _, known := vendors[l.LIFNR]; !known {
				add(SevError, "ACDOCA", key+"/"+l.DOCLN, "vendor %s not in extract", l.LIFNR)
			}
			if l.AUGBL == "" && ok && a.MITKZ == "K" {
				openByVendor[l.LIFNR] = true
			}
		}
		if _, err := sapDate(l.BUDAT); err != nil {
			add(SevError, "ACDOCA", key+"/"+l.DOCLN, "%v", err)
		}
		if documentType := "journal_" + strings.ToLower(l.BLART); !t.balances && !t.hasRange(l.RBUKRS, documentType) && !noRange[l.RBUKRS+documentType] {
			noRange[l.RBUKRS+documentType] = true
			add(SevError, "ACDOCA", key, "the configuration has no %s number range for company %s in %d", documentType, l.RBUKRS, t.year)
		}
	}
	for _, key := range sortedKeys(docTotals) {
		if total := docTotals[key]; total != 0 {
			add(SevError, "ACDOCA", key, "document does not balance (off by %d minor units)", total)
		}
	}
	for _, id := range sortedKeys(openByVendor) {
		if v := vendors[id]; v.SPERR == "X" {
			add(SevWarning, "LFA1", id, "vendor %q is posting-blocked but has open payables", v.NAME1)
		}
	}
	if t.balances {
		for _, c := range e.T001 {
			if !t.hasRange(c.BUKRS, "journal_ob") {
				add(SevError, "T001", c.BUKRS, "the configuration has no journal_ob number range for the opening entry in %d", t.year)
			}
		}
	}

	// Supplier invoices: header must agree with items and tax.
	invCurrency := map[string]string{}
	for _, h := range e.RBKP {
		invCurrency[h.BELNR+"/"+h.GJAHR] = h.WAERS
	}
	itemTotals := map[string]int64{}
	for _, it := range e.RSEG {
		amt, err := t.amount(it.WRBTR, invCurrency[it.BELNR+"/"+it.GJAHR])
		if err != nil {
			add(SevError, "RSEG", it.BELNR+"/"+it.BUZEI, "%v", err)
			continue
		}
		itemTotals[it.BELNR+"/"+it.GJAHR] += amt
	}
	dups := duplicateInvoices(e)
	for _, h := range e.RBKP {
		key := h.BELNR + "/" + h.GJAHR
		gross, err1 := t.amount(h.RMWWR, h.WAERS)
		tax, err2 := t.amount(h.WMWST1, h.WAERS)
		_, err3 := sapDate(h.BLDAT)
		switch {
		case err1 != nil || err2 != nil || err3 != nil:
			add(SevError, "RBKP", key, "unparseable amounts %q / %q or date %q", h.RMWWR, h.WMWST1, h.BLDAT)
			continue
		case h.RBSTAT == "5":
			continue // posted in SAP: its journal entry carries it
		case h.RBSTAT != "A":
			add(SevWarning, "RBKP", key, "invoice status %q is not migrated", h.RBSTAT)
			continue
		case dups[key] != "":
			add(SevWarning, "RBKP", key, "supplier reference %q repeats invoice %s; it is not migrated", h.XBLNR, dups[key])
			continue
		}
		if net := itemTotals[key]; net+tax != gross {
			add(SevWarning, "RBKP", key, "gross %d != items %d + tax %d minor units (it will be held at matching)", gross, net, tax)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
