package cabank

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name, institution, transit, number string
		want                               Account
		err                                error
	}{
		{"plain", "003", "12345", "1234567", Account{"003", "12345", "1234567"}, nil},
		{"written with spaces and dashes", "0 03", "12-345", "12 3456-7890 12", Account{"003", "12345", "123456789012"}, nil},
		{"short institution", "03", "12345", "1234567", Account{}, ErrInstitution},
		{"letter in institution", "00A", "12345", "1234567", Account{}, ErrInstitution},
		{"long transit", "003", "123456", "1234567", Account{}, ErrTransit},
		{"short account", "003", "12345", "123456", Account{}, ErrAccount},
		{"long account", "003", "12345", "1234567890123", Account{}, ErrAccount},
		{"IBAN as account", "003", "12345", "DE89370400440532013000", Account{}, ErrAccount},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.institution, c.transit, c.number)
			if !errors.Is(err, c.err) {
				t.Fatalf("error: got %v, want %v", err, c.err)
			}
			if got != c.want {
				t.Errorf("account: got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestStoredForms(t *testing.T) {
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"routing", ValidRouting("003", "12345"), true},
		{"routing with a short transit", ValidRouting("003", "1234"), false},
		{"routing with a short institution", ValidRouting("03", "12345"), false},
		{"number", ValidNumber("1234567"), true},
		{"number with a dash", ValidNumber("123-4567"), false},
		{"member id", ValidMemberID(MemberID("003", "12345")), true},
		{"member id without its zero", ValidMemberID("00312345"), false},
		{"member id with a letter", ValidMemberID("0003A2345"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
	if got, want := MemberID("003", "12345"), "000312345"; got != want {
		t.Errorf("member id: got %q, want %q", got, want)
	}
}
