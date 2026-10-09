package migration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

type Severity string

const (
	SevError   Severity = "error"   // blocks the load unless forced
	SevWarning Severity = "warning" // migrates, but needs a business decision
)

type Issue struct {
	Severity Severity `json:"severity"`
	Table    string   `json:"table"`
	Key      string   `json:"key"`
	Message  string   `json:"message"`
}

func (i Issue) String() string {
	return fmt.Sprintf("[%s] %s %s: %s", i.Severity, i.Table, i.Key, i.Message)
}

func HasErrors(issues []Issue) bool {
	return slices.ContainsFunc(issues, func(i Issue) bool { return i.Severity == SevError })
}

// CheckQuality profiles an extract before anything is loaded. Problems found
// here are cheap to fix in SAP; problems found after cutover are not.
func CheckQuality(e *sap.Extract, m Mapping) []Issue {
	var out []Issue
	add := func(sev Severity, table, key, format string, args ...any) {
		out = append(out, Issue{Severity: sev, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
	}

	currency := map[string]string{}
	for _, c := range e.T001 {
		if c.WAERS == "" {
			add(SevError, "T001", c.BUKRS, "company code has no currency")
		}
		currency[c.BUKRS] = c.WAERS
	}

	accounts := map[string]sap.SKB1{}
	for _, a := range e.SKB1 {
		accounts[a.BUKRS+"/"+a.SAKNR] = a
		if _, err := m.accountType(alpha(a.SAKNR)); err != nil {
			add(SevError, "SKB1", a.BUKRS+"/"+a.SAKNR, "%v", err)
		}
	}

	vendors := map[string]sap.LFA1{}
	byName := map[string][]string{}
	for _, v := range e.LFA1 {
		vendors[v.LIFNR] = v
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
		a, ok := accounts[v.BUKRS+"/"+v.AKONT]
		if !ok {
			add(SevError, "LFB1", v.LIFNR, "reconciliation account %s missing in company %s", v.AKONT, v.BUKRS)
		} else if a.MITKZ != "K" {
			add(SevError, "LFB1", v.LIFNR, "account %s is not a vendor reconciliation account", v.AKONT)
		}
		if _, err := m.paymentDays(v.ZTERM); err != nil {
			add(SevError, "LFB1", v.LIFNR, "%v", err)
		}
	}

	poHeaders := map[string]sap.EKKO{}
	for _, h := range e.EKKO {
		poHeaders[h.EBELN] = h
		if _, ok := vendors[h.LIFNR]; !ok {
			add(SevError, "EKKO", h.EBELN, "vendor %s not in extract", h.LIFNR)
		}
	}
	for _, i := range e.EKPO {
		h, ok := poHeaders[i.EBELN]
		if !ok {
			add(SevError, "EKPO", i.EBELN+"/"+i.EBELP, "item without header")
			continue
		}
		if _, ok := accounts[h.BUKRS+"/"+i.SAKTO]; !ok {
			add(SevError, "EKPO", i.EBELN+"/"+i.EBELP, "G/L account %s missing in company %s", i.SAKTO, h.BUKRS)
		}
		if _, err := money.ParseQty(i.MENGE); err != nil {
			add(SevError, "EKPO", i.EBELN+"/"+i.EBELP, "%v", err)
		}
		if _, err := m.parseAmount(i.NETPR, h.WAERS); err != nil {
			add(SevError, "EKPO", i.EBELN+"/"+i.EBELP, "%v", err)
		}
	}

	// Universal Journal: every document must balance and hit known accounts.
	docTotals := map[string]money.Minor{}
	openByVendor := map[string]bool{}
	for _, l := range e.ACDOCA {
		key := l.RBUKRS + "/" + l.GJAHR + "/" + l.BELNR
		amt, err := m.parseAmount(l.HSL, currency[l.RBUKRS])
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
			if _, ok := vendors[l.LIFNR]; !ok {
				add(SevError, "ACDOCA", key+"/"+l.DOCLN, "vendor %s not in extract", l.LIFNR)
			}
			if l.AUGBL == "" && ok && a.MITKZ == "K" {
				openByVendor[l.LIFNR] = true
			}
		}
		if _, err := isoDate(l.BUDAT); err != nil {
			add(SevError, "ACDOCA", key+"/"+l.DOCLN, "%v", err)
		}
	}
	for _, key := range sortedKeys(docTotals) {
		if t := docTotals[key]; t != 0 {
			add(SevError, "ACDOCA", key, "document does not balance (off by %s)", t)
		}
	}
	for _, id := range sortedKeys(openByVendor) {
		if v := vendors[id]; v.SPERR == "X" {
			add(SevWarning, "LFA1", id, "vendor %q is posting-blocked but has open payables", v.NAME1)
		}
	}

	// Supplier invoices: header must agree with items + tax.
	invCurrency := map[string]string{}
	for _, h := range e.RBKP {
		invCurrency[h.BELNR+"/"+h.GJAHR] = h.WAERS
	}
	itemTotals := map[string]money.Minor{}
	for _, it := range e.RSEG {
		amt, err := m.parseAmount(it.WRBTR, invCurrency[it.BELNR+"/"+it.GJAHR])
		if err != nil {
			add(SevError, "RSEG", it.BELNR+"/"+it.BUZEI, "%v", err)
			continue
		}
		itemTotals[it.BELNR+"/"+it.GJAHR] += amt
	}
	for _, h := range e.RBKP {
		key := h.BELNR + "/" + h.GJAHR
		gross, err1 := m.parseAmount(h.RMWWR, h.WAERS)
		tax, err2 := m.parseAmount(h.WMWST1, h.WAERS)
		if err1 != nil || err2 != nil {
			add(SevError, "RBKP", key, "unparseable amounts %q / %q", h.RMWWR, h.WMWST1)
			continue
		}
		if net := itemTotals[key]; net+tax != gross {
			add(SevWarning, "RBKP", key, "gross %s != items %s + tax %s (will be held at invoice matching)", gross, net, tax)
		}
		if h.RBSTAT != "A" && h.RBSTAT != "5" {
			add(SevWarning, "RBKP", key, "invoice status %q is not migrated", h.RBSTAT)
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
