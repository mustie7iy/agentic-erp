package integration

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// withDocument appends a vendor document of company 2000 (KZ, so dpTarget's
// number range takes it) of two lines: 170000 debited and the given line
// credited, both in the given document currency, the document amount
// (WSL) twice the company code amount (HSL) unless the line gives one.
func withDocument(e *sap.Extract, belnr, currency string, credit sap.ACDOCA) {
	if credit.WSL == "" {
		credit.WSL = twice(credit.HSL)
	}
	debit := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: belnr, DOCLN: "000001", RACCT: "0000170000",
		HSL: strings.TrimPrefix(credit.HSL, "-"), WSL: strings.TrimPrefix(credit.WSL, "-"), BUDAT: "20260310", BLART: "KZ", RWCUR: currency}
	credit.RLDNR, credit.RBUKRS, credit.GJAHR, credit.BELNR, credit.DOCLN, credit.BUDAT, credit.BLART, credit.RWCUR =
		"0L", "2000", "2026", belnr, "000002", "20260310", "KZ", currency
	e.ACDOCA = append(e.ACDOCA, debit, credit)
}

// twice is an SAP amount doubled: 50.00 -> 100.00, -0.50 -> -1.00; one
// that does not parse is returned as it is.
func twice(hsl string) string {
	m, err := (target{exponents: map[string]int{"X": 2}}).amount(hsl, "X")
	if err != nil {
		return hsl
	}
	return (2 * m).Plain(2)
}

// inUSD puts dpExtract's down payment document in USD, each line's
// document amount twice its company code amount.
func inUSD(e *sap.Extract) {
	for i := range e.ACDOCA {
		if e.ACDOCA[i].BELNR == "0100000009" {
			e.ACDOCA[i].RWCUR, e.ACDOCA[i].WSL = "USD", twice(e.ACDOCA[i].HSL)
		}
	}
}

// withGRIRPosition gives company 2000 the GR/IR account 219000 and an open
// position: order 4500000001's item 00010 received (MSEG 101) and not
// invoiced, its GR/IR line in document 0100000012 in the given currency.
func withGRIRPosition(e *sap.Extract, t *target, currency string) {
	t.charts["2000"]["219000"] = ledger.ChartAccount{GRIR: true}
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000219000"})
	e.EKKO = []sap.EKKO{{EBELN: "4500000001", BUKRS: "2000", LIFNR: "0000000001", WAERS: "EUR", BEDAT: "20260301"}}
	e.EKPO = []sap.EKPO{{EBELN: "4500000001", EBELP: "00010", MENGE: "2", NETPR: "50.00", PEINH: "1", SAKTO: "0000170000"}}
	e.MSEG = []sap.MSEG{{MBLNR: "5000000001", MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: "4500000001", EBELP: "00010", MENGE: "2",
		BUDAT: "20260305"}}
	withDocument(e, "0100000012", currency, sap.ACDOCA{RACCT: "0000219000", HSL: "-100.00", EBELN: "4500000001", EBELP: "00010"})
}

// documentCurrencyCase changes dpExtract and dpTarget (which knows USD)
// and names the journal errors the quality gate then reports, in order,
// each by the start of its key and message.
type documentCurrencyCase struct {
	name   string
	change func(e *sap.Extract, t *target)
	want   []string
}

