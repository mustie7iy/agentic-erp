// Package bankfile writes payment instructions for banks: ISO 20022
// pain.001.001.03 customer credit transfers, the bank file format of the
// payments context (03-data-model §3.10).
package bankfile

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Payment is one credit transfer.
type Payment struct {
	EndToEndID      string
	CreditorName    string
	CreditorAccount string // IBAN, or a domestic account number
	Amount          money.Minor
	Remittance      string
}

// Batch is one payment instruction file from one debtor account.
type Batch struct {
	MessageID     string
	InitiatorName string
	DebtorName    string
	DebtorIBAN    string
	DebtorBIC     string
	Currency      string
	Decimals      int    // minor-unit digits of Currency
	ExecutionDate string // YYYY-MM-DD
	Created       time.Time
	Payments      []Payment
}

// ValidIBAN checks an IBAN with the ISO 13616 mod-97 algorithm.
func ValidIBAN(iban string) bool {
	s := strings.ToUpper(strings.ReplaceAll(iban, " ", ""))
	if len(s) < 15 || len(s) > 34 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return false
	}
	var digits strings.Builder
	for _, r := range s[4:] + s[:4] {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			fmt.Fprintf(&digits, "%d", r-'A'+10)
		default:
			return false
		}
	}
	n, _ := new(big.Int).SetString(digits.String(), 10) // only digits were written, at least 15 (the length check above)
	return new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// Pain001 renders the batch as XML after validating it.
func Pain001(b Batch) ([]byte, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	amounts := make([]money.Minor, len(b.Payments))
	for i, p := range b.Payments {
		amounts[i] = p.Amount
	}
	// The control sum is what the bank checks the file against: a wrapped
	// sum would be a wrong file, so it is refused instead.
	total, err := money.Sum(amounts...)
	if err != nil {
		return nil, fmt.Errorf("bankfile: control sum of %d payments: %w", len(amounts), err)
	}
	txs := make([]cdtTrfTxInf, 0, len(b.Payments))
	for _, p := range b.Payments {
		acct := account{}
		if ValidIBAN(p.CreditorAccount) {
			acct.IBAN = strings.ReplaceAll(p.CreditorAccount, " ", "")
		} else {
			acct.Othr = &other{ID: p.CreditorAccount}
		}
		txs = append(txs, cdtTrfTxInf{
			PmtID:    pmtID{EndToEndID: clip(p.EndToEndID, 35)},
			Amt:      amt{InstdAmt: instdAmt{Ccy: b.Currency, Value: p.Amount.Plain(b.Decimals)}},
			Cdtr:     party{Nm: clip(p.CreditorName, 70)},
			CdtrAcct: cashAccount{ID: acct},
			RmtInf:   &rmtInf{Ustrd: clip(p.Remittance, 140)},
		})
	}
	doc := document{
		Xmlns: "urn:iso:std:iso:20022:tech:xsd:pain.001.001.03",
		CstmrCdtTrfInitn: cstmrCdtTrfInitn{
			GrpHdr: grpHdr{
				MsgID: clip(b.MessageID, 35), CreDtTm: b.Created.UTC().Format("2006-01-02T15:04:05"),
				NbOfTxs: len(txs), CtrlSum: total.Plain(b.Decimals), InitgPty: party{Nm: clip(b.InitiatorName, 70)},
			},
			PmtInf: pmtInf{
				PmtInfID: clip(b.MessageID+"-1", 35), PmtMtd: "TRF", NbOfTxs: len(txs), CtrlSum: total.Plain(b.Decimals),
				ReqdExctnDt: b.ExecutionDate, Dbtr: party{Nm: clip(b.DebtorName, 70)},
				DbtrAcct: cashAccount{ID: account{IBAN: strings.ReplaceAll(b.DebtorIBAN, " ", "")}},
				DbtrAgt:  agent{FinInstnID: finInstnID{BIC: b.DebtorBIC}},
				Txs:      txs,
			},
		},
	}
	out, _ := xml.MarshalIndent(doc, "", "  ") // fixed structs of strings and ints always encode
	return append([]byte(xml.Header), out...), nil
}

func (b Batch) validate() error {
	switch {
	case b.MessageID == "":
		return errors.New("message id is required")
	case !ValidIBAN(b.DebtorIBAN):
		return fmt.Errorf("debtor IBAN %q is invalid", b.DebtorIBAN)
	case len(b.Currency) != 3:
		return fmt.Errorf("currency %q is invalid", b.Currency)
	case len(b.Payments) == 0:
		return errors.New("batch has no payments")
	}
	if _, err := time.Parse(time.DateOnly, b.ExecutionDate); err != nil {
		return fmt.Errorf("execution date: %w", err)
	}
	for i, p := range b.Payments {
		if p.Amount <= 0 {
			return fmt.Errorf("payment %d: amount must be positive", i+1)
		}
		if strings.TrimSpace(p.CreditorName) == "" || strings.TrimSpace(p.CreditorAccount) == "" {
			return fmt.Errorf("payment %d: creditor name and account are required", i+1)
		}
	}
	return nil
}

// clip shortens s to n characters, the field limits of the schema.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

type document struct {
	XMLName          xml.Name         `xml:"Document"`
	Xmlns            string           `xml:"xmlns,attr"`
	CstmrCdtTrfInitn cstmrCdtTrfInitn `xml:"CstmrCdtTrfInitn"`
}

type cstmrCdtTrfInitn struct {
	GrpHdr grpHdr `xml:"GrpHdr"`
	PmtInf pmtInf `xml:"PmtInf"`
}

type grpHdr struct {
	MsgID    string `xml:"MsgId"`
	CreDtTm  string `xml:"CreDtTm"`
	NbOfTxs  int    `xml:"NbOfTxs"`
	CtrlSum  string `xml:"CtrlSum"`
	InitgPty party  `xml:"InitgPty"`
}

type pmtInf struct {
	PmtInfID    string        `xml:"PmtInfId"`
	PmtMtd      string        `xml:"PmtMtd"`
	NbOfTxs     int           `xml:"NbOfTxs"`
	CtrlSum     string        `xml:"CtrlSum"`
	ReqdExctnDt string        `xml:"ReqdExctnDt"`
	Dbtr        party         `xml:"Dbtr"`
	DbtrAcct    cashAccount   `xml:"DbtrAcct"`
	DbtrAgt     agent         `xml:"DbtrAgt"`
	Txs         []cdtTrfTxInf `xml:"CdtTrfTxInf"`
}

type party struct {
	Nm string `xml:"Nm"`
}

type cashAccount struct {
	ID account `xml:"Id"`
}

type account struct {
	IBAN string `xml:"IBAN,omitempty"`
	Othr *other `xml:"Othr,omitempty"`
}

type other struct {
	ID string `xml:"Id"`
}

type agent struct {
	FinInstnID finInstnID `xml:"FinInstnId"`
}

type finInstnID struct {
	BIC string `xml:"BIC"`
}

type cdtTrfTxInf struct {
	PmtID    pmtID       `xml:"PmtId"`
	Amt      amt         `xml:"Amt"`
	Cdtr     party       `xml:"Cdtr"`
	CdtrAcct cashAccount `xml:"CdtrAcct"`
	RmtInf   *rmtInf     `xml:"RmtInf,omitempty"`
}

type pmtID struct {
	EndToEndID string `xml:"EndToEndId"`
}

type amt struct {
	InstdAmt instdAmt `xml:"InstdAmt"`
}

type instdAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type rmtInf struct {
	Ustrd string `xml:"Ustrd"`
}
