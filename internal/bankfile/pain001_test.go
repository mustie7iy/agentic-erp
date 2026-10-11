package bankfile

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func batch() Batch {
	return Batch{
		MessageID: "PAY-00000042", InitiatorName: "Acme Corp US", DebtorName: "Acme Corp US",
		DebtorIBAN: "GB33 BUKB 2020 1555 5555 55", DebtorBIC: "BUKBGB22", Decimals: 2, Currency: "USD", ExecutionDate: "2026-10-09",
		Created: time.Date(2026, 10, 9, 9, 30, 0, 0, time.UTC),
		Payments: []Payment{
			{EndToEndID: "E2E-1", CreditorName: "Initech & Sons <Ltd>", CreditorAccount: "DE89370400440532013000", Amount: 123456, Remittance: "INV-1 INV-2"},
			{EndToEndID: "E2E-2", CreditorName: "Umbrella Logistics", CreditorAccount: "400000007777", Amount: 5},
		},
	}
}

func TestPain001(t *testing.T) {
	out, err := Pain001(batch())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "<?xml") || !strings.Contains(s, `xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.001.03"`) {
		t.Fatal("header/namespace")
	}
	var doc struct {
		Hdr struct {
			N   int    `xml:"NbOfTxs"`
			Sum string `xml:"CtrlSum"`
			At  string `xml:"CreDtTm"`
		} `xml:"CstmrCdtTrfInitn>GrpHdr"`
		Pmt struct {
			Debtor string `xml:"DbtrAcct>Id>IBAN"`
			Date   string `xml:"ReqdExctnDt"`
			Txs    []struct {
				Name  string `xml:"Cdtr>Nm"`
				IBAN  string `xml:"CdtrAcct>Id>IBAN"`
				Other string `xml:"CdtrAcct>Id>Othr>Id"`
				Amt   struct {
					Ccy   string `xml:"Ccy,attr"`
					Value string `xml:",chardata"`
				} `xml:"Amt>InstdAmt"`
			} `xml:"CdtTrfTxInf"`
		} `xml:"CstmrCdtTrfInitn>PmtInf"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not valid XML: %v", err)
	}
	if doc.Hdr.N != 2 || doc.Hdr.Sum != "1234.61" || doc.Hdr.At != "2026-10-09T09:30:00" {
		t.Fatalf("group header: %+v", doc.Hdr)
	}
	if doc.Pmt.Debtor != "GB33BUKB20201555555555" || doc.Pmt.Date != "2026-10-09" {
		t.Fatal("debtor account and date")
	}
	tx := doc.Pmt.Txs
	if tx[0].Name != "Initech & Sons <Ltd>" || tx[0].IBAN != "DE89370400440532013000" || tx[0].Amt.Value != "1234.56" || tx[0].Amt.Ccy != "USD" {
		t.Fatalf("IBAN creditor (and XML escaping): %+v", tx[0])
	}
	if tx[1].Other != "400000007777" || tx[1].IBAN != "" || tx[1].Amt.Value != "0.05" {
		t.Fatalf("domestic account creditor: %+v", tx[1])
	}
}

func TestPain001Validation(t *testing.T) {
	cases := map[string]func(b *Batch){
		"message id":  func(b *Batch) { b.MessageID = "" },
		"debtor iban": func(b *Batch) { b.DebtorIBAN = "GB00BUKB20201555555555" },
		"currency":    func(b *Batch) { b.Currency = "US" },
		"no payments": func(b *Batch) { b.Payments = nil },
		"date":        func(b *Batch) { b.ExecutionDate = "9 Oct" },
		"amount":      func(b *Batch) { b.Payments[0].Amount = 0 },
		"creditor":    func(b *Batch) { b.Payments[1].CreditorName = " " },
	}
	for name, mutate := range cases {
		b := batch()
		mutate(&b)
		if _, err := Pain001(b); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestFieldLengthsAndIBAN(t *testing.T) {
	b := batch()
	b.Payments[0].CreditorName = strings.Repeat("Ä", 100)
	b.Payments[0].EndToEndID = strings.Repeat("9", 50)
	out, err := Pain001(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), strings.Repeat("Ä", 71)) || strings.Contains(string(out), strings.Repeat("9", 36)) {
		t.Fatal("fields must be clipped to schema limits (by characters, not bytes)")
	}
	for iban, ok := range map[string]bool{
		"GB33BUKB20201555555555": true, "gb33bukb20201555555555": true, "GB33BUKB2020155555555$": false,
		"1B33BUKB20201555555555": false, "GB33": false, "DE89370400440532013000": true,
	} {
		if ValidIBAN(iban) != ok {
			t.Errorf("ValidIBAN(%q) != %v", iban, ok)
		}
	}
}