// TestDocumentCurrencyQuality: the quality gate lets an open item in a
// foreign document currency (ACDOCA RWCUR) through when its document can
// post in that currency, a down payment or an open vendor item, and
// refuses one without a document currency rather than assume the
// company's, a line of an open GR/IR position in another currency than
// its order's, and one whose document's lines are not all in its
// currency, naming the document; items in the company's currency, lines SAP cleared, lines
// that are no item and lines of no amount are not refused.
func TestDocumentCurrencyQuality(t *testing.T) {
	runDocumentCurrencyCases(t, []documentCurrencyCase{
		{"in the company's currency", func(e *sap.Extract, t *target) {
			withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "-50.00", LIFNR: "0000000001"})
			withGRIRPosition(e, t, "EUR")
		}, nil},
		{"a down payment in a foreign currency", func(e *sap.Extract, _ *target) { inUSD(e) }, nil},
		{"a down payment one of whose document's lines is in a foreign currency", func(e *sap.Extract, _ *target) { e.ACDOCA[0].RWCUR = "USD" },
			[]string{"2000/2026/0100000009/000001: open item of SAP document 0100000009 is in document currency USD, and not every line of its document is"}},
		{"a down payment without a document currency", func(e *sap.Extract, _ *target) { e.ACDOCA[0].RWCUR = "" },
			[]string{"2000/2026/0100000009/000001: open item of SAP document 0100000009 has no document currency (RWCUR)"}},
		{"an open vendor item in a foreign currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
		}, nil},
		{"an open vendor item without a document currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
		}, []string{"2000/2026/0100000011/000002: open item of SAP document 0100000011 has no document currency (RWCUR)"}},
		{"a cleared vendor item in a foreign currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001", AUGBL: "0100000013"})
		}, nil},
		{"a vendor line of no amount without a document currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "", sap.ACDOCA{RACCT: "0000211000", HSL: "0.00", LIFNR: "0000000001"})
		}, nil},
		{"an open GR/IR line in a foreign currency of an order in company currency", func(e *sap.Extract, t *target) { withGRIRPosition(e, t, "USD") },
			[]string{"2000/2026/0100000012/000002: open item of SAP document 0100000012 is in document currency USD, but its order 4500000001 is in " +
				"company 2000's currency EUR: an order's GR/IR positions are taken over in the order's currency"}},
		{"an open GR/IR position without a document currency", func(e *sap.Extract, t *target) { withGRIRPosition(e, t, "") },
			[]string{"2000/2026/0100000012/000002: open item of SAP document 0100000012 has no document currency (RWCUR)"}},
		{"the GR/IR lines of a closed position in a foreign currency", func(e *sap.Extract, t *target) {
			withGRIRPosition(e, t, "USD")
			e.MSEG = nil // nothing received, and the receipt's GR/IR line reversed: no balance and no open quantity
			reversal := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000014", BUDAT: "20260310", BLART: "KZ", RWCUR: "USD"}
			grir, offset := reversal, reversal
			grir.DOCLN, grir.RACCT, grir.HSL, grir.WSL, grir.EBELN, grir.EBELP = "000001", "0000219000", "100.00", "200.00", "4500000001", "00010"
			offset.DOCLN, offset.RACCT, offset.HSL, offset.WSL = "000002", "0000170000", "-100.00", "-200.00"
			e.ACDOCA = append(e.ACDOCA, grir, offset)
		}, nil},
		{"a line on an ordinary account in a foreign currency", func(e *sap.Extract, _ *target) { e.ACDOCA[2].RWCUR = "USD" }, nil},
	})
}

