package sap

import (
	"strings"
	"testing"
)

// TestCurrencyDecimals: TCURX gives each currency it lists its decimals,
// padding trimmed; a row whose CURRDEC is no single digit is refused, and a
// currency listed with two different decimals is refused once and left
// out.
func TestCurrencyDecimals(t *testing.T) {
	got, problems := CurrencyDecimals([]TCURX{{CURRKEY: "JPY  ", CURRDEC: " 0"}, {CURRKEY: "JPY", CURRDEC: "0"}, {CURRKEY: "KWD", CURRDEC: "3"},
		{CURRKEY: "KWD", CURRDEC: "2"}, {CURRKEY: "KWD", CURRDEC: "1"}, {CURRKEY: "A", CURRDEC: "x"}, {CURRKEY: "B", CURRDEC: "-1"}, {CURRKEY: "C", CURRDEC: "10"},
		{CURRKEY: "D", CURRDEC: ""}, {CURRKEY: "E", CURRDEC: "+3"}})
	if len(got) != 1 || got["JPY"] != 0 {
		t.Errorf("decimals: got %v, want only JPY 0", got)
	}
	var keys []string
	for _, p := range problems {
		keys = append(keys, p.CURRKEY)
	}
	if got, want := strings.Join(keys, " "), "KWD A B C D E"; got != want {
		t.Errorf("refused rows: got %s, want %s", got, want)
	}
}

// TestFromInternal: an amount in internal form has two decimals, shifted
// to the currency's TCURX decimals; a currency TCURX does not list keeps
// two, and says so.
func TestFromInternal(t *testing.T) {
	decimals := map[string]int{"JPY": 0, "KWD": 3}
	for name, c := range map[string]struct {
		s, cur, want string
		listed       bool
		wantErr      bool
	}{
		"yen":            {"10.00", "JPY", "1000", true, false},
		"negative yen":   {"-1000.00", "JPY", "-100000", true, false},
		"dinar":          {"12.34", "KWD", "1.234", true, false},
		"dollar":         {"100.00", "CAD", "100.00", false, false},
		"three decimals": {"10.005", "JPY", "", true, true},
		"not a number":   {"x", "CAD", "", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, listed, err := FromInternal(c.s, c.cur, decimals)
			if (err != nil) != c.wantErr {
				t.Fatalf("FromInternal(%q, %s): got error %v, want one: %v", c.s, c.cur, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("FromInternal(%q, %s): got %q, want %q", c.s, c.cur, got, c.want)
			}
			if listed != c.listed {
				t.Errorf("FromInternal(%q, %s) listed: got %v, want %v", c.s, c.cur, listed, c.listed)
			}
		})
	}
}
