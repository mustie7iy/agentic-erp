package bankfile

import (
	"encoding/xml"
	"strings"
	"testing"
)

// canadianBatch pays a Canadian supplier from a Canadian house bank (T45d).
func canadianBatch() Batch {
	b := batch()
	b.DebtorIBAN, b.DebtorBIC, b.DebtorClearingID, b.DebtorAccount = "", "ROYCCAT2", "000300002", "1000001"
	b.Currency = "CAD"
	b.Payments = []Payment{{EndToEndID: "E2E-CA", CreditorName: "Maple Supply Ltd", CreditorAccount: "123456789012",
		CreditorClearingID: "000412345", Amount: 250000, Remittance: "INV-CA-1"}}
	return b
}

func TestPain001Canadian(t *testing.T) {
	out, err := Pain001(canadianBatch())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Pmt struct {
			DebtorIBAN  string `xml:"DbtrAcct>Id>IBAN"`
			DebtorOther string `xml:"DbtrAcct>Id>Othr>Id"`
			DebtorBIC   string `xml:"DbtrAgt>FinInstnId>BIC"`
			DebtorSys   string `xml:"DbtrAgt>FinInstnId>ClrSysMmbId>ClrSysId>Cd"`
			DebtorMmb   string `xml:"DbtrAgt>FinInstnId>ClrSysMmbId>MmbId"`
			Txs         []struct {
				Sys   string `xml:"CdtrAgt>FinInstnId>ClrSysMmbId>ClrSysId>Cd"`
				Mmb   string `xml:"CdtrAgt>FinInstnId>ClrSysMmbId>MmbId"`
				BIC   string `xml:"CdtrAgt>FinInstnId>BIC"`
				IBAN  string `xml:"CdtrAcct>Id>IBAN"`
				Other string `xml:"CdtrAcct>Id>Othr>Id"`
			} `xml:"CdtTrfTxInf"`
		} `xml:"CstmrCdtTrfInitn>PmtInf"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not valid XML: %v", err)
	}
	checks := []struct{ name, got, want string }{
		{"debtor IBAN", doc.Pmt.DebtorIBAN, ""},
		{"debtor account", doc.Pmt.DebtorOther, "1000001"},
		{"debtor BIC", doc.Pmt.DebtorBIC, "ROYCCAT2"},
		{"debtor clearing system", doc.Pmt.DebtorSys, "CACPA"},
		{"debtor member id", doc.Pmt.DebtorMmb, "000300002"},
	}
	if len(doc.Pmt.Txs) != 1 {
		t.Fatalf("transactions: got %d, want 1", len(doc.Pmt.Txs))
	}
	tx := doc.Pmt.Txs[0]
	checks = append(checks, []struct{ name, got, want string }{
		{"creditor clearing system", tx.Sys, "CACPA"},
		{"creditor member id", tx.Mmb, "000412345"},
		{"creditor BIC", tx.BIC, ""},
		{"creditor IBAN", tx.IBAN, ""},
		{"creditor account", tx.Other, "123456789012"},
	}...)
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
	// pain.001.001.03 orders CdtrAgt after Amt and before Cdtr.
	s := string(out)
	if a, g, c := strings.Index(s, "<Amt>"), strings.Index(s, "<CdtrAgt>"), strings.Index(s, "<Cdtr>"); a >= g || g >= c {
		t.Errorf("element order: got Amt at %d, CdtrAgt at %d, Cdtr at %d, want Amt < CdtrAgt < Cdtr", a, g, c)
	}
}

func TestPain001IBANHasNoCreditorAgent(t *testing.T) {
	out, err := Pain001(batch())
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "CdtrAgt") || strings.Contains(s, "ClrSysMmbId") {
		t.Error("IBAN file: got a creditor agent or clearing member, want the file as before T45d")
	}
}

func TestPain001CanadianValidation(t *testing.T) {
	cases := map[string]func(b *Batch){
		"debtor member id":           func(b *Batch) { b.DebtorClearingID = "00030000" },
		"debtor account":             func(b *Batch) { b.DebtorAccount = "123" },
		"debtor IBAN beside it":      func(b *Batch) { b.DebtorIBAN = "GB33BUKB20201555555555" },
		"creditor member id":         func(b *Batch) { b.Payments[0].CreditorClearingID = "A00412345" },
		"creditor account":           func(b *Batch) { b.Payments[0].CreditorAccount = "DE89370400440532013000" },
		"creditor account too short": func(b *Batch) { b.Payments[0].CreditorAccount = "123456" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := canadianBatch()
			mutate(&b)
			if _, err := Pain001(b); err == nil {
				t.Error("Pain001: got nil error, want a validation error")
			}
		})
	}
}

// TestPain001DebtorCurrency: a batch from a foreign-currency account names
// the account's currency after its id, an IBAN's or a Canadian account's;
// one without leaves it out, so files to local-currency accounts are as
// before (ADR-0051 §3, T53a).
func TestPain001DebtorCurrency(t *testing.T) {
	for name, c := range map[string]struct {
		batch      Batch
		currency   string
		debtorTag  string
		currencyIn bool
	}{
		"IBAN, local":       {batch(), "", "<IBAN>GB33BUKB20201555555555</IBAN>", false},
		"IBAN, foreign":     {func() Batch { b := batch(); b.DebtorCurrency = "USD"; return b }(), "USD", "<IBAN>GB33BUKB20201555555555</IBAN>", true},
		"Canadian, foreign": {func() Batch { b := canadianBatch(); b.DebtorCurrency = "USD"; return b }(), "USD", "<Id>1000001</Id>", true},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Pain001(c.batch)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Ccy string `xml:"CstmrCdtTrfInitn>PmtInf>DbtrAcct>Ccy"`
			}
			if err := xml.Unmarshal(out, &doc); err != nil {
				t.Fatalf("not valid XML: %v", err)
			}
			if doc.Ccy != c.currency {
				t.Errorf("debtor account currency: got %q, want %q", doc.Ccy, c.currency)
			}
			if got := strings.Contains(string(out), "<Ccy>"); got != c.currencyIn {
				t.Errorf("a Ccy element in the file: got %v, want %v", got, c.currencyIn)
			}
			if !strings.Contains(string(out), c.debtorTag) {
				t.Errorf("file: got\n%s\nwant the debtor account %s", out, c.debtorTag)
			}
		})
	}
}