// TestForeignDocumentQuality: a document that posts in a foreign currency
// is refused when the platform does not know its currency, a document
// amount (WSL) does not parse, a line has a document amount without a
// company code amount, a vendor line has none in document currency, a
// line's two amounts have opposite signs, the document does not balance
// in its currency or the rate its amounts imply rounds to zero; SAP's
// exchange difference, an amount in company currency alone, is not. An
// open item SAP cleared a line of together with a document in another
// currency is refused.
func TestForeignDocumentQuality(t *testing.T) {
	open := func(e *sap.Extract, wsl string) {
		withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: wsl, LIFNR: "0000000001"})
	}
	last := func(e *sap.Extract, n int) *sap.ACDOCA { return &e.ACDOCA[len(e.ACDOCA)-n] }
	extra := func(docln, account, hsl, wsl string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000011", DOCLN: docln, RACCT: account, HSL: hsl, WSL: wsl,
			BUDAT: "20260310", BLART: "KZ", RWCUR: "USD"}
	}
	runDocumentCurrencyCases(t, []documentCurrencyCase{
		{"a currency the platform does not know", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "XXX", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
		}, []string{"2000/2026/0100000011: document currency XXX (RWCUR) is not a currency the platform knows"}},
		{"a document amount that does not parse", func(e *sap.Extract, _ *target) { open(e, "-1x") },
			[]string{"2000/2026/0100000011/000001: document amount (WSL)", "2000/2026/0100000011/000002: document amount (WSL)"}},
		{"a document amount without a company code amount", func(e *sap.Extract, _ *target) {
			open(e, "-100.00")
			e.ACDOCA = append(e.ACDOCA, extra("000003", "0000170000", "0.00", "1.00"), extra("000004", "0000100000", "0.00", "-1.00"))
		}, []string{"2000/2026/0100000011/000003: line has 1.00 USD in its document currency (WSL) and no amount in company currency (HSL)",
			"2000/2026/0100000011/000004: line has -1.00 USD"}},
		{"an exchange difference in company currency alone", func(e *sap.Extract, _ *target) {
			open(e, "-100.00")
			last(e, 2).HSL = "49.00"
			e.ACDOCA = append(e.ACDOCA, extra("000003", "0000100000", "1.00", "0.00"))
		}, nil},
		{"a vendor line without a document amount", func(e *sap.Extract, _ *target) {
			open(e, "0.00")
			last(e, 2).WSL = "100.00"
		}, []string{"2000/2026/0100000011/000002: vendor line has no amount in its document currency USD (WSL)"}},
		{"amounts of opposite signs", func(e *sap.Extract, _ *target) {
			open(e, "100.00")
			last(e, 2).WSL = "-100.00"
		}, []string{"2000/2026/0100000011/000001: line's amounts in document currency (WSL -100.00 USD) and company currency (HSL 50.00 EUR) have " +
			"opposite signs", "2000/2026/0100000011/000002: line's amounts"}},
		{"a document that does not balance in its currency", func(e *sap.Extract, _ *target) {
			open(e, "-100.00")
			last(e, 2).WSL = "99.00"
		}, []string{"2000/2026/0100000011: document does not balance in its document currency USD (WSL, off by -100 minor units)"}},
		{"a rate that rounds to zero", func(e *sap.Extract, _ *target) { open(e, "-900000000000000.00") },
			[]string{"2000/2026/0100000011: the rate from USD to EUR that the document's amounts imply is not above zero"}},
		{"a rate of 10^16", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-100000000000000.00", WSL: "-0.01", LIFNR: "0000000001"})
		}, []string{"2000/2026/0100000011: the rate from USD to EUR that the document's amounts imply is not above zero to the 12 decimals a " +
			"journal entry records, or not below 10^16"}},
		{"document amounts out of range together", func(e *sap.Extract, _ *target) { open(e, "-50000000000000000.00") },
			[]string{"2000/2026/0100000011: the document amounts (WSL) of the document add up to more than an amount holds"}},
		{"no document amount on any line", func(e *sap.Extract, _ *target) {
			open(e, "0.00")
			last(e, 2).WSL = "0.00"
		}, []string{"2000/2026/0100000011/000002: open item of SAP document 0100000011 is in document currency USD, but no line of its " +
			"document has an amount in it (WSL)"}},
		{"no document amount on a cleared document", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "0.00", LIFNR: "0000000001", AUGBL: "0100000013"})
			last(e, 2).WSL = "0.00"
		}, nil},
		{"cleared together with a document in another currency", func(e *sap.Extract, _ *target) {
			open(e, "-100.00")
			last(e, 2).HSL, last(e, 2).WSL = "51.00", "102.00"
			cleared := extra("000003", "0000211000", "-1.00", "-2.00")
			cleared.LIFNR, cleared.AUGBL = "0000000001", "0100000013"
			e.ACDOCA = append(e.ACDOCA, cleared)
			withDocument(e, "0100000013", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "-50.00", LIFNR: "0000000001", AUGBL: "0100000013"})
		}, []string{"2000/2026/0100000011/000002: open item of SAP document 0100000011 is in document currency USD, and SAP cleared its document " +
			"together with documents in another currency or without one (AUGBL)"}},
	})
}

