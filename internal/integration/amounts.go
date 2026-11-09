package integration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The forms an SAP amount comes in (the pilot customer questionnaire's D10
// planning default, po:2026-11-09). SAP stores every amount with two
// decimals, shifting the amount of a currency with other decimals by
// 2 - TCURX-CURRDEC: JPY 1,000 is stored as "10.00" and KWD 1.234 as
// "12.34". Table exports (SE16N, SE16H, raw dumps) write that internal
// form; OData, and an export asked for in external form, write the amount
// itself. A sap_file connector reads the internal form unless its
// settings say amount_format "external"; an OData extract is always
// external.
const (
	AmountInternal = "internal"
	AmountExternal = "external"
)

// amountsInternal reports whether a source connector's amounts come in
// SAP's internal form: a file extract's unless its settings say otherwise
// (checkSettings held amount_format to the two forms).
func amountsInternal(c Connector) bool {
	return c.Kind == SAPFile && c.Settings["amount_format"] != AmountExternal
}

// checkAmountFormat validates a sap_file connector's amount_format.
func checkAmountFormat(settings map[string]any) error {
	if f, ok := settings["amount_format"]; ok && f != AmountInternal && f != AmountExternal {
		return invalid("integration.invalid_settings", "settings.amount_format",
			"The form the extract's amounts are in: internal (SAP's stored form, shifted by TCURX, the default) or external.")
	}
	return nil
}

// currencyDecimals is sap.CurrencyDecimals with each problem as an error
// issue of table TCURX.
func currencyDecimals(rows []sap.TCURX) (map[string]int, []Issue) {
	decimals, problems := sap.CurrencyDecimals(rows)
	issues := make([]Issue, 0, len(problems))
	for _, p := range problems {
		issues = append(issues, Issue{SevError, "TCURX", p.CURRKEY, p.Message})
	}
	return decimals, issues
}

// amount parses an SAP amount in minor units of the currency: in external
// form with the currency's ISO digits, in internal form with two decimals
// shifted to the currency's TCURX decimals. An amount with more
// significant decimals than its form allows, or than the currency's minor
// unit holds once converted, is an error, as is an internal amount in a
// currency of other than two decimals that TCURX does not list.
func (t target) amount(s, currency string) (money.Minor, error) {
	exp, ok := t.exponents[currency]
	if !ok {
		return 0, fmt.Errorf("currency %q is not known", currency)
	}
	if !t.internal {
		return money.ParseDecimal(s, exp)
	}
	value, listed, err := sap.FromInternal(s, currency, t.decimals)
	switch {
	case !listed && exp != sap.InternalDecimals:
		return 0, fmt.Errorf("amount %q is in SAP's internal form, and TCURX does not give the decimals of %s, which has %d", s, currency, exp)
	case err != nil:
		return 0, err
	}
	m, err := money.ParseDecimal(value, exp)
	if err != nil {
		return 0, fmt.Errorf("amount %q in SAP's internal form is %s %s (TCURX: %d decimals): %w", s, value, currency, t.decimals[currency], err)
	}
	return m, nil
}

// amountForm refuses an extract in internal form that holds amounts in a
// currency of other than two decimals which its TCURX does not list,
// naming those currencies once for the extract (each amount is refused
// where it is checked too), and every TCURX row the conversion cannot use.
func (q *qualityCheck) amountForm() {
	if !q.t.internal {
		return
	}
	_, issues := currencyDecimals(q.e.TCURX)
	q.out = append(q.out, issues...)
	var missing []string
	for _, c := range q.amountCurrencies() {
		if exp, known := q.t.exponents[c]; known && exp != sap.InternalDecimals {
			if _, listed := q.t.decimals[c]; !listed {
				missing = append(missing, fmt.Sprintf("%s (%d decimals)", c, exp))
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	what := "its TCURX does not list"
	if len(q.e.TCURX) == 0 {
		what = "it holds no TCURX to convert"
	}
	q.add(SevError, "TCURX", "", "the extract's amounts are in SAP's internal form (two decimals, shifted by TCURX-CURRDEC), and %s the amounts in %s; "+
		"extract TCURX (CURRKEY, CURRDEC) with the tables, or set the connector's amount_format to external for an export in external form",
		what, strings.Join(missing, ", "))
}

// amountCurrencies are the currencies the extract's amounts are in, sorted:
// the company codes' (HSL), the documents' (WSL), the orders' (NETPR) and
// the supplier invoices' (RMWWR, WMWST1, WRBTR).
func (q *qualityCheck) amountCurrencies() []string {
	seen := map[string]bool{}
	for _, c := range q.e.T001 {
		seen[c.WAERS] = true
	}
	for _, l := range q.e.ACDOCA {
		seen[l.RWCUR] = true
	}
	for _, h := range q.e.EKKO {
		seen[h.WAERS] = true
	}
	for _, h := range q.e.RBKP {
		seen[h.WAERS] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}
