package sapconfig

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// yenAndDinar is the mock with company 1000 in JPY and 2000 in KWD, their
// price tolerances JPY 1,000 and KWD 1.234 and company 1000's methods paid
// up to JPY 50,000, in SAP's internal form ("10.00", "12.34", "500.00").
func yenAndDinar() *sap.Extract {
	e := sap.GenerateMock(42, "2026")
	currencies := map[string]string{"1000": "JPY", "2000": "KWD"}
	for i := range e.T001 {
		if c, ok := currencies[e.T001[i].BUKRS]; ok {
			e.T001[i].WAERS = c
		}
	}
	werto := map[string]string{"1000": "10.00", "2000": "12.34"}
	for i := range e.T169G {
		if w, ok := werto[e.T169G[i].BUKRS]; ok {
			e.T169G[i].WERTO = w
		}
	}
	for i := range e.T042E {
		if e.T042E[i].ZBUKR == "1000" {
			e.T042E[i].BISBT = "500.00"
		}
	}
	return e
}

// TestDraftAmountForms (po:2026-11-09, D10): the draft converts the
// amounts it takes from SAP's internal form with the extract's TCURX, takes
// them as they are when the options say external, and never guesses: a
// currency of other than two decimals TCURX does not list keeps the
// default tolerance with a note, as does an amount of more than two
// decimals in internal form, and a TCURX row it cannot use is noted.
func TestDraftAmountForms(t *testing.T) {
	noTCURX := yenAndDinar()
	noTCURX.TCURX = nil
	fine := yenAndDinar()
	fine.T169G[0].WERTO = "10.005"
	fine.TCURX = append(fine.TCURX, sap.TCURX{CURRKEY: "OMR", CURRDEC: "x"})
	for name, c := range map[string]struct {
		e             *sap.Extract
		external      bool
		jpy, kwd, cad string
		notes         []string
	}{
		"internal with TCURX": {yenAndDinar(), false, "1000", "1.234", "5.00",
			[]string{`companies[1000].payment_methods[T]: SAP pays by the method only from "" to "50000" (T042E VONBT/BISBT)`}},
		"external": {yenAndDinar(), true, "10.00", "12.34", "5.00",
			[]string{`companies[1000].payment_methods[T]: SAP pays by the method only from "" to "500.00" (T042E VONBT/BISBT)`}},
		"internal without TCURX": {noTCURX, false, "5.00", "5.00", "5.00", []string{
			`companies[1000].invoice_tolerance.price_absolute: T169G key PP amount "10.00" is in SAP's internal form, and the extract's TCURX does not give the decimals of JPY; used 5.00`,
			`companies[2000].invoice_tolerance.price_absolute: T169G key PP amount "12.34" is in SAP's internal form, and the extract's TCURX does not give the decimals of KWD; used 5.00`,
			`from "" to "500.00" as exported, which is in SAP's internal form, and the extract's TCURX does not give the decimals of JPY (T042E`}},
		"internal with three decimals": {fine, false, "5.00", "1.234", "5.00", []string{
			`companies[1000].invoice_tolerance.price_absolute: T169G key PP amount "10.005" is no amount of two decimals, SAP's internal form; used 5.00`,
			`tcurx: TCURX CURRDEC "x" of currency OMR is not a number of decimals from 0 to 9; amounts in OMR are not converted`}},
	} {
		t.Run(name, func(t *testing.T) {
			opts := zoned()
			opts.ExternalAmounts = c.external
			cfg, notes := Generate(c.e, opts)
			for code, want := range map[string]string{"1000": c.jpy, "2000": c.kwd, "3000": c.cad} {
				if got := cfg.Company(code).InvoiceTolerance.PriceAbsolute; got != want {
					t.Errorf("company %s's price tolerance: got %s, want %s", code, got, want)
				}
			}
			all := notesText(notes)
			for _, want := range c.notes {
				if !strings.Contains(all, want) {
					t.Errorf("notes: got\n%s\nwant one containing %s", all, want)
				}
			}
		})
	}
}