// TestForeignDocumentEarlierRuns: documents SAP cleared together with
// documents an earlier run loaded post in the currency those were posted
// in: a USD payment of a USD item loaded earlier posts in USD, and one
// in EUR, or a clearing whose earlier documents were posted in two
// currencies, is refused, naming the currency; an item loaded earlier in
// company currency takes a USD payment in it.
func TestForeignDocumentEarlierRuns(t *testing.T) {
	paid := func(e *sap.Extract, currency, wsl string) {
		withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001", AUGBL: "0100000013"})
		withDocument(e, "0100000013", currency, sap.ACDOCA{RACCT: "0000100000", HSL: "-50.00", WSL: "-" + wsl})
		pay := &e.ACDOCA[len(e.ACDOCA)-2]
		pay.RACCT, pay.LIFNR, pay.AUGBL = "0000211000", "0000000001", "0100000013"
	}
	posted := func(t *target, currencies ...string) {
		t.posted = map[string]string{"2000/2026/0100000011": currencies[0]}
		if len(currencies) > 1 {
			t.posted["2000/2026/0100000013"] = currencies[1]
		}
	}
	runDocumentCurrencyCases(t, []documentCurrencyCase{
		{"paid in USD", func(e *sap.Extract, t *target) { paid(e, "USD", "100.00"); posted(t, "USD") }, nil},
		{"paid in EUR", func(e *sap.Extract, t *target) { paid(e, "EUR", "50.00"); posted(t, "USD") },
			[]string{"2000/2026/0100000013: SAP document 2000/2026/0100000013 cannot load in a currency its clearing can be taken over in: SAP " +
				"cleared it together with documents an earlier run loaded in USD, and 2000/2026/0100000013 is not in USD"}},
		{"loaded in two currencies", func(e *sap.Extract, t *target) {
			paid(e, "USD", "100.00")
			posted(t, "USD", "EUR")
			withDocument(e, "0100000015", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-1.00", LIFNR: "0000000001", AUGBL: "0100000013"})
		}, []string{"2000/2026/0100000015: SAP document 2000/2026/0100000015 cannot load in a currency its clearing can be taken over in: " +
			"earlier runs loaded the documents SAP cleared together with it in EUR and USD"}},
		{"loaded in company currency", func(e *sap.Extract, t *target) { paid(e, "USD", "100.00"); posted(t, "EUR") }, nil},
	})
}

// TestSourceRateWithoutAmount: a document with no document amount implies
// no rate.
func TestSourceRateWithoutAmount(t *testing.T) {
	tg := dpTarget()
	if _, ok := sourceRate([]sap.ACDOCA{{WSL: "0.00", HSL: "1.00"}}, "EUR", "EUR", tg); ok {
		t.Error("rate of a document without document amounts: got one, want none")
	}
}

// TestForeignDownPaymentTax: a down payment in a foreign currency has its
// tax checked in its document currency (where SAP is assumed to calculate
// it, for an SAP specialist to confirm): 19%
// of its document amount passes whatever its company code amounts round
// to, and a tax that is not is refused in that currency; a down payment
// or tax amount in it that does not parse is foreignDocument's to refuse.
func TestForeignDownPaymentTax(t *testing.T) {
	runDocumentCurrencyCases(t, []documentCurrencyCase{
		{"19% in document currency", func(e *sap.Extract, _ *target) {
			inUSD(e)
			e.ACDOCA[1].HSL, e.ACDOCA[2].HSL = "191.00", "-1191.00"
		}, nil},
		{"not 19% in document currency", func(e *sap.Extract, _ *target) {
			inUSD(e)
			e.ACDOCA[1].WSL, e.ACDOCA[2].WSL = "381.00", "-2381.00"
		}, []string{"2000/2026/0100000009: the down payment's tax on V1 is 381.00 USD, not 19% of the down payment, 380.00"}},
		{"a document amount that does not parse", func(e *sap.Extract, _ *target) {
			inUSD(e)
			e.ACDOCA[0].WSL = "x"
		}, []string{"2000/2026/0100000009/000001: document amount (WSL)"}},
		{"a tax amount that does not parse", func(e *sap.Extract, _ *target) {
			inUSD(e)
			e.ACDOCA[1].WSL = "x"
		}, []string{"2000/2026/0100000009/000002: document amount (WSL)"}},
	})
}

// TestForeignDocumentBalances: in balances mode a document holding an open
// item in a foreign currency loads on its own, so the gate refuses one
// with a vendor line SAP cleared and one whose document type has no number
// range, as it does a down payment's document.
func TestForeignDocumentBalances(t *testing.T) {
	runDocumentCurrencyCases(t, []documentCurrencyCase{
		{"loads on its own", func(e *sap.Extract, t *target) {
			t.balances = true
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
		}, nil},
		{"a cleared vendor line and no number range", func(e *sap.Extract, t *target) {
			t.balances = true
			delete(t.ranges, "2000/journal_kz/2026")
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
			debit := &e.ACDOCA[len(e.ACDOCA)-2]
			debit.RACCT, debit.LIFNR, debit.AUGBL = "0000211000", "0000000001", "0100000020"
		}, []string{"2000/2026/0100000009: the configuration has no journal_kz number range for the down payment's document",
			"2000/2026/0100000011/000001: the document in USD has a vendor line cleared by 0100000020",
			"2000/2026/0100000011: the configuration has no journal_kz number range for the document in USD in 2026"}},
	})
}

// runDocumentCurrencyCases runs each case's change on dpExtract and
// dpTarget, with USD known, and compares the gate's journal errors.
func runDocumentCurrencyCases(t *testing.T, cases []documentCurrencyCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			tg.exponents["USD"] = 2
			c.change(e, &tg)
			var got []string
			for _, i := range checkQuality(e, tg) {
				if i.Severity == SevError && i.Table == "ACDOCA" {
					got = append(got, i.Key+": "+i.Message)
				}
			}
			if len(got) != len(c.want) {
				t.Fatalf("errors: got %d %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			for n := range got {
				if !strings.HasPrefix(got[n], c.want[n]) {
					t.Errorf("error %d: got %q, want one starting %q", n, got[n], c.want[n])
				}
			}
		})
	}
}

