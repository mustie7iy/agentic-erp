// Package cabank checks Canadian bank account identifiers (ADR-0038, T45d):
// an institution number of 3 digits, a transit (branch) number of 5 and an
// account number of 7 to 12. Canada has no IBAN; partner accounts, house
// banks and bank files carry these instead. Only the format is checked: an
// institution unknown to the Canadian Payments Association is the bank's
// to refuse, as an unknown BIC is.
package cabank

import (
	"errors"
	"regexp"
	"strings"
)

// The parts Parse refuses, so that callers can name the field.
var (
	ErrInstitution = errors.New("institution number must be 3 digits")
	ErrTransit     = errors.New("transit number must be 5 digits")
	ErrAccount     = errors.New("account number must be 7-12 digits")
)

var (
	institution = regexp.MustCompile(`^[0-9]{3}$`)
	transit     = regexp.MustCompile(`^[0-9]{5}$`)
	account     = regexp.MustCompile(`^[0-9]{7,12}$`)
	memberID    = regexp.MustCompile(`^0[0-9]{8}$`)
)

// Account is a Canadian bank account, digits only.
type Account struct {
	Institution, Transit, Number string
}

// Parse checks the three numbers, after dropping the spaces and dashes
// people write them with.
func Parse(institutionNumber, transitNumber, number string) (Account, error) {
	a := Account{Institution: Digits(institutionNumber), Transit: Digits(transitNumber), Number: Digits(number)}
	switch {
	case !institution.MatchString(a.Institution):
		return Account{}, ErrInstitution
	case !transit.MatchString(a.Transit):
		return Account{}, ErrTransit
	case !account.MatchString(a.Number):
		return Account{}, ErrAccount
	}
	return a, nil
}

// ValidRouting reports whether the institution and transit numbers, as
// stored, are a bank's routing.
func ValidRouting(institutionNumber, transitNumber string) bool {
	return institution.MatchString(institutionNumber) && transit.MatchString(transitNumber)
}

// ValidNumber reports whether s, as stored, is an account number.
func ValidNumber(s string) bool { return account.MatchString(s) }

// MemberID is the bank's clearing system member id in the Canadian
// Payments Association's electronic routing form, 0 followed by the
// institution and transit numbers, which pain.001 names under the
// clearing system CACPA.
func MemberID(institutionNumber, transitNumber string) string {
	return "0" + institutionNumber + transitNumber
}

// ValidMemberID reports whether s is a MemberID.
func ValidMemberID(s string) bool { return memberID.MatchString(s) }

// Digits is s without the spaces and dashes people write the numbers
// with: the form they are stored in.
func Digits(s string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(s)
}
