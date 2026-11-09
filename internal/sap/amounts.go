package sap

import (
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// InternalDecimals is the decimals of every amount in SAP's internal form,
// and so of a currency TCURX does not list (TCURX's comment says what of
// this is stated from memory).
const InternalDecimals = 2

// TCURXProblem is a TCURX row the conversion cannot use, by its currency.
type TCURXProblem struct {
	CURRKEY string
	Message string
}

// CurrencyDecimals are the decimals TCURX gives each currency it lists,
// keys and values trimmed (a raw dump may pad them), with a problem for
// each row the conversion cannot use: a CURRDEC that is not a number of
// decimals from 0 to 9, or a currency listed twice with different
// decimals, which is reported once and left out. A currency of no usable
// row converts as one of two decimals.
func CurrencyDecimals(rows []TCURX) (map[string]int, []TCURXProblem) {
	out := map[string]int{}
	conflicting := map[string]bool{}
	var problems []TCURXProblem
	for _, r := range rows {
		key, dec := strings.TrimSpace(r.CURRKEY), strings.TrimSpace(r.CURRDEC)
		if len(dec) != 1 || dec[0] < '0' || dec[0] > '9' {
			problems = append(problems, TCURXProblem{key, fmt.Sprintf("CURRDEC %q of currency %s is not a number of decimals from 0 to 9", r.CURRDEC, key)})
			continue
		}
		d := int(dec[0] - '0')
		if prev, seen := out[key]; seen && prev != d && !conflicting[key] {
			conflicting[key] = true
			problems = append(problems, TCURXProblem{key, fmt.Sprintf("currency %s is listed with %d and with %d decimals", key, prev, d)})
		}
		out[key] = d
	}
	for c := range conflicting {
		delete(out, c)
	}
	return out, problems
}

// FromInternal is the amount an SAP amount in internal form stands for, as
// a plain decimal with the currency's TCURX decimals (JPY "10.00" of
// CURRDEC 0 is "1000", KWD "12.34" of CURRDEC 3 is "1.234"), and whether
// decimals (CurrencyDecimals) lists the currency; one it does not list
// keeps two decimals, which holds only for a currency of two, so the
// caller refuses another. An amount with more than two significant
// decimals is an error.
func FromInternal(s, currency string, decimals map[string]int) (value string, listed bool, err error) {
	d, listed := decimals[currency]
	if !listed {
		d = InternalDecimals
	}
	stored, err := money.ParseDecimal(s, InternalDecimals)
	if err != nil {
		return "", listed, fmt.Errorf("amount %q in SAP's internal form of two decimals: %w", s, err)
	}
	return stored.Plain(d), listed, nil
}