// TestDocumentCurrencyQualityPasses: an extract whose items are all in the
// company's currency, with an open vendor item and an open GR/IR position
// beside the open down payment, has no errors at all, so the cases above
// that expect none test the document currency checks alone.
func TestDocumentCurrencyQualityPasses(t *testing.T) {
	e, tg := dpExtract(), dpTarget()
	withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "-50.00", LIFNR: "0000000001"})
	withGRIRPosition(e, &tg, "EUR")
	for _, i := range checkQuality(e, tg) {
		if i.Severity == SevError {
			t.Errorf("errors: got %s %s: %s, want none", i.Table, i.Key, i.Message)
		}
	}
}

// TestDocumentAmountAlone (review:batch31): an open vendor item with an
// amount in its document currency (WSL) and none in company currency
// (HSL), which the load would post no line for, is refused naming its
// document and line, in both modes, whatever its document posts in: in
// company currency, in a document not all in its currency or cleared
// together with one in another, without a document currency (or without
// either, as an OData extract), and in a foreign currency; and so is a
// line of an open GR/IR position, in company currency for its document
// amount alone and in a foreign one as any such line. One that
// carries no document amount in company currency has none, and one with
// a company code amount alone in company currency loads from it.
func TestDocumentAmountAlone(t *testing.T) {
	last := func(e *sap.Extract) *sap.ACDOCA { return &e.ACDOCA[len(e.ACDOCA)-1] }
	// alone appends document 0100000011 in the given currencies: line 1
	// on 170000, line 2 the vendor's of the given document amount, both of
	// no company code amount.
	alone := func(e *sap.Extract, debit, credit, wsl string) {
		withDocument(e, "0100000011", debit, sap.ACDOCA{RACCT: "0000211000", HSL: "0.00", WSL: wsl, LIFNR: "0000000001"})
		last(e).RWCUR = credit
	}
	const item = "2000/2026/0100000011/000002: open item of SAP document 0100000011 "
	// debit is line 1 refused in documents mode, where its document posts
	// as an entry of its own in company currency (leftOutLines); in
	// balances mode it is part of the opening entry.
	const debit = "2000/2026/0100000011/000001: line of SAP document 0100000011 has a document amount (WSL "
	inUSD := []string{"2000/2026/0100000011/000001: line has 0.01 USD in its document currency (WSL) and no amount in company currency (HSL)",
		"2000/2026/0100000011/000002: line has -0.01 USD in its document currency (WSL) and no amount in company currency (HSL)"}
	// grir adds to an open GR/IR position (withGRIRPosition, in EUR)
	// document 0100000011 in the given currency, its GR/IR line on the
	// position's order item of no company code amount.
	var tg *target
	grir := func(e *sap.Extract, currency string) {
		withGRIRPosition(e, tg, "EUR")
		withDocument(e, "0100000011", currency, sap.ACDOCA{RACCT: "0000219000", HSL: "0.00", WSL: "-5.00", EBELN: "4500000001", EBELP: "00010"})
	}
	const grirItem = "2000/2026/0100000011/000002: open item of SAP document 0100000011 "
	const grirForeign = "is in document currency USD, but its order 4500000001 is in company 2000's currency EUR: an order's GR/IR positions are taken over"
	cases := []struct {
		name                string
		change              func(e *sap.Extract)
		documents, balances []string
	}{
		{"in company currency", func(e *sap.Extract) { alone(e, "EUR", "EUR", "-0.01") },
			[]string{item + `has no amount in company currency EUR (HSL), but its document amount (WSL) is "-0.01"`, debit + `"0.01"`},
			[]string{item + `has no amount in company currency EUR (HSL), but its document amount (WSL) is "-0.01"`}},
		{"in company currency, not a number", func(e *sap.Extract) { alone(e, "EUR", "EUR", "-x") },
			[]string{item + `has no amount in company currency EUR (HSL), but its document amount (WSL) is "-x"`, debit + `"x"`},
			[]string{item + `has no amount in company currency EUR (HSL), but its document amount (WSL) is "-x"`}},
		{"in a document not all in its currency", func(e *sap.Extract) { alone(e, "EUR", "USD", "-0.01") },
			[]string{item + "is in document currency USD, and not every line of its document is", debit + `"0.01", RWCUR "EUR")`},
			[]string{item + "is in document currency USD, and not every line of its document is"}},
		{"without a document currency", func(e *sap.Extract) { alone(e, "", "", "-0.01") },
			[]string{item + "has no document currency (RWCUR)", debit + `"0.01", RWCUR "")`}, []string{item + "has no document currency (RWCUR)"}},
		{"without a document currency or amount", func(e *sap.Extract) { alone(e, "", "", ""); last(e).WSL = "" },
			[]string{item + "has no document currency (RWCUR)"}, []string{item + "has no document currency (RWCUR)"}},
		{"in a foreign currency", func(e *sap.Extract) { alone(e, "USD", "USD", "-0.01") }, inUSD, inUSD},
		{"cleared together with a document in another currency", func(e *sap.Extract) {
			alone(e, "USD", "USD", "-0.01")
			debit := &e.ACDOCA[len(e.ACDOCA)-2]
			debit.HSL, debit.WSL = "1.00", "2.01"
			e.ACDOCA = append(e.ACDOCA, sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000011", DOCLN: "000003",
				RACCT: "0000211000", HSL: "-1.00", WSL: "-2.00", BUDAT: "20260310", BLART: "KZ", RWCUR: "USD", LIFNR: "0000000001", AUGBL: "0100000013"})
			withDocument(e, "0100000013", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "-50.00", LIFNR: "0000000001", AUGBL: "0100000013"})
		}, []string{item + "is in document currency USD, and SAP cleared its document together with documents in another currency"},
			// balances mode takes no clearing over: the document posts on its own in USD
			inUSD[1:]},
		{"a GR/IR line of an open position in company currency", func(e *sap.Extract) { grir(e, "EUR") },
			[]string{grirItem + `has no amount in company currency EUR (HSL), but its document amount (WSL) is "-5.00"`, debit + `"5.00"`},
			[]string{grirItem + `has no amount in company currency EUR (HSL), but its document amount (WSL) is "-5.00"`}},
		{"a GR/IR line of an open position in a foreign currency", func(e *sap.Extract) { grir(e, "USD") },
			[]string{grirItem + grirForeign, "2000/2026/0100000011/000001: line has 5.00 USD", "2000/2026/0100000011/000002: line has -5.00 USD"},
			[]string{grirItem + grirForeign}},
		{"carrying no document amount in company currency", func(e *sap.Extract) { alone(e, "EUR", "EUR", ""); last(e).WSL = "" }, nil, nil},
		{"a company code amount alone in company currency", func(e *sap.Extract) {
			withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "0.00", LIFNR: "0000000001"})
		}, nil, nil},
	}
	for _, balances := range []bool{false, true} {
		var run []documentCurrencyCase
		for _, c := range cases {
			want, mode := c.documents, "documents"
			if balances {
				want, mode = c.balances, "balances"
			}
			run = append(run, documentCurrencyCase{mode + ": " + c.name, func(e *sap.Extract, t *target) { t.balances, tg = balances, t; c.change(e) }, want})
		}
		runDocumentCurrencyCases(t, run)
	}
}

