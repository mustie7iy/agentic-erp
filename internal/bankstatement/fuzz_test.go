package bankstatement

import (
	"testing"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// holds checks what every statement a parser accepts must satisfy: an
// identity, and entries that take the opening balance to the closing,
// with text that is valid UTF-8.
func holds(t *testing.T, stmts []Statement) {
	t.Helper()
	for _, s := range stmts {
		if s.ID == "" || s.Account.Identifier() == "" || s.Currency == "" {
			t.Fatalf("statement without identity or currency: %+v", s)
		}
		amounts := []money.Minor{s.Opening.Amount}
		for _, e := range s.Entries {
			amounts = append(amounts, e.Amount)
			for _, d := range e.Details {
				if !utf8.ValidString(d.Counterparty.Name + d.EndToEndID + d.Remittance) {
					t.Fatalf("detail text not UTF-8: %+v", d)
				}
			}
			if !utf8.ValidString(e.Text + e.TransactionCode + e.BankReference) {
				t.Fatalf("entry text not UTF-8: %+v", e)
			}
		}
		if sum, err := money.Sum(amounts...); err != nil || sum != s.Closing.Amount {
			t.Fatalf("statement %s accepted unbalanced: %d (%v), closing %d", s.ID, sum, err, s.Closing.Amount)
		}
	}
}

// Whatever a bank or an attacker uploads, the camt.053 parser never
// panics, and what it accepts balances.
func FuzzParseCamt053(f *testing.F) {
	f.Add(sample(f, "camt053_ca_v02.xml"))
	f.Add(sample(f, "camt053_ae_v08.xml"))
	f.Add([]byte(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"><BkToCstmrStmt><Stmt/></BkToCstmrStmt></Document>`))
	exps := currency.Seeded()
	f.Fuzz(func(t *testing.T, b []byte) {
		stmts, err := ParseCamt053(b, exps)
		if err != nil {
			return
		}
		holds(t, stmts)
	})
}

// Whatever a bank or an attacker uploads, the MT940 parser never panics,
// and what it accepts balances.
func FuzzParseMT940(f *testing.F) {
	f.Add(sample(f, "mt940_ae.txt"))
	f.Add(sample(f, "mt940_ca.txt"))
	f.Add([]byte(":20:X\n:25:1\n:28C:1\n:60F:C261231EUR1,\n:61:2612311231RD1,NTRF//\n:86:/EREF//NAME/\xff/\n:62F:C261231EUR2,\n"))
	exps := currency.Seeded()
	f.Fuzz(func(t *testing.T, b []byte) {
		stmts, err := ParseMT940(b, exps)
		if err != nil {
			return
		}
		holds(t, stmts)
	})
}
