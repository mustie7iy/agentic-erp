package integration

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// withDocument appends a vendor document of company 2000 (KZ, so dpTarget's
// number range takes it) of two lines: 170000 debited and the given line
// credited, both in the given document currency.
func withDocument(e *sap.Extract, belnr, currency string, credit sap.ACDOCA) {
	debit := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: belnr, DOCLN: "000001", RACCT: "0000170000",
		HSL: strings.TrimPrefix(credit.HSL, "-"), BUDAT: "20260310", BLART: "KZ", RWCUR: currency}
	credit.RLDNR, credit.RBUKRS, credit.GJAHR, credit.BELNR, credit.DOCLN, credit.BUDAT, credit.BLART, credit.RWCUR =
		"0L", "2000", "2026", belnr, "000002", "20260310", "KZ", currency
	e.ACDOCA = append(e.ACDOCA, debit, credit)
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

// TestDocumentCurrencyQuality: the quality gate refuses an open item whose
// document currency (ACDOCA RWCUR) is not its company's, a down payment,
// an open vendor item or a line of an open GR/IR position, naming the
// document, and refuses one without a document currency rather than assume
// the company's (review:batch20); items in the company's currency, lines
// SAP cleared, lines that are no item and lines of no amount are not
// refused.
func TestDocumentCurrencyQuality(t *testing.T) {
	cases := []struct {
		name   string
		change func(e *sap.Extract, t *target)
		want   []string // the document currency errors, in order
	}{
		{"in the company's currency", func(e *sap.Extract, t *target) {
			withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
			withGRIRPosition(e, t, "EUR")
		}, nil},
		{"a down payment in a foreign currency", func(e *sap.Extract, _ *target) { e.ACDOCA[0].RWCUR = "USD" },
			[]string{"2000/2026/0100000009/000001: open item of SAP document 0100000009 is in document currency USD, not company 2000's currency EUR"}},
		{"a down payment without a document currency", func(e *sap.Extract, _ *target) { e.ACDOCA[0].RWCUR = "" },
			[]string{"2000/2026/0100000009/000001: open item of SAP document 0100000009 has no document currency (RWCUR)"}},
		{"an open vendor item in a foreign currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
		}, []string{"2000/2026/0100000011/000002: open item of SAP document 0100000011 is in document currency USD"}},
		{"an open vendor item without a document currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
		}, []string{"2000/2026/0100000011/000002: open item of SAP document 0100000011 has no document currency (RWCUR)"}},
		{"a cleared vendor item in a foreign currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "USD", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001", AUGBL: "0100000013"})
		}, nil},
		{"a vendor line of no amount without a document currency", func(e *sap.Extract, _ *target) {
			withDocument(e, "0100000011", "", sap.ACDOCA{RACCT: "0000211000", HSL: "0.00", LIFNR: "0000000001"})
		}, nil},
		{"an open GR/IR position in a foreign currency", func(e *sap.Extract, t *target) { withGRIRPosition(e, t, "USD") },
			[]string{"2000/2026/0100000012/000002: open item of SAP document 0100000012 is in document currency USD"}},
		{"an open GR/IR position without a document currency", func(e *sap.Extract, t *target) { withGRIRPosition(e, t, "") },
			[]string{"2000/2026/0100000012/000002: open item of SAP document 0100000012 has no document currency (RWCUR)"}},
		{"the GR/IR lines of a closed position in a foreign currency", func(e *sap.Extract, t *target) {
			withGRIRPosition(e, t, "USD")
			e.MSEG = nil // nothing received, and the receipt's GR/IR line reversed: no balance and no open quantity
			reversal := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000014", BUDAT: "20260310", BLART: "KZ", RWCUR: "USD"}
			grir, offset := reversal, reversal
			grir.DOCLN, grir.RACCT, grir.HSL, grir.EBELN, grir.EBELP = "000001", "0000219000", "100.00", "4500000001", "00010"
			offset.DOCLN, offset.RACCT, offset.HSL = "000002", "0000170000", "-100.00"
			e.ACDOCA = append(e.ACDOCA, grir, offset)
		}, nil},
		{"a line on an ordinary account in a foreign currency", func(e *sap.Extract, _ *target) { e.ACDOCA[2].RWCUR = "USD" }, nil},
		{"an amount that does not parse", func(e *sap.Extract, _ *target) { e.ACDOCA[0].RWCUR, e.ACDOCA[0].HSL = "USD", "x" }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			c.change(e, &tg)
			var got []string
			for _, i := range checkQuality(e, tg) {
				if i.Severity == SevError && strings.Contains(i.Message, "document currency") {
					got = append(got, i.Key+": "+i.Message)
				}
			}
			if len(got) != len(c.want) {
				t.Fatalf("document currency errors: got %d %q, want %d %q", len(got), got, len(c.want), c.want)
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
// that expect none test the document currency check alone.
func TestDocumentCurrencyQualityPasses(t *testing.T) {
	e, tg := dpExtract(), dpTarget()
	withDocument(e, "0100000011", "EUR", sap.ACDOCA{RACCT: "0000211000", HSL: "-50.00", LIFNR: "0000000001"})
	withGRIRPosition(e, &tg, "EUR")
	for _, i := range checkQuality(e, tg) {
		if i.Severity == SevError {
			t.Errorf("errors: got %s %s: %s, want none", i.Table, i.Key, i.Message)
		}
	}
}
