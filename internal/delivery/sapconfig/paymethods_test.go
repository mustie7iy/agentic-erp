package sapconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// rankings is a method's ranking as "CAD: MAIN; USD: MUSD".
func rankings(m config.PaymentMethod) string {
	parts := make([]string, len(m.Ranking))
	for i, r := range m.Ranking {
		parts[i] = r.Currency + ": " + strings.Join(r.HouseBanks, " ")
	}
	return strings.Join(parts, "; ")
}

// TestPaymentMethodsFromT042: the mock's companies pay by bank transfer,
// set up in the company code (T042E), named in the company's country
// (T042Z) and ranking each house bank for its account's currency (T042A),
// so company 3000's draft ranks MAIN for CAD and MUSD for USD, with no
// note on its methods, and validates once reviewed (ADR-0038).
func TestPaymentMethodsFromT042(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), zoned())
	co := c.Company("3000")
	if len(co.PaymentMethods) != 1 {
		t.Fatalf("payment methods of 3000: got %+v, want T", co.PaymentMethods)
	}
	m := co.PaymentMethods[0]
	for name, f := range map[string]struct{ got, want string }{
		"code":            {m.Code, "T"},
		"description":     {m.Description, "Bank transfer"},
		"ranking":         {rankings(m), "CAD: MAIN; USD: MUSD"},
		"ranking of 1000": {rankings(c.Company("1000").PaymentMethods[0]), "USD: MAIN"},
	} {
		if f.got != f.want {
			t.Errorf("method T's %s: got %q, want %q", name, f.got, f.want)
		}
	}
	if all := notesText(notes); strings.Contains(all, "payment_methods") {
		t.Errorf("notes: got some on payment methods, want none for the mock's:\n%s", all)
	}
	if got := errorPaths(reviewed(c)); len(got) != 0 {
		t.Errorf("errors of the reviewed draft: got %v, want none", got)
	}
}

// secondCAD gives company 3000 a second CAD house bank, SCND, on its own
// ledger account, set up for bank transfers in T042I, and ranks it as
// rangf for CAD.
func secondCAD(e *sap.Extract, rangf string) {
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "3000", SAKNR: "0000101000", TXT50: "Bank - second CAD", XBILK: "X"})
	e.T012K = append(e.T012K, sap.T012K{BUKRS: "3000", HBKID: "SCND", HKONT: "0000101000", SWIFT: "TDOMCATT", BANKS: "CA", BANKL: "000400002",
		BANKN: "7012345", WAERS: "CAD"})
	e.T042A = append(e.T042A, sap.T042A{ZBUKR: "3000", ZLSCH: "T", WAERS: "CAD", RANGF: rangf, HBKID: "SCND"})
}

// t042a sets the T042A row of company 3000's bank to currency and order.
func t042a(e *sap.Extract, bank, currency, rangf string) {
	for i, a := range e.T042A {
		if a.ZBUKR == "3000" && a.HBKID == bank {
			e.T042A[i].WAERS, e.T042A[i].RANGF = currency, rangf
		}
	}
}

