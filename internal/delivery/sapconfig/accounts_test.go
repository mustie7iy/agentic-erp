package sapconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// secondUSD gives company 3000's USD house bank MUSD a second account,
// 00002, on its own ledger account 103000, and has T042I name it for bank
// transfers in USD, so MUSD's first account, 00001, pays by no method.
func secondUSD(e *sap.Extract) {
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "3000", SAKNR: "0000103000", TXT50: "Bank - USD payments", XBILK: "X", WAERS: "USD"})
	e.T012K = append(e.T012K, sap.T012K{BUKRS: "3000", HBKID: "MUSD", HKONT: "0000103000", SWIFT: "ROYCCAT2", BANKS: "CA", BANKL: "000300001",
		BANKN: "5012347", WAERS: "USD", HKTID: "00002"})
	t042i(e, "MUSD", "00002")
}

// t042i sets the account T042I names for company 3000's bank.
func t042i(e *sap.Extract, bank, id string) {
	for i, r := range e.T042I {
		if r.ZBUKR == "3000" && r.HBKID == bank {
			e.T042I[i].HKTID = id
		}
	}
}

// TestHouseBankAccountByT042I: a house bank of several accounts, each with
// its ID (T012K HKTID), gives each account a house bank of its own, coded
// house bank and account ID (product owner 2026-11-10), and the payment
// method's ranking names the account T042I names for the method and
// currency (SAP specialist, po:2026-11-09, A4 [H]; ADR-0038 As built).
func TestHouseBankAccountByT042I(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	secondUSD(e)
	c, notes, r := FitGapReport(e, zoned())
	co := c.Company("3000")
	var banks []string
	for _, hb := range co.AllHouseBanks() {
		banks = append(banks, fmt.Sprint(hb.Name, " ", co.HouseBankCurrency(hb), " ", co.HouseBankAccount(hb)))
	}
	if want := []string{"MAIN CAD 100000", "MUSD00001 USD 102000", "MUSD00002 USD 103000"}; !slices.Equal(banks, want) {
		t.Errorf("house banks of 3000: got %v, want %v", banks, want)
	}
	if got := rankings(co.PaymentMethods[0]); got != "CAD: MAIN; USD: MUSD00002" {
		t.Errorf("ranking of method T: got %q, want MUSD's account 00002 for USD", got)
	}
	all := notesText(notes)
	for _, want := range []string{
		"companies[3000].house_banks[MUSD00002].name: house bank MUSD has several accounts in T012K: each is a house bank of its own, coded house bank and account ID (HBKID MUSD, HKTID 00002",
		"companies[3000].house_banks[MUSD00001].name: house bank MUSD has several accounts in T012K",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("notes: got\n%s\nwant one beginning %q", all, want)
		}
	}
	if strings.Contains(all, "T042I names") {
		t.Errorf("notes: got one on T042I, want none where it names an account of the bank:\n%s", all)
	}
	for _, key := range []string{"3000/MUSD/00001", "3000/MUSD/00002"} {
		if o := findObject(t, r, "T012K", key); o.Outcome != CarriedWithNote {
			t.Errorf("T012K %s: got %s, want %s", key, o.Outcome, CarriedWithNote)
		}
	}
	if got := methodErrors(reviewed(c)); len(got) != 0 {
		t.Errorf("payment method errors of the reviewed draft: got %v, want none", got)
	}
}

