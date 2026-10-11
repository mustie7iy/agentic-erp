package bankfile

import (
	"encoding/xml"
	"testing"
	"time"
)

// Whatever text ends up in names and remittance lines, the file stays
// well-formed XML and amounts are untouched.
func FuzzPain001Text(f *testing.F) {
	f.Add("Initech", "INV-1", "E2E-1")
	f.Add("<script>&\"'", "]]>", "\x00\xff")
	f.Fuzz(func(t *testing.T, name, remittance, e2e string) {
		b := Batch{MessageID: "M1", InitiatorName: name, DebtorName: name, DebtorIBAN: "GB33BUKB20201555555555", DebtorBIC: "BUKBGB22", Decimals: 2,
			Currency: "EUR", ExecutionDate: "2026-10-09", Created: time.Unix(0, 0),
			Payments: []Payment{{EndToEndID: e2e, CreditorName: name + "x", CreditorAccount: "DE89370400440532013000", Amount: 4242, Remittance: remittance}}}
		out, err := Pain001(b)
		if err != nil {
			return
		}
		var doc struct {
			Sum string `xml:"CstmrCdtTrfInitn>GrpHdr>CtrlSum"`
		}
		if err := xml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("not well-formed for name %q: %v", name, err)
		}
		if doc.Sum != "42.42" {
			t.Fatalf("control sum changed: %s", doc.Sum)
		}
	})
}

func FuzzValidIBAN(f *testing.F) {
	f.Add("GB33BUKB20201555555555")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) { ValidIBAN(s) })
}