// TestLeftOutLines (review:batch31): a line that is no open item, with a
// document amount (WSL) and no company code amount (HSL), in a document
// that posts as an entry of its own in company currency, which the load
// would leave out, is refused naming its document and line: a G/L line of
// a document whose lines mix currencies, a cleared vendor line, a WSL that
// does not parse, and in balances mode a line of a down payment's document
// in company currency. Not refused: a line that carries no WSL, or whose
// WSL is zero; in balances mode a document of the opening entry, which
// carries company code amounts alone; a document an earlier run posted;
// and a document refused as a conflict, which is refused whole.
func TestLeftOutLines(t *testing.T) {
	const gl = "2000/2026/0100000011/000001: line of SAP document 0100000011 has a document amount (WSL "
	// mixed appends document 0100000011: line 1 on 170000 in USD of the
	// given document amount and none in EUR, line 2 an open EUR vendor item.
	mixed := func(e *sap.Extract, wsl string) {
		withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", WSL: "-50.00", LIFNR: "0000000001"})
		debit := &e.ACDOCA[len(e.ACDOCA)-2]
		debit.HSL, debit.WSL, debit.RWCUR = "0.00", wsl, "USD"
		e.ACDOCA = append(e.ACDOCA, sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000011", DOCLN: "000003",
			RACCT: "0000170000", HSL: "50.00", WSL: "50.00", BUDAT: "20260310", BLART: "KZ", RWCUR: "EUR"})
	}
	// dpLineAlone adds to the down payment's EUR document a line on 170000
	// of 0.01 USD and no EUR amount.
	dpLineAlone := func(e *sap.Extract) {
		l := dpLine("000004", "0000170000", "0.00", "", "", "")
		l.WSL, l.RWCUR = "0.01", "USD"
		e.ACDOCA = append(e.ACDOCA, l)
	}
	// dpAlone is the refusal of dpLineAlone's line, in either mode: the
	// document posts on its own in both.
	const dpAlone = "2000/2026/0100000009/000004: line of SAP document 0100000009 has a document amount (WSL \"0.01\", RWCUR \"USD\") and no " +
		"amount in company currency EUR (HSL): its document posts in company currency from HSL, so the load would leave the line out; " +
		"have SAP's line corrected"
	cases := []struct {
		name                string
		change              func(e *sap.Extract, t *target)
		documents, balances []string
	}{
		{"a G/L line of a document whose lines mix currencies", func(e *sap.Extract, _ *target) { mixed(e, "0.01") },
			[]string{gl + `"0.01", RWCUR "USD") and no amount in company currency EUR (HSL): its document posts in company currency from HSL, ` +
				"so the load would leave the line out; have SAP's line corrected"}, nil},
		{"a document amount that does not parse", func(e *sap.Extract, _ *target) { mixed(e, "x") },
			[]string{gl + `"x", RWCUR "USD")`}, nil},
		{"a cleared vendor line", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "0.00", WSL: "-0.01", LIFNR: "0000000001", AUGBL: "0100000013"})
			e.ACDOCA[len(e.ACDOCA)-2].WSL = "0.00"
		}, []string{"2000/2026/0100000011/000002: line of SAP document 0100000011 has a document amount (WSL \"-0.01\", RWCUR \"EUR\")"}, nil},
		{"a line that carries no document amount", func(e *sap.Extract, _ *target) { mixed(e, ""); e.ACDOCA[len(e.ACDOCA)-3].RWCUR = "" }, nil, nil},
		{"a line of a document amount of zero", func(e *sap.Extract, _ *target) { mixed(e, "0.00") }, nil, nil},
		{"a line of a down payment's document in company currency", func(e *sap.Extract, _ *target) { dpLineAlone(e) },
			[]string{dpAlone}, []string{dpAlone}},
		{"a document an earlier run posted", func(e *sap.Extract, t *target) {
			mixed(e, "0.01")
			t.posted = map[string]string{"2000/2026/0100000011": "EUR"}
		}, nil, nil},
		{"a document refused as a conflict", func(e *sap.Extract, t *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001", AUGBL: "0100000013"})
			withDocument(e, "0100000013", "EUR", sap.ACDOCA{RACCT: "0000100000", HSL: "-50.00", WSL: "-50.00"})
			pay := &e.ACDOCA[len(e.ACDOCA)-2]
			pay.RACCT, pay.LIFNR, pay.AUGBL = "0000211000", "0000000001", "0100000013"
			e.ACDOCA = append(e.ACDOCA, sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000013", DOCLN: "000003",
				RACCT: "0000170000", HSL: "0.00", WSL: "0.01", BUDAT: "20260310", BLART: "KZ", RWCUR: "USD"})
			t.posted = map[string]string{"2000/2026/0100000011": "USD"}
		}, []string{"2000/2026/0100000013: SAP document 2000/2026/0100000013 cannot load in a currency its clearing can be taken over in"}, nil},
	}
	for _, balances := range []bool{false, true} {
		var run []documentCurrencyCase
		for _, c := range cases {
			want, mode := c.documents, "documents"
			if balances {
				want, mode = c.balances, "balances"
			}
			run = append(run, documentCurrencyCase{mode + ": " + c.name, func(e *sap.Extract, t *target) { t.balances = balances; c.change(e, t) }, want})
		}
		runDocumentCurrencyCases(t, run)
	}
}
