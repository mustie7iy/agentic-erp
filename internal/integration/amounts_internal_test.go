package integration

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestAmountConversion: an amount in external form is read with its
// currency's ISO digits; one in SAP's internal form with two decimals,
// shifted by 2 - TCURX-CURRDEC, a currency TCURX does not list having two.
func TestAmountConversion(t *testing.T) {
	exponents := map[string]int{"JPY": 0, "KWD": 3, "CAD": 2, "AED": 2, "HUF": 2}
	external := target{exponents: exponents}
	internal := target{exponents: exponents, internal: true, decimals: map[string]int{"JPY": 0, "KWD": 3, "AED": 3, "HUF": 0}}
	unlisted := target{exponents: exponents, internal: true, decimals: map[string]int{"JPY": 0}}
	for name, c := range map[string]struct {
		t       target
		s, cur  string
		want    money.Minor
		wantErr string
	}{
		"external yen":                 {external, "100000", "JPY", 100000, ""},
		"external dinar":               {external, "1.234", "KWD", 1234, ""},
		"external dollar":              {external, "-100.00", "CAD", -10000, ""},
		"external yen with decimals":   {external, "100000.5", "JPY", 0, "more than 0 significant decimals"},
		"internal yen":                 {internal, "10.00", "JPY", 1000, ""},
		"internal negative yen":        {internal, "-1000.00", "JPY", -100000, ""},
		"internal dinar":               {internal, "12.34", "KWD", 1234, ""},
		"internal dinar, one decimal":  {internal, "12.3", "KWD", 1230, ""},
		"internal dollar, not listed":  {internal, "100.00", "CAD", 10000, ""},
		"internal forint, 0 in TCURX":  {internal, "10.00", "HUF", 100000, ""},
		"internal with three decimals": {internal, "10.005", "JPY", 0, "internal form of two decimals"},
		"internal finer than ISO":      {internal, "12.34", "AED", 0, "is 1.234 AED (TCURX: 3 decimals)"},
		"internal not listed, not two": {unlisted, "12.34", "KWD", 0, "TCURX does not give the decimals of KWD, which has 3"},
		"internal not a number":        {internal, "x", "CAD", 0, "internal form of two decimals"},
		"unknown currency":             {internal, "1.00", "XXX", 0, `currency "XXX" is not known`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := c.t.amount(c.s, c.cur)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("amount(%q, %s): got error %v, want %d", c.s, c.cur, err, c.want)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("amount(%q, %s): got error %v, want one saying %q", c.s, c.cur, err, c.wantErr)
			case got != c.want:
				t.Errorf("amount(%q, %s): got %d, want %d", c.s, c.cur, got, c.want)
			}
		})
	}
}

// TestCurrencyDecimals: each TCURX row sap.CurrencyDecimals cannot use is
// an error of table TCURX keyed by its currency.
func TestCurrencyDecimals(t *testing.T) {
	got, issues := currencyDecimals([]sap.TCURX{{CURRKEY: "JPY", CURRDEC: "0"}, {CURRKEY: "OMR", CURRDEC: "x"}})
	if len(got) != 1 || got["JPY"] != 0 {
		t.Errorf("decimals: got %v, want only JPY 0", got)
	}
	if len(issues) != 1 || issues[0] != (Issue{SevError, "TCURX", "OMR", `CURRDEC "x" of currency OMR is not a number of decimals from 0 to 9`}) {
		t.Errorf("issues: got %v, want OMR's refused", issues)
	}
}

// TestAmountsInternal: a file extract's amounts are in internal form unless
// its connector says external; OData's are external.
func TestAmountsInternal(t *testing.T) {
	for name, c := range map[string]struct {
		c    Connector
		want bool
	}{
		"file":          {Connector{Kind: SAPFile, Settings: map[string]any{"path": "x.json"}}, true},
		"file internal": {Connector{Kind: SAPFile, Settings: map[string]any{"path": "x.json", "amount_format": "internal"}}, true},
		"file external": {Connector{Kind: SAPFile, Settings: map[string]any{"path": "x.json", "amount_format": "external"}}, false},
		"OData":         {Connector{Kind: SAPOData, Settings: map[string]any{"base_url": "https://x"}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := amountsInternal(c.c); got != c.want {
				t.Errorf("amountsInternal: got %v, want %v", got, c.want)
			}
		})
	}
}