// TestHouseBankAccountNotNamed: where T042I names no account of a bank of
// several, or one T012K does not give the company, the ranking takes the
// bank's first account as before, or the code the account would have,
// which validation refuses, each noted; an account T042I names is chosen
// for a T042A row without a currency too, and its subaccount noted by it.
func TestHouseBankAccountNotNamed(t *testing.T) {
	const mp = "companies[3000].payment_methods[T]"
	for name, c := range map[string]struct {
		change  func(*sap.Extract)
		ranking string
		note    string
		unnoted string // a note the draft must not give
		errors  []string
	}{
		"T042I naming no account ID for a bank of one": {
			change:  func(e *sap.Extract) { t042i(e, "MAIN", "") },
			ranking: "CAD: MAIN; USD: MUSD", unnoted: "T042I names",
		},
		"T042I names no account": {
			change: func(e *sap.Extract) {
				secondUSD(e)
				e.T042I = slices.DeleteFunc(e.T042I, func(r sap.T042I) bool { return r.ZBUKR == "3000" && r.HBKID == "MUSD" })
			},
			ranking: "CAD: MAIN; USD: MUSD00001",
			note: mp + ".ranking: T042I names no account of house bank MUSD for the method for USD, so the bank is known but not its account " +
				"(SAP specialist, po:2026-11-09, A4): its first account MUSD00001 is ranked",
		},
		"T042I names no account for a row without a currency": {
			change: func(e *sap.Extract) {
				secondUSD(e)
				t042a(e, "MUSD", "", "00001")
				e.T042I = slices.DeleteFunc(e.T042I, func(r sap.T042I) bool { return r.ZBUKR == "3000" && r.HBKID == "MUSD" })
			},
			ranking: "CAD: MAIN; USD: MUSD00001",
			note:    mp + ".ranking: T042I names no account of house bank MUSD for the method without a currency",
		},
		"a row without a currency": {
			change:  func(e *sap.Extract) { secondUSD(e); t042a(e, "MUSD", "", "00001") },
			ranking: "CAD: MAIN; USD: MUSD00002",
		},
		"a row without a currency, its account named for two": {
			change: func(e *sap.Extract) {
				secondUSD(e)
				t042a(e, "MUSD", "", "00001")
				e.T042I = append(e.T042I, sap.T042I{ZBUKR: "3000", HBKID: "MUSD", ZLSCH: "T", HKTID: "00002", UKONT: "0000103000"})
			},
			ranking: "CAD: MAIN; USD: MUSD00002",
		},
		"an account T012K does not give": {
			change:  func(e *sap.Extract) { secondUSD(e); t042i(e, "MUSD", "00009") },
			ranking: "CAD: MAIN; USD: MUSD00009",
			note: mp + `.ranking: T042I names account "00009" of house bank MUSD for the method in "USD", which T012K does not give the company: ` +
				"ranked as MUSD00009, which validation refuses",
			errors: []string{mp + ".ranking[USD].house_banks"},
		},
		"an account of a bank of one that T012K does not give": {
			change:  func(e *sap.Extract) { t042i(e, "MAIN", "00009") },
			ranking: "CAD: MAIN00009; USD: MUSD",
			note:    mp + `.ranking: T042I names account "00009" of house bank MAIN for the method in "CAD"`,
			errors:  []string{mp + ".ranking[CAD].house_banks"},
		},
		"an account T012K does not give, for a row without a currency": {
			change:  func(e *sap.Extract) { t042a(e, "MAIN", "", "00001"); t042i(e, "MAIN", "00009") },
			ranking: "CAD: MAIN00009; USD: MUSD",
			errors:  []string{mp + ".ranking[CAD].house_banks"},
		},
		"the subaccount of the account named": {
			change: func(e *sap.Extract) {
				secondUSD(e)
				for i := range e.T042I {
					if e.T042I[i].ZBUKR == "3000" && e.T042I[i].HBKID == "MUSD" {
						e.T042I[i].UKONT = "0000103010"
					}
				}
			},
			ranking: "CAD: MAIN; USD: MUSD00002",
			note:    mp + `: SAP posts payments by the method from house bank MUSD00002 in "USD" to its bank subaccount 103010 (T042I UKONT); the platform credits the bank's one clearing account 103000`,
		},
		"accounts without IDs": {
			change: func(e *sap.Extract) {
				secondUSD(e)
				for i := range e.T012K {
					if e.T012K[i].HBKID == "MUSD" {
						e.T012K[i].HKTID = ""
					}
				}
			},
			ranking: "CAD: MAIN; USD: MUSD",
			note: "companies[3000].house_banks[MUSD]: house bank MUSD has several accounts in T012K; only the first is taken: the extract does not give " +
				"each an account ID of its own (HKTID)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			c.change(e)
			cfg, notes := Generate(e, zoned())
			if got := rankings(cfg.Company("3000").PaymentMethods[0]); got != c.ranking {
				t.Errorf("ranking: got %q, want %q", got, c.ranking)
			}
			if all := notesText(notes); c.note != "" && !strings.Contains(all, c.note) {
				t.Errorf("notes: got\n%s\nwant one beginning %q", all, c.note)
			}
			if all := notesText(notes); c.unnoted != "" && strings.Contains(all, c.unnoted) {
				t.Errorf("notes: got\n%s\nwant none with %q", all, c.unnoted)
			}
			if got, want := methodErrors(reviewed(cfg)), fmt.Sprint(c.errors); fmt.Sprint(got) != want {
				t.Errorf("payment method errors of the reviewed draft: got %v, want %s", got, want)
			}
		})
	}
}

// TestHouseBankOfOneAccountKeepsItsCode: a house bank of one account keeps
// its code (HBKID), whether or not the extract gives the account's ID, and
// its fit-gap key: the mock's banks are such, with no note on their names.
func TestHouseBankOfOneAccountKeepsItsCode(t *testing.T) {
	for name, id := range map[string]string{"with its ID": "00001", "without": ""} {
		t.Run(name, func(t *testing.T) {
			e := sap.GenerateMock(42, "2026")
			for i := range e.T012K {
				e.T012K[i].HKTID = id
			}
			c, notes, r := FitGapReport(e, zoned())
			if got := c.Company("3000").HouseBanks; len(got) != 1 || got[0].Name != "MUSD" {
				t.Errorf("further house banks of 3000: got %+v, want MUSD", got)
			}
			if all := notesText(notes); strings.Contains(all, ".name: house bank") {
				t.Errorf("notes: got one on a house bank's code, want none for banks of one account:\n%s", all)
			}
			findObject(t, r, "T012K", "3000/MUSD")
			if got := c.Company("3000").PaymentMethods[0]; rankings(got) != "CAD: MAIN; USD: MUSD" {
				t.Errorf("ranking: got %q, want CAD: MAIN; USD: MUSD", rankings(got))
			}
		})
	}
}
