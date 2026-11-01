package web

import (
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The entry form's running totals (T73h2): its lines' debits, credits
// and their difference, summed by the workspace as the form is rendered
// and, with htmx, each time an amount changes. They are exact: each
// amount is read as the person typed it (i18n.Locale.Amount, at most four
// decimals) into integer ten-thousandths (money.Minor, never a float) and
// summed with money.Sum, which refuses an overflow. No API names a
// currency's minor-unit digits, so the totals have as many decimals as the
// amounts typed. An amount that cannot be read, or whose sum would be out
// of range, is not counted, and the form says how many are not. The totals
// are the person's aid only: the API checks the entry's balance.

// totalPlaces are the decimals the totals are summed in: the most an
// amount typed has.
const totalPlaces = 4

// totalsOf are the lines' running totals in the currency.
func totalsOf(loc i18n.Locale, lines []entryLine, currency string) ui.EntryTotals {
	var debits, credits money.Minor
	places, uncounted := 0, 0
	for _, l := range lines {
		if strings.TrimSpace(l.amount) == "" {
			continue
		}
		amount, ok := loc.Amount(l.amount)
		m, err := money.ParseDecimal(amount, totalPlaces)
		if !ok || err != nil {
			uncounted++
			continue
		}
		side := &debits
		if m < 0 {
			side, m = &credits, -m
		}
		sum, err := money.Sum(*side, m)
		if err != nil {
			uncounted++
			continue
		}
		*side = sum
		_, frac, _ := strings.Cut(amount, ".")
		places = max(places, len(frac))
	}
	if !currencyCode.MatchString(currency) {
		currency = ""
	}
	// Both sums are at least zero, so their difference is in range.
	return ui.EntryTotals{Debits: plain(debits, places), Credits: plain(credits, places), Difference: plain(debits-credits, places),
		Currency: currency, Uncounted: uncounted}
}

// plain is an amount of ten-thousandths as a decimal string with the
// places given, the API's shape: the places dropped are zeros, as no
// amount summed has more.
func plain(m money.Minor, places int) string {
	s := m.Plain(totalPlaces)
	if places == 0 {
		return s[:len(s)-totalPlaces-1]
	}
	return s[:len(s)-(totalPlaces-places)]
}
