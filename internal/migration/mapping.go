// Package migration moves SAP data into the native core one scope (company
// code + fiscal year) at a time, and proves the result with reconciliation.
package migration

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Mapping holds the customer-specific rules a migration needs. Defaults suit
// the mock data; real projects replace them with the client's chart of
// accounts design and payment terms configuration.
type Mapping struct {
	// AccountTypes classify G/L accounts by number range (after ALPHA conversion).
	AccountTypes []RangeRule `json:"accountTypes"`
	// PaymentTerms maps ZTERM keys to net days; unmapped keys like "NT30" are parsed.
	PaymentTerms map[string]int `json:"paymentTerms"`
	// CurrencyDecimals overrides the default of 2 (e.g. JPY: 0).
	CurrencyDecimals map[string]int `json:"currencyDecimals"`
}

// RangeRule matches account numbers From <= n <= To, compared numerically.
type RangeRule struct {
	From string           `json:"from"`
	To   string           `json:"to"`
	Type core.AccountType `json:"type"`
}

func DefaultMapping() Mapping {
	return Mapping{
		AccountTypes: []RangeRule{
			{From: "100000", To: "199999", Type: core.Asset},
			{From: "200000", To: "299999", Type: core.Liability},
			{From: "300000", To: "399999", Type: core.Equity},
			{From: "400000", To: "499999", Type: core.Revenue},
			{From: "500000", To: "999999", Type: core.Expense},
		},
		PaymentTerms:     map[string]int{"0001": 0},
		CurrencyDecimals: map[string]int{"JPY": 0, "KRW": 0, "BHD": 3, "KWD": 3},
	}
}

func (m Mapping) AccountType(number string) (core.AccountType, error) {
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return "", fmt.Errorf("account %q is not numeric; add an explicit mapping rule", number)
	}
	for _, r := range m.AccountTypes {
		from, err1 := strconv.ParseInt(r.From, 10, 64)
		to, err2 := strconv.ParseInt(r.To, 10, 64)
		if err1 == nil && err2 == nil && n >= from && n <= to {
			return r.Type, nil
		}
	}
	return "", fmt.Errorf("no account type rule covers account %s", number)
}

func (m Mapping) paymentDays(zterm string) (int, error) {
	if d, ok := m.PaymentTerms[zterm]; ok {
		return d, nil
	}
	digits := strings.TrimLeftFunc(zterm, func(r rune) bool { return !unicode.IsDigit(r) })
	if d, err := strconv.Atoi(digits); err == nil && digits != "" {
		return d, nil
	}
	return 0, fmt.Errorf("payment terms %q not mapped", zterm)
}

func (m Mapping) decimals(currency string) int {
	if d, ok := m.CurrencyDecimals[currency]; ok {
		return d
	}
	return 2
}

// parseAmount expects external-format amounts (as OData delivers them). Raw
// table dumps store e.g. JPY shifted by two decimals (TCURX); convert those
// before extraction.
func (m Mapping) parseAmount(s, currency string) (money.Minor, error) {
	return money.ParseDecimal(s, m.decimals(currency))
}

// alpha strips SAP ALPHA-conversion leading zeros ("0000100001" -> "100001").
func alpha(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" && s != "" {
		return "0"
	}
	return t
}

// isoDate converts SAP YYYYMMDD to YYYY-MM-DD.
func isoDate(d string) (string, error) {
	if len(d) != 8 {
		return "", fmt.Errorf("invalid SAP date %q", d)
	}
	return d[:4] + "-" + d[4:6] + "-" + d[6:], nil
}
