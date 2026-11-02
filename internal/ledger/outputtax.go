package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// checkOutputTax holds a manual or integration entry's output tax to its
// codes (T61f, ADR-0049 §7: the pilot's sales arrive as journal entries,
// one per sales invoice or a summary per code and jurisdiction, P2): each
// taxed line names the subdivision its code's component levies, where one
// does (checkJurisdictions), and the tax lines of each output code and
// jurisdiction are the tax its base lines give at the rates of the tax
// date, within one minor unit per base line and component, a sum of
// per-invoice roundings (checkOutputLines). Zero-rated
// and exempt bases carry their codes without tax lines and are items at
// rate 0 (ledger.write_tax_items).
func checkOutputTax(ctx context.Context, le uuid.UUID, date time.Time, lines []Line, expTC int) error {
	if !slices.ContainsFunc(lines, func(l Line) bool { return l.TaxCode != nil || l.TaxJurisdiction != nil }) {
		return nil
	}
	codes, err := Accounts{}.TaxCodes(ctx, le, date)
	if err != nil {
		return err
	}
	byCode := map[string]TaxCode{}
	for _, t := range codes {
		byCode[t.Code] = t
	}
	if err := checkJurisdictions(lines, byCode); err != nil {
		return err
	}
	return checkOutputLines(lines, byCode, date, expTC)
}

// onLineCountry is the country whose subdivision a line with the code
// names: its component's that levies the line's; "" when none does.
func onLineCountry(t TaxCode) string {
	for _, c := range t.Components {
		if c.JurisdictionOnLine {
			return c.Jurisdiction
		}
	}
	return ""
}

// checkJurisdictions requires a line's jurisdiction exactly where its
// code's component levies the line's subdivision (422
// ledger.jurisdiction_required), and then one of that component's country
// (422 ledger.jurisdiction_not_levied).
func checkJurisdictions(lines []Line, byCode map[string]TaxCode) error {
	for i, l := range lines {
		country := ""
		if l.TaxCode != nil {
			country = onLineCountry(byCode[*l.TaxCode])
		}
		field := fmt.Sprintf("lines[%d].tax_jurisdiction", i)
		switch j := l.TaxJurisdiction; {
		case j == nil && country != "":
			return problem.Unprocessable("ledger.jurisdiction_required", fmt.Sprintf("Line %d carries tax code %s, which levies the tax of the "+
				"subdivision its lines name: name the province or emirate (%s-…).", i+1, *l.TaxCode, country),
				problem.FieldError{Field: field, Code: "required", Message: "a subdivision of " + country})
		case j != nil && (country == "" || !strings.HasPrefix(*j, country+"-")):
			return problem.Unprocessable("ledger.jurisdiction_not_levied", fmt.Sprintf("Line %d names jurisdiction %s, which its tax code does not "+
				"levy: a line names one only where its code levies its lines' subdivisions.", i+1, *j),
				problem.FieldError{Field: field, Code: "not_levied", Message: "only with a code that levies its lines' subdivision"})
		}
	}
	return nil
}

// outputGroup is an output code in a jurisdiction ("" for the code's own):
// what its tax is calculated over.
type outputGroup struct{ code, jurisdiction string }

// outputLines are an entry's lines of output codes by code and
// jurisdiction: the base lines (not on a component's account) and what
// the tax lines post on each component's account.
type outputLines struct {
	bases  map[outputGroup][]tax.Line
	posted map[outputGroup]map[int]money.Minor // by component
}

// splitOutput sorts the output lines into base and tax lines; tax lines
// adding up to more than an amount are refused.
func splitOutput(lines []Line, byCode map[string]TaxCode) (outputLines, error) {
	out := outputLines{bases: map[outputGroup][]tax.Line{}, posted: map[outputGroup]map[int]money.Minor{}}
	for _, l := range lines {
		if l.TaxCode == nil || byCode[*l.TaxCode].Direction != "output" {
			continue
		}
		t := byCode[*l.TaxCode]
		g := outputGroup{t.Code, deref(l.TaxJurisdiction, "")}
		k := slices.IndexFunc(t.Components, func(c TaxComponent) bool { return c.AccountID != nil && *c.AccountID == l.AccountID })
		if k < 0 {
			out.bases[g] = append(out.bases[g], tax.Line{Amount: l.TC, Code: t.Code})
			continue
		}
		if out.posted[g] == nil {
			out.posted[g] = map[int]money.Minor{}
		}
		sum, err := money.Sum(out.posted[g][k], l.TC)
		if err != nil {
			return outputLines{}, errTotalTooLarge("lines")
		}
		out.posted[g][k] = sum
	}
	return out, nil
}

