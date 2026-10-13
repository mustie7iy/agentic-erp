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
	if !strings.HasPrefix(s, "<?xml") {
		t.Errorf("header: got %.40q, want an <?xml declaration first", s)
	}
	if !strings.Contains(s, `xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.001.03"`) {
		t.Error("namespace: got none, want the pain.001.001.03 namespace")
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
	if got := doc.Hdr.N; got != 2 {
		t.Errorf("group header NbOfTxs: got %d, want %d", got, 2)
	}
	if got := doc.Hdr.Sum; got != "1234.61" {
		t.Errorf("group header CtrlSum: got %q, want %q", got, "1234.61")
	}
	if got := doc.Hdr.At; got != "2026-10-09T09:30:00" {
		t.Errorf("group header CreDtTm: got %q, want %q", got, "2026-10-09T09:30:00")
	}
	if got := doc.Pmt.Debtor; got != "GB33BUKB20201555555555" {
		t.Errorf("debtor IBAN: got %q, want %q", got, "GB33BUKB20201555555555")
	}
	if got := doc.Pmt.Date; got != "2026-10-09" {
		t.Errorf("execution date: got %q, want %q", got, "2026-10-09")
	}
	tx := doc.Pmt.Txs
	if len(tx) < 2 {
		t.Fatalf("transactions: got %d, want at least 2", len(tx))
	}
	if got := tx[0].Name; got != "Initech & Sons <Ltd>" {
		t.Errorf("IBAN creditor name (XML escaping): got %q, want %q", got, "Initech & Sons <Ltd>")
	}
	if got := tx[0].IBAN; got != "DE89370400440532013000" {
		t.Errorf("IBAN creditor IBAN: got %q, want %q", got, "DE89370400440532013000")
	}
	if got := tx[0].Amt.Value; got != "1234.56" {
		t.Errorf("IBAN creditor amount: got %q, want %q", got, "1234.56")
	}
	if got := tx[0].Amt.Ccy; got != "USD" {
		t.Errorf("IBAN creditor currency: got %q, want %q", got, "USD")
	}
	if got := tx[1].Other; got != "400000007777" {
		t.Errorf("domestic account creditor account: got %q, want %q", got, "400000007777")
	}
	if got := tx[1].IBAN; got != "" {
		t.Errorf("domestic account creditor IBAN: got %q, want %q", got, "")
	}
	if got := tx[1].Amt.Value; got != "0.05" {
		t.Errorf("domestic account creditor amount: got %q, want %q", got, "0.05")
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
		t.Run(name, func(t *testing.T) {
			b := batch()
			mutate(&b)
			if _, err := Pain001(b); err == nil {
				t.Error("Pain001: got nil error, want a validation error")
			}
		})
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
	// Fields are clipped to schema limits by characters, not bytes.
	if strings.Contains(string(out), strings.Repeat("Ä", 71)) {
		t.Error("creditor name: got more than 70 characters, want it clipped to 70")
	}
	if strings.Contains(string(out), strings.Repeat("9", 36)) {
		t.Error("end-to-end id: got more than 35 characters, want it clipped to 35")
	}
	for iban, ok := range map[string]bool{
		"GB33BUKB20201555555555": true, "gb33bukb20201555555555": true, "GB33BUKB2020155555555$": false,
		"1B33BUKB20201555555555": false, "GB33": false, "DE89370400440532013000": true,
	} {
		t.Run(iban, func(t *testing.T) {
			if got := ValidIBAN(iban); got != ok {
				t.Errorf("ValidIBAN(%q) = %v, want %v", iban, got, ok)
			}
		})
	}
}
