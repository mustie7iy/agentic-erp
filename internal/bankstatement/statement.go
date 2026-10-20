// Package bankstatement reads bank account statements, ISO 20022
// camt.053 (.001.02 and .001.08) and SWIFT MT940, into one model for the
// payments context's reconciliation (ADR-0047). It is pure: no database
// and no I/O. Amounts are minor units of the statement's currency
// (ADR-0005), signed: a credit to the account is positive, a debit
// negative. A statement is read only whole: its entries must take its
// opening balance to its closing balance, in its one currency.
package bankstatement

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// Formats a statement is read from; the camt.053 version is the one the
// document declares.
const (
	Camt053V02 = "camt.053.001.02"
	Camt053V08 = "camt.053.001.08"
	MT940      = "mt940"
)

// ErrMalformed is wrapped by every refusal of a file's content.
var ErrMalformed = errors.New("bankstatement: malformed statement")

// malformed is a refusal of the content, saying what is wrong.
func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

// Statement is one account statement.
type Statement struct {
	Format    string
	MessageID string // camt GrpHdr/MsgId; empty in MT940
	ID        string // camt Stmt/Id; MT940 :20:
	// Sequence is the statement's number in the account's series: camt
	// ElctrncSeqNb, else LglSeqNb; MT940 :28C:.
	Sequence string
	Account  Account
	Currency string
	Opening  Balance
	Closing  Balance
	Entries  []Entry
}

// Key identifies the statement for the account (ADR-0047): another file
// with the same key is the same statement.
func (s Statement) Key() string {
	return s.ID + "|" + s.Account.Identifier() + "|" + s.Sequence
}

// Account is the statement's bank account: an IBAN, or where the country
// has none (Canada) the bank's own account number.
type Account struct {
	IBAN   string
	Number string
	BIC    string
}

// Identifier is the account's IBAN, or its number when it has none.
func (a Account) Identifier() string {
	if a.IBAN != "" {
		return a.IBAN
	}
	return a.Number
}

// Balance is a booked balance on a day.
type Balance struct {
	Amount money.Minor
	Date   time.Time
}

// Entry is one booking on the account.
type Entry struct {
	Amount      money.Minor // signed: credit positive
	Reversal    bool        // the booking reverses an earlier one
	BookingDate time.Time
	ValueDate   time.Time
	// TransactionCode is the bank transaction code: ISO 20022 domain,
	// family and sub-family (PMNT-ICDT-ESCT), the bank's proprietary code
	// (a BAI code such as 398), or the MT940 type (NTRF, NCHG).
	TransactionCode string
	BankReference   string // camt AcctSvcrRef; MT940 the bank's reference
	OwnerReference  string // camt NtryRef; MT940 the reference for the account owner
	Text            string // camt AddtlNtryInf; MT940 :86:
	// Details are the transactions the booking holds: one for a single
	// payment, several for a batch booking, none where the bank gives none.
	Details []Detail
}

// Detail is one transaction within an entry.
type Detail struct {
	Amount          money.Minor // signed as the entry; 0 when the bank gives none
	MessageID       string
	PaymentInfoID   string
	InstructionID   string
	EndToEndID      string
	TransactionCode string
	Counterparty    Party
	Remittance      string
}

// Party is the other side of a transaction: the creditor of a debit, the
// debtor of a credit.
type Party struct {
	Name    string
	IBAN    string
	Account string
	BIC     string
}

// check holds the statement to its balances: a known currency, an
// identity, and entries that take the opening balance to the closing.
func (s *Statement) check() error {
	if s.ID == "" || s.Account.Identifier() == "" {
		return malformed("statement without an id or an account")
	}
	amounts := make([]money.Minor, 0, len(s.Entries)+1)
	amounts = append(amounts, s.Opening.Amount)
	for _, e := range s.Entries {
		amounts = append(amounts, e.Amount)
	}
	sum, err := money.Sum(amounts...)
	if err != nil {
		return malformed("statement %s: entries out of range", s.ID)
	}
	if sum != s.Closing.Amount {
		return malformed("statement %s: opening balance and entries make %d, closing balance is %d", s.ID, sum, s.Closing.Amount)
	}
	return nil
}

var (
	isoAmount   = regexp.MustCompile(`^[0-9]{1,18}(\.[0-9]{1,18})?$`)
	swiftAmount = regexp.MustCompile(`^[0-9]{1,15},[0-9]{0,15}$`)
)

// amount reads an unsigned amount of the currency, with "." (camt) or ","
// (MT940) before its decimals, and signs it.
func amount(s, ccy string, exps currency.Exponents, debit bool) (money.Minor, error) {
	digits, ok := exps[ccy]
	if !ok {
		return 0, malformed("currency %q is not in the currency reference data", ccy)
	}
	switch {
	case isoAmount.MatchString(s):
	case swiftAmount.MatchString(s):
		s = strings.Replace(s, ",", ".", 1)
	default:
		return 0, malformed("amount %q is not a plain decimal", s)
	}
	m, err := money.ParseDecimal(s, digits)
	if err != nil {
		return 0, malformed("amount %s %s: %v", s, ccy, err)
	}
	if debit {
		m = -m
	}
	return m, nil
}

// date reads an ISO date, or the date of an ISO date and time as the bank
// wrote it (its local day).
func date(s string) (time.Time, error) {
	if len(s) > 10 && s[10] == 'T' {
		s = s[:10]
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, malformed("date %q", s)
	}
	return d, nil
}