// checkOutputLines compares each output code's tax lines in a jurisdiction
// with the tax its base lines give (422 ledger.output_tax_mismatch). A code
// with tax lines and no base line is an adjustment of tax alone, recorded
// as posted.
func checkOutputLines(lines []Line, byCode map[string]TaxCode, date time.Time, expTC int) error {
	split, err := splitOutput(lines, byCode)
	if err != nil {
		return err
	}
	groups := make([]outputGroup, 0, len(split.bases))
	for g := range split.bases {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b outputGroup) int {
		return cmp.Or(cmp.Compare(a.code, b.code), cmp.Compare(a.jurisdiction, b.jurisdiction))
	})
	for _, g := range groups {
		t := byCode[g.code]
		res, err := tax.Calculate(tax.Document{TaxDate: date, Codes: map[string]tax.Code{t.Code: t.Calculable()}, Lines: split.bases[g]})
		if err != nil {
			return calculationFailed(t.Code, err)
		}
		// One minor unit per base line: a billing system rounds each
		// invoice (or line) it sums, the calculator the sum once.
		units := money.Minor(len(split.bases[g]))
		for k, it := range res.Items {
			if got := split.posted[g][k]; t.Components[k].AccountID != nil && (got-it.Tax > units || it.Tax-got > units) {
				return outputMismatch(g, it, got, expTC)
			}
		}
	}
	return nil
}

// outputMismatch refuses an output code's tax lines that are not the tax
// on its base lines.
func outputMismatch(g outputGroup, it tax.ComponentTax, got money.Minor, expTC int) error {
	where := ""
	if g.jurisdiction != "" {
		where = " in " + g.jurisdiction
	}
	return problem.Unprocessable("ledger.output_tax_mismatch", fmt.Sprintf("Tax code %s%s: its tax lines post %s of %s, the rate on the tax date "+
		"gives %s on its base lines of %s.", g.code, where, got.Plain(expTC), it.Key, it.Tax.Plain(expTC), it.Base.Plain(expTC)),
		problem.FieldError{Field: "lines", Code: "output_tax_mismatch", Message: "tax lines of the code's tax on its base lines, within a minor unit per base line"})
}

// calculationFailed is the calculator's error: base lines adding up to more
// than an amount are the entry's (422 ledger.amount_too_large); a code it
// cannot calculate on the date is the configuration's then (notCalculable);
// the rest are not expected.
func calculationFailed(code string, err error) error {
	if errors.Is(err, money.ErrOverflow) {
		return errTotalTooLarge("lines")
	}
	if p := notCalculable(err); p != nil {
		return p
	}
	return fmt.Errorf("ledger: output tax of code %s: %w", code, err)
}

// notCalculable is the problem of a code the calculator cannot calculate
// on the entry's date, the configuration's on that date, which the entry
// cannot post on until it is corrected or another date or code is chosen:
// a component without a rate then (422 ledger.no_tax_rate), or components
// as rated then that do not make a code (a self-assessed code missing half
// its pair, its halves at different rates: 422 ledger.invalid_tax_code),
// the calculator's words naming the code; nil for its other errors.
func notCalculable(err error) error {
	switch {
	case errors.Is(err, tax.ErrNoRate):
		return problem.Unprocessable("ledger.no_tax_rate", "The entry's tax cannot be calculated: "+tax.Reason(err)+".")
	case errors.Is(err, tax.ErrInvalidCode):
		return problem.Unprocessable("ledger.invalid_tax_code", "The entry's tax cannot be calculated: "+tax.Reason(err)+".")
	}
	return nil
}