// TestPaymentMethodsNotedAndRefused: what SAP does not give is left in a
// form validation refuses, with a note, and what the configuration has no
// counterpart for is noted; the ranking follows T042A's order and its
// rows without a currency.
func TestPaymentMethodsNotedAndRefused(t *testing.T) {
	const mp = "companies[3000].payment_methods[T]"
	for name, c := range map[string]struct {
		change  func(*sap.Extract)
		ranking string
		note    string
		errors  []string
	}{
		"no name in the company's country": {
			change: func(e *sap.Extract) {
				e.T042Z = slices.DeleteFunc(e.T042Z, func(z sap.T042Z) bool { return z.LAND1 == "CA" })
			},
			ranking: "CAD: MAIN; USD: MUSD", note: mp + ".description: T042Z names no payment method T for the company's country CA",
			errors: []string{mp + ".description"},
		},
		"no ranking": {
			change: func(e *sap.Extract) {
				e.T042A = slices.DeleteFunc(e.T042A, func(a sap.T042A) bool { return a.ZBUKR == "3000" })
			},
			note:   mp + ".ranking: T042A ranks no house bank for the method",
			errors: []string{mp + ".ranking"},
		},
		"a bank the company does not have": {
			change: func(e *sap.Extract) {
				e.T042A = append(e.T042A, sap.T042A{ZBUKR: "3000", ZLSCH: "T", WAERS: "CAD", RANGF: "00002", HBKID: "GONE"})
			},
			ranking: "CAD: MAIN GONE; USD: MUSD", note: mp + ".ranking: T042A ranks house bank GONE, which T012K does not give the company",
			errors: []string{mp + ".ranking[CAD].house_banks"},
		},
		"a bank ranked twice": {
			change: func(e *sap.Extract) {
				e.T042A = append(e.T042A, sap.T042A{ZBUKR: "3000", ZLSCH: "T", WAERS: "CAD", RANGF: "00002", HBKID: "MAIN"})
			},
			ranking: "CAD: MAIN MAIN; USD: MUSD", note: mp + `.ranking: T042A ranks house bank MAIN twice for CAD`,
			errors: []string{mp + ".ranking[CAD].house_banks"},
		},
		"a bank ranked for another currency": {
			change:  func(e *sap.Extract) { t042a(e, "MUSD", "CAD", "00002") },
			ranking: "CAD: MAIN MUSD", note: mp + ".ranking[CAD]: T042A ranks house bank MUSD, an account in USD, for payments in CAD",
			errors: []string{mp + ".ranking[CAD].house_banks"},
		},
		"a bank ranked twice without a currency": {
			change: func(e *sap.Extract) {
				t042a(e, "MAIN", "", "00001")
				e.T042A = append(e.T042A, sap.T042A{ZBUKR: "3000", ZLSCH: "T", RANGF: "00003", HBKID: "MAIN"})
			},
			ranking: "CAD: MAIN MAIN; USD: MUSD", note: mp + ".ranking: T042A ranks house bank MAIN twice without a currency",
			errors: []string{mp + ".ranking[CAD].house_banks"},
		},
		"rows without a currency": {
			change:  func(e *sap.Extract) { t042a(e, "MAIN", "", "00001"); t042a(e, "MUSD", "", "00002") },
			ranking: "CAD: MAIN; USD: MUSD", note: mp + ".ranking: T042A ranks house banks without a currency",
		},
		"a row without a currency where the currency is ranked": {
			change:  func(e *sap.Extract) { secondCAD(e, "00001"); t042a(e, "MAIN", "", "00001") },
			ranking: "CAD: SCND; USD: MUSD",
			note:    mp + ".ranking[CAD]: T042A ranks house bank MAIN without a currency, which SAP uses only where the method ranks no bank for the currency",
		},
		"the ranking order": {
			change:  func(e *sap.Extract) { secondCAD(e, "00001"); t042a(e, "MAIN", "CAD", "00002") },
			ranking: "CAD: SCND MAIN; USD: MUSD",
		},
		"a ranking order that is not a number": {
			change:  func(e *sap.Extract) { secondCAD(e, "00001"); t042a(e, "MAIN", "CAD", "x") },
			ranking: "CAD: SCND MAIN; USD: MUSD", note: mp + `.ranking: T042A ranking order "x" of house bank MAIN is not a number; ranked last`,
		},
		"amount limits": {
			change: func(e *sap.Extract) {
				for i := range e.T042E {
					if e.T042E[i].ZBUKR == "3000" {
						e.T042E[i].BISBT = "50000.00"
					}
				}
			},
			ranking: "CAD: MAIN; USD: MUSD", note: mp + `: SAP pays by the method only from "" to "50000.00" (T042E VONBT/BISBT)`,
		},
		"a bank subaccount": {
			change: func(e *sap.Extract) {
				for i := range e.T042I {
					if e.T042I[i].ZBUKR == "3000" && e.T042I[i].HBKID == "MAIN" {
						e.T042I[i].UKONT = "0000100010"
					}
				}
				e.T042I = append(e.T042I, sap.T042I{ZBUKR: "3000", HBKID: "GONE", ZLSCH: "T", WAERS: "CAD", UKONT: "0000100020"})
			},
			ranking: "CAD: MAIN; USD: MUSD",
			note:    mp + `: SAP posts payments by the method from house bank MAIN in "CAD" to its bank subaccount 100010 (T042I UKONT)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			c.change(e)
			cfg, notes := Generate(e, zoned())
			co := cfg.Company("3000")
			if len(co.PaymentMethods) != 1 {
				t.Fatalf("payment methods of 3000: got %+v, want T", co.PaymentMethods)
			}
			if got := rankings(co.PaymentMethods[0]); got != c.ranking {
				t.Errorf("ranking: got %q, want %q", got, c.ranking)
			}
			if all := notesText(notes); c.note != "" && !strings.Contains(all, c.note) {
				t.Errorf("notes: got\n%s\nwant one beginning %q", all, c.note)
			}
			if got, want := methodErrors(reviewed(cfg)), fmt.Sprint(c.errors); fmt.Sprint(got) != want {
				t.Errorf("payment method errors of the reviewed draft: got %v, want %s", got, want)
			}
		})
	}
}

// methodErrors are the validation errors on the company's payment methods.
func methodErrors(c *config.Config) []string {
	out := []string{}
	for _, p := range errorPaths(c) {
		if strings.Contains(p, "payment_methods") {
			out = append(out, p)
		}
	}
	return out
}

// TestPaymentMethodsNotSetUp: a company code without T042E has no
// payment methods, noted, and a method T042A ranks banks for without
// T042E is left out with a note.
func TestPaymentMethodsNotSetUp(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T042E = slices.DeleteFunc(e.T042E, func(r sap.T042E) bool { return r.ZBUKR == "3000" })
	c, notes := Generate(e, zoned())
	if got := c.Company("3000").PaymentMethods; len(got) != 0 {
		t.Errorf("payment methods of 3000: got %+v, want none", got)
	}
	all := notesText(notes)
	for _, want := range []string{
		"companies[3000].payment_methods: T042A ranks house banks for payment method T, which T042E does not set up for the company: left out",
		"companies[3000].payment_methods: T042E sets up no payment method for the company",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("notes: got\n%s\nwant one beginning %q", all, want)
		}
	}
	if strings.Count(all, "which T042E does not set up") != 1 {
		t.Errorf("notes: got\n%s\nwant method T's T042A rows noted once", all)
	}
}
