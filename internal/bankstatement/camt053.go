package bankstatement

import (
	"bytes"
	"encoding/xml"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// camtNamespaces are the camt.053 versions read (ADR-0047).
var camtNamespaces = map[string]string{
	"urn:iso:std:iso:20022:tech:xsd:camt.053.001.02": Camt053V02,
	"urn:iso:std:iso:20022:tech:xsd:camt.053.001.08": Camt053V08,
}

// The camt.053 elements read. One set of types reads both versions: where
// .001.08 moved an element (a party's name under Pty, the BIC as BICFI,
// the status as a code, a transaction's amount on the transaction), both
// places are read.
type (
	camtDocument struct {
		XMLName xml.Name   `xml:"Document"`
		MsgID   string     `xml:"BkToCstmrStmt>GrpHdr>MsgId"`
		Stmts   []camtStmt `xml:"BkToCstmrStmt>Stmt"`
	}
	camtStmt struct {
		ID         string        `xml:"Id"`
		ElctrncSeq string        `xml:"ElctrncSeqNb"`
		LglSeq     string        `xml:"LglSeqNb"`
		IBAN       string        `xml:"Acct>Id>IBAN"`
		Other      string        `xml:"Acct>Id>Othr>Id"`
		Ccy        string        `xml:"Acct>Ccy"`
		BIC        string        `xml:"Acct>Svcr>FinInstnId>BIC"`
		BICFI      string        `xml:"Acct>Svcr>FinInstnId>BICFI"`
		Bals       []camtBalance `xml:"Bal"`
		Ntries     []camtEntry   `xml:"Ntry"`
	}
	camtAmount struct {
		Value string `xml:",chardata"`
		Ccy   string `xml:"Ccy,attr"`
	}
	camtDate struct {
		Dt   string `xml:"Dt"`
		DtTm string `xml:"DtTm"`
	}
	camtBalance struct {
		Code string     `xml:"Tp>CdOrPrtry>Cd"`
		Amt  camtAmount `xml:"Amt"`
		Ind  string     `xml:"CdtDbtInd"`
		Dt   camtDate   `xml:"Dt"`
	}
	camtTxCode struct {
		Domain    string `xml:"Domn>Cd"`
		Family    string `xml:"Domn>Fmly>Cd"`
		SubFamily string `xml:"Domn>Fmly>SubFmlyCd"`
		Prtry     string `xml:"Prtry>Cd"`
	}
	camtEntry struct {
		Ref     string     `xml:"NtryRef"`
		Amt     camtAmount `xml:"Amt"`
		Ind     string     `xml:"CdtDbtInd"`
		Rvsl    bool       `xml:"RvslInd"`
		Sts     camtStatus `xml:"Sts"`
		BookgDt camtDate   `xml:"BookgDt"`
		ValDt   camtDate   `xml:"ValDt"`
		SvcrRef string     `xml:"AcctSvcrRef"`
		TxCd    camtTxCode `xml:"BkTxCd"`
		Info    string     `xml:"AddtlNtryInf"`
		Batches []camtBtch `xml:"NtryDtls>Btch"`
		Txs     []camtTx   `xml:"NtryDtls>TxDtls"`
	}
	// camtStatus is an entry's status: the text of .001.02, the code of
	// .001.08.
	camtStatus struct {
		Text string `xml:",chardata"`
		Cd   string `xml:"Cd"`
	}
	camtBtch struct {
		MsgID    string `xml:"MsgId"`
		PmtInfID string `xml:"PmtInfId"`
	}
	camtTx struct {
		MsgID      string     `xml:"Refs>MsgId"`
		PmtInfID   string     `xml:"Refs>PmtInfId"`
		InstrID    string     `xml:"Refs>InstrId"`
		EndToEndID string     `xml:"Refs>EndToEndId"`
		Amt        camtAmount `xml:"Amt"`
		TxAmt      camtAmount `xml:"AmtDtls>TxAmt>Amt"`
		Ind        string     `xml:"CdtDbtInd"`
		TxCd       camtTxCode `xml:"BkTxCd"`
		Dbtr       camtParty  `xml:"RltdPties>Dbtr"`
		DbtrAcct   camtAcct   `xml:"RltdPties>DbtrAcct"`
		Cdtr       camtParty  `xml:"RltdPties>Cdtr"`
		CdtrAcct   camtAcct   `xml:"RltdPties>CdtrAcct"`
		DbtrAgt    camtAgent  `xml:"RltdAgts>DbtrAgt"`
		CdtrAgt    camtAgent  `xml:"RltdAgts>CdtrAgt"`
		Ustrd      []string   `xml:"RmtInf>Ustrd"`
		StrdRef    []string   `xml:"RmtInf>Strd>CdtrRefInf>Ref"`
	}
	camtParty struct {
		Nm    string `xml:"Nm"`
		PtyNm string `xml:"Pty>Nm"`
	}
	camtAcct struct {
		IBAN  string `xml:"Id>IBAN"`
		Other string `xml:"Id>Othr>Id"`
	}
	camtAgent struct {
		BIC   string `xml:"FinInstnId>BIC"`
		BICFI string `xml:"FinInstnId>BICFI"`
	}
)

// ParseCamt053 reads the statements of a camt.053 document, .001.02 or
// .001.08, in the currencies of exps. Only booked entries are read: a
// statement's balances are booked balances.
func ParseCamt053(b []byte, exps currency.Exponents) ([]Statement, error) {
	var doc camtDocument
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil {
		return nil, malformed("camt.053: %v", err)
	}
	format, ok := camtNamespaces[doc.XMLName.Space]
	if !ok {
		return nil, malformed("camt.053: namespace %q is not camt.053.001.02 or .001.08", doc.XMLName.Space)
	}
	if len(doc.Stmts) == 0 {
		return nil, malformed("camt.053: no statement")
	}
	out := make([]Statement, len(doc.Stmts))
	for i, st := range doc.Stmts {
		s, err := camtStatement(st, exps)
		if err != nil {
			return nil, err
		}
		s.Format, s.MessageID = format, strings.TrimSpace(doc.MsgID)
		out[i] = s
	}
	return out, nil
}

// camtStatement reads one statement.
func camtStatement(st camtStmt, exps currency.Exponents) (Statement, error) {
	s := Statement{ID: strings.TrimSpace(st.ID), Sequence: strings.TrimSpace(firstOf(st.ElctrncSeq, st.LglSeq)),
		Account: Account{IBAN: strings.TrimSpace(st.IBAN), Number: strings.TrimSpace(st.Other), BIC: firstOf(st.BIC, st.BICFI)}}
	var opening, closing bool
	for _, b := range st.Bals {
		bal, err := camtBalanceOf(b, exps)
		if err != nil {
			return Statement{}, err
		}
		switch b.Code {
		case "OPBD", "PRCD": // opening booked, or the previous statement's closing: the first given
			if !opening {
				s.Opening, s.Currency, opening = bal, b.Amt.Ccy, true
			}
		case "CLBD":
			s.Closing, closing = bal, true
		}
	}
	if !opening || !closing {
		return Statement{}, malformed("camt.053 statement %q: no opening (OPBD, PRCD) or closing (CLBD) booked balance", s.ID)
	}
	if st.Ccy != "" && st.Ccy != s.Currency || camtCurrencies(st.Bals) != s.Currency {
		return Statement{}, malformed("camt.053 statement %q: balances in more than one currency", s.ID)
	}
	for _, n := range st.Ntries {
		if firstOf(strings.TrimSpace(n.Sts.Cd), strings.TrimSpace(n.Sts.Text)) != "BOOK" {
			continue
		}
		e, err := camtEntryOf(n, s.Currency, exps)
		if err != nil {
			return Statement{}, err
		}
		s.Entries = append(s.Entries, e)
	}
	return s, s.check()
}

// camtCurrencies is the currency of the opening and closing balances, or
// "" when they differ.
func camtCurrencies(bals []camtBalance) string {
	ccy := ""
	for _, b := range bals {
		if b.Code != "OPBD" && b.Code != "PRCD" && b.Code != "CLBD" {
			continue
		}
		if ccy != "" && b.Amt.Ccy != ccy {
			return ""
		}
		ccy = b.Amt.Ccy
	}
	return ccy
}

// camtBalanceOf reads a balance.
func camtBalanceOf(b camtBalance, exps currency.Exponents) (Balance, error) {
	amt, err := camtSigned(b.Amt, b.Ind, exps)
	if err != nil {
		return Balance{}, err
	}
	d, err := date(firstOf(b.Dt.Dt, b.Dt.DtTm))
	if err != nil {
		return Balance{}, err
	}
	return Balance{Amount: amt, Date: d}, nil
}

// camtSigned reads an amount with its credit or debit indicator.
func camtSigned(a camtAmount, ind string, exps currency.Exponents) (money.Minor, error) {
	if ind != "CRDT" && ind != "DBIT" {
		return 0, malformed("credit or debit indicator %q", ind)
	}
	return amount(strings.TrimSpace(a.Value), a.Ccy, exps, ind == "DBIT")
}

// camtEntryOf reads a booked entry in the statement's currency.
func camtEntryOf(n camtEntry, ccy string, exps currency.Exponents) (Entry, error) {
	if n.Amt.Ccy != ccy {
		return Entry{}, malformed("camt.053 entry %q: amount in %s, the statement is in %s", n.SvcrRef, n.Amt.Ccy, ccy)
	}
	amt, err := camtSigned(n.Amt, n.Ind, exps)
	if err != nil {
		return Entry{}, err
	}
	booked, err := date(firstOf(n.BookgDt.Dt, n.BookgDt.DtTm))
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Amount: amt, Reversal: n.Rvsl, BookingDate: booked, ValueDate: booked, TransactionCode: n.TxCd.code(),
		BankReference: strings.TrimSpace(n.SvcrRef), OwnerReference: strings.TrimSpace(n.Ref), Text: strings.TrimSpace(n.Info)}
	if v := firstOf(n.ValDt.Dt, n.ValDt.DtTm); v != "" {
		if e.ValueDate, err = date(v); err != nil {
			return Entry{}, err
		}
	}
	for _, tx := range n.Txs {
		d, err := camtDetail(tx, n.Ind, exps)
		if err != nil {
			return Entry{}, err
		}
		d.TransactionCode = firstOf(d.TransactionCode, e.TransactionCode)
		e.Details = append(e.Details, d)
	}
	// A batch booking without transaction details still names its batch.
	if len(n.Txs) == 0 {
		for _, bt := range n.Batches {
			e.Details = append(e.Details, Detail{MessageID: strings.TrimSpace(bt.MsgID), PaymentInfoID: strings.TrimSpace(bt.PmtInfID)})
		}
	}
	// The one transaction or batch of an entry is the entry's amount where
	// the bank does not repeat it.
	if len(e.Details) == 1 && e.Details[0].Amount == 0 {
		e.Details[0].Amount = e.Amount
	}
	return e, nil
}

