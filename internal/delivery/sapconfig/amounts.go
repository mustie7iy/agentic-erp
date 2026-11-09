package sapconfig

import (
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// amount is an SAP amount in a currency as the configuration holds it: as
// the export wrote it when Options.ExternalAmounts says it is in external
// form, else converted from SAP's internal form with the extract's TCURX
// (the pilot customer questionnaire's D10 planning default,
// po:2026-11-09; docs/migration.md, "Amounts in SAP's internal form").
// problem says why it cannot be converted: it is no amount of two
// decimals, or its currency has other than two ISO decimals and TCURX does
// not give its decimals; the draft never guesses.
func (g *gen) amount(s, cur string) (value, problem string) {
	s = strings.TrimSpace(s)
	if g.opts.ExternalAmounts {
		return s, ""
	}
	value, listed, err := sap.FromInternal(s, cur, g.decimals)
	if exp, known := currency.SeededExponent(cur); !listed && (!known || exp != sap.InternalDecimals) {
		return "", fmt.Sprintf("is in SAP's internal form, and the extract's TCURX does not give the decimals of %s", cur)
	}
	if err != nil {
		return "", "is no amount of two decimals, SAP's internal form"
	}
	return value, ""
}

// tcurxNotes notes each TCURX row the conversion cannot use, when the
// extract's amounts are in internal form.
func (g *gen) tcurxNotes() {
	if g.opts.ExternalAmounts {
		return
	}
	for _, p := range g.problems {
		g.note("tcurx", "TCURX "+p.Message+"; amounts in "+p.CURRKEY+" are not converted")
	}
}