// camtDetail reads a transaction of an entry; ind is the entry's credit or
// debit indicator, which .001.02 does not repeat on the transaction.
func camtDetail(tx camtTx, ind string, exps currency.Exponents) (Detail, error) {
	d := Detail{MessageID: strings.TrimSpace(tx.MsgID), PaymentInfoID: strings.TrimSpace(tx.PmtInfID),
		InstructionID: strings.TrimSpace(tx.InstrID), EndToEndID: strings.TrimSpace(tx.EndToEndID), TransactionCode: tx.TxCd.code(),
		Remittance: strings.TrimSpace(strings.Join(append(tx.Ustrd, tx.StrdRef...), " "))}
	if d.EndToEndID == "NOTPROVIDED" {
		d.EndToEndID = ""
	}
	party, acct, agent := tx.Dbtr, tx.DbtrAcct, tx.DbtrAgt
	if ind == "DBIT" {
		party, acct, agent = tx.Cdtr, tx.CdtrAcct, tx.CdtrAgt
	}
	d.Counterparty = Party{Name: strings.TrimSpace(firstOf(party.Nm, party.PtyNm)), IBAN: strings.TrimSpace(acct.IBAN),
		Account: strings.TrimSpace(acct.Other), BIC: firstOf(agent.BIC, agent.BICFI)}
	a := tx.Amt
	if a.Value == "" {
		a = tx.TxAmt
	}
	if a.Value == "" {
		return d, nil
	}
	amt, err := camtSigned(a, firstOf(tx.Ind, ind), exps)
	if err != nil {
		return Detail{}, err
	}
	d.Amount = amt
	return d, nil
}

// code is the bank transaction code: the ISO domain, family and
// sub-family, else the bank's proprietary code.
func (c camtTxCode) code() string {
	if c.Domain != "" && c.Family != "" && c.SubFamily != "" {
		return c.Domain + "-" + c.Family + "-" + c.SubFamily
	}
	return strings.TrimSpace(c.Prtry)
}

// firstOf is the first of the values that is not empty.
func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
