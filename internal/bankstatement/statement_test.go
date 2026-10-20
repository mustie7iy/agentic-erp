package bankstatement

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

func sample(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// day is a date the tests write; a mistyped one is the zero time, which
// no expectation matches.
func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // see above: a bad literal fails the comparison
	return d
}

// summary is what the tests compare of a statement.
type summary struct {
	format, key, ccy  string
	opening, closing  money.Minor
	entries           int
	openDay, closeDay time.Time
}

func summarise(s Statement) summary {
	return summary{s.Format, s.Key(), s.Currency, s.Opening.Amount, s.Closing.Amount, len(s.Entries), s.Opening.Date, s.Closing.Date}
}

// TestSamples reads the samples modelled on Canadian and UAE banks' files
// into the same model.
func TestSamples(t *testing.T) {
	exps := currency.Seeded()
	for _, c := range []struct {
		file  string
		parse func([]byte, currency.Exponents) ([]Statement, error)
		want  []summary
	}{
		{"camt053_ca_v02.xml", ParseCamt053, []summary{{Camt053V02, "20261020-1234567|00021-1234567|293", "CAD", 5000000, 5325044, 4, day("2026-10-19"), day("2026-10-20")}}},
		{"camt053_ae_v08.xml", ParseCamt053, []summary{{Camt053V08, "AE-STMT-20261020|AE070331234567890123456|201", "AED", 12000000, 9319675, 4, day("2026-10-19"), day("2026-10-20")}}},
		{"mt940_ae.txt", ParseMT940, []summary{{MT940, "STMT20261020|AE070331234567890123456|00293/001", "AED", 12000000, 9319675, 4, day("2026-10-19"), day("2026-10-20")}}},
		{"mt940_ca.txt", ParseMT940, []summary{
			{MT940, "CA2026123101|00021/1234567|00012/1", "CAD", 1000000, 905000, 2, day("2026-12-30"), day("2026-12-31")},
			{MT940, "CA2027010201|00021/1234567|00013/1", "CAD", 905000, 904005, 1, day("2026-12-31"), day("2027-01-02")},
		}},
	} {
		t.Run(c.file, func(t *testing.T) {
			got, err := c.parse(sample(t, c.file), exps)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("statements: got %d, want %d", len(got), len(c.want))
			}
			for i, w := range c.want {
				if g := summarise(got[i]); g != w {
					t.Errorf("statement %d: got %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

// TestCamtEntries: amounts signed, transaction codes ISO or proprietary,
// references, counterparties of either version, batches, reversals, and
// pending entries left out.
func TestCamtEntries(t *testing.T) {
	ca, err := ParseCamt053(sample(t, "camt053_ca_v02.xml"), currency.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	ae, err := ParseCamt053(sample(t, "camt053_ae_v08.xml"), currency.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	c, a := ca[0].Entries, ae[0].Entries
	checks := []struct {
		name      string
		got, want any
	}{
		{"CA message id", ca[0].MessageID, "CA-STMT-20261020-0001"},
		{"CA bank", ca[0].Account.BIC, "ROYCCAT2"},
		{"CA payment amount", c[0].Amount, money.Minor(-123456)},
		{"CA payment code", c[0].TransactionCode, "PMNT-ICDT-DMCT"},
		{"CA payment end-to-end id", c[0].Details[0].EndToEndID, "E2E-1"},
		{"CA payment creditor", c[0].Details[0].Counterparty, Party{Name: "Northwind Supplies Ltd", Account: "00456-7654321", BIC: "BOFMCAM2"}},
		{"CA payment code on the detail", c[0].Details[0].TransactionCode, "PMNT-ICDT-DMCT"},
		{"CA batch details", len(c[1].Details), 2},
		{"CA batch payment information", c[1].Details[1].PaymentInfoID, "PAY-00000042-T"},
		{"CA batch transaction amount", c[1].Details[1].Amount, money.Minor(-200000)},
		{"CA charge BAI code", c[2].TransactionCode, "398"},
		{"CA charge text", c[2].Text, "SERVICE CHARGE"},
		{"CA charge value date from booking", c[2].ValueDate, day("2026-10-20")},
		{"CA receipt booked on its local day", c[3].BookingDate, day("2026-10-20")},
		{"CA receipt debtor", c[3].Details[0].Counterparty.Name, "Contoso Retail Inc"},
		{"CA receipt without end-to-end id", c[3].Details[0].EndToEndID, ""},
		{"CA receipt amount from the entry", c[3].Details[0].Amount, money.Minor(750000)},
		{"CA receipt remittance", c[3].Details[0].Remittance, "INV 2026-118"},
		{"AE BICFI", ae[0].Account.BIC, "EBILAEAD"},
		{"AE payment references", [4]string{a[0].Details[0].MessageID, a[0].Details[0].PaymentInfoID, a[0].Details[0].InstructionID, a[0].Details[0].EndToEndID},
			[4]string{"PAY-00000043", "PAY-00000043-T", "I-7", "E2E-AE-7"}},
		{"AE creditor under Pty", a[0].Details[0].Counterparty, Party{Name: "Gulf Logistics LLC", IBAN: "AE460260001015333439101", BIC: "NBADAEAA"}},
		{"AE structured reference", a[0].Details[0].Remittance, "INV 7781"},
		{"AE charges code", a[1].TransactionCode, "ACMT-MDOP-CHRG"},
		{"AE receipt value date", a[2].ValueDate, day("2026-10-21")},
		{"AE receipt debtor IBAN", a[2].Details[0].Counterparty.IBAN, "AE210030000123456789012"},
		{"AE receipt in USD read in AED", a[2].Details[0].Amount, money.Minor(1800000)},
		{"AE reversal", a[3].Reversal, true},
		{"AE reversal batch", a[3].Details[0].PaymentInfoID, "PAY-00000040-T"},
		{"AE reversal batch amount", a[3].Details[0].Amount, money.Minor(50000)},
	}
	for _, ch := range checks {
		t.Run(ch.name, func(t *testing.T) {
			if ch.got != ch.want {
				t.Errorf("got %v, want %v", ch.got, ch.want)
			}
		})
	}
}

// TestCamtFirstOpening: of an opening booked balance and the previous
// statement's closing, the first given is the opening.
func TestCamtFirstOpening(t *testing.T) {
	ae := string(sample(t, "camt053_ae_v08.xml"))
	extra := `<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="AED">1.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-10-20</Dt></Dt></Bal>`
	s, err := ParseCamt053([]byte(strings.Replace(ae, "<Bal>", extra+"<Bal>", 1)), currency.Seeded())
	if err == nil {
		t.Fatalf("read the later balance: opening %d", s[0].Opening.Amount)
	}
	s, err = ParseCamt053([]byte(strings.Replace(ae, "<Ntry>", extra+"<Ntry>", 1)), currency.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	if s[0].Opening.Amount != 12000000 {
		t.Errorf("opening: got %d, want 12000000", s[0].Opening.Amount)
	}
}

// TestMT940Entries: :61: marks, funds codes, entry dates across a new
// year, references and structured :86: subfields.
func TestMT940Entries(t *testing.T) {
	ae, err := ParseMT940(sample(t, "mt940_ae.txt"), currency.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ParseMT940(sample(t, "mt940_ca.txt"), currency.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	a, c := ae[0].Entries, ca[0].Entries
	checks := []struct {
		name      string
		got, want any
	}{
		{"AE IBAN", ae[0].Account, Account{IBAN: "AE070331234567890123456"}},
		{"AE payment", [3]any{a[0].Amount, a[0].TransactionCode, a[0].BankReference}, [3]any{money.Minor(-4525075), "NTRF", "EB26102000123"}},
		{"AE owner reference", a[0].OwnerReference, "E2E-AE-7"},
		{"AE supplementary details kept", strings.HasPrefix(a[0].Text, "OUTWARD TT"), true},
		{"AE :86: across lines", a[0].Details[0].Counterparty, Party{Name: "Gulf Logistics LLC", IBAN: "AE460260001015333439101"}},
		{"AE :86: references", [3]string{a[0].Details[0].EndToEndID, a[0].Details[0].PaymentInfoID, a[0].Details[0].Remittance},
			[3]string{"E2E-AE-7", "PAY-00000043-T", "INV 7781"}},
		{"AE charges", [3]any{a[1].Amount, a[1].TransactionCode, a[1].OwnerReference}, [3]any{money.Minor(-5250), "NCHG", ""}},
		{"AE free text has no detail", len(a[1].Details), 0},
		{"AE unstructured remittance", a[2].Details[0].Remittance, "INV 2026-201"},
		{"AE reversal of a debit credits", [2]any{a[3].Amount, a[3].Reversal}, [2]any{money.Minor(50000), true}},
		{"CA account number", ca[0].Account, Account{Number: "00021/1234567"}},
		{"CA funds code", c[0].Amount, money.Minor(25000)},
		{"CA reference with slashes", [2]string{c[0].OwnerReference, c[0].BankReference}, [2]string{"INV/2026/1", "BK0001"}},
		{"CA booked in the old year", [2]time.Time{c[1].BookingDate, c[1].ValueDate}, [2]time.Time{day("2026-12-31"), day("2027-01-02")}},
	}
	for _, ch := range checks {
		t.Run(ch.name, func(t *testing.T) {
			if ch.got != ch.want {
				t.Errorf("got %v, want %v", ch.got, ch.want)
			}
		})
	}
}

// TestMT940Details: account forms of :25:, reversal of a credit, an entry
// date in the next year, and :86: subfields.
func TestMT940Details(t *testing.T) {
	stmt := func(account, line, info string) string {
		return ":20:S1\n:25:" + account + "\n:28C:1\n:60F:C261231EUR100,00\n:61:" + line + "\n:86:" + info + "\n:62F:C270101EUR90,00\n"
	}
	t.Run("BIC and IBAN", func(t *testing.T) {
		s, err := ParseMT940([]byte(stmt("DEUTDEFF/DE89370400440532013000", "2701010101RC10,00NTRFNONREF", "x")), currency.Seeded())
		if err != nil {
			t.Fatal(err)
		}
		if want := (Account{IBAN: "DE89370400440532013000", BIC: "DEUTDEFF"}); s[0].Account != want {
			t.Errorf("account: got %+v, want %+v", s[0].Account, want)
		}
		if e := s[0].Entries[0]; e.Amount != -1000 || !e.Reversal {
			t.Errorf("reversal of a credit: got %d reversal %v, want -1000 reversal true", e.Amount, e.Reversal)
		}
	})
	t.Run("entry date in the next year", func(t *testing.T) {
		s, err := ParseMT940([]byte(stmt("DE89370400440532013000", "2612310101D10,00NTRFNOTPROVIDED", "/EREF/NOTPROVIDED/BENM//NAME/A/B/")), currency.Seeded())
		if err != nil {
			t.Fatal(err)
		}
		e := s[0].Entries[0]
		if e.BookingDate != day("2027-01-01") {
			t.Errorf("booking date: got %v, want 2027-01-01", e.BookingDate)
		}
		if d := e.Details[0]; d.EndToEndID != "" || d.Counterparty.Name != "A/B" {
			t.Errorf("detail: got %+v, want no end-to-end id and name A/B", d)
		}
	})
}

// TestMT940LongField: a field of very many lines is read in linear time;
// joining them one by one would take minutes for an uploaded file.
func TestMT940LongField(t *testing.T) {
	info := strings.Repeat("x\n", 200000)
	s, err := ParseMT940([]byte(":20:S1\n:25:1\n:28C:1\n:60F:C261231EUR1,00\n:61:261231D1,00NTRF\n:86:"+info+":62F:C261231EUR0,00\n"), currency.Seeded())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s[0].Entries[0].Text); got != 2*200000-1 {
		t.Errorf("text length: got %d, want %d", got, 2*200000-1)
	}
}

// TestMalformed: each file is refused with ErrMalformed.
func TestMalformed(t *testing.T) {
	ca, ae := string(sample(t, "camt053_ca_v02.xml")), string(sample(t, "camt053_ae_v08.xml"))
	mt := string(sample(t, "mt940_ca.txt"))
	camt := func(s string) func() error {
		return func() error { _, err := ParseCamt053([]byte(s), currency.Seeded()); return err }
	}
	mt940 := func(s string) func() error {
		return func() error { _, err := ParseMT940([]byte(s), currency.Seeded()); return err }
	}
	cases := map[string]func() error{
		"comma in a camt amount":      camt(strings.Replace(ca, ">15.00<", ">15,00<", 1)),
		"references too long":         mt940(strings.Replace(mt, "NMSCE2E-CA-9//BK0002", "NMSCE2E-CA-9-0123456789//BK0002", 1)),
		"second opening":              mt940(strings.Replace(mt, ":60F:C261230CAD10000,00\n", ":60F:C261230CAD10000,00\n:60M:C261230CAD10000,00\n", 1)),
		"closing before the opening":  mt940(":20:X\n:25:1\n:28C:1\n:62F:C270102USD9040,05\n:60F:C261231CAD9050,00\n"),
		"not XML":                     camt("<Document"),
		"another version":             camt(strings.Replace(ca, `xsd:camt.053.001.02"`, `xsd:camt.053.001.04"`, 1)),
		"no statement":                camt(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt/></Document>`),
		"no closing balance":          camt(strings.Replace(ca, "<Cd>CLBD</Cd>", "<Cd>XXXX</Cd>", 1)),
		"balances in two currencies":  camt(strings.Replace(ae, `<Amt Ccy="AED">93196.75`, `<Amt Ccy="USD">93196.75`, 1)),
		"account in another currency": camt(strings.Replace(ae, "<Ccy>AED</Ccy>", "<Ccy>USD</Ccy>", 1)),
		"balance indicator":           camt(strings.Replace(ca, "<CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-10-19", "<CdtDbtInd>X</CdtDbtInd><Dt><Dt>2026-10-19", 1)),
		"balance date":                camt(strings.Replace(ca, "<Dt>2026-10-19</Dt>", "<Dt>2026-13-19</Dt>", 1)),
		"unknown currency":            camt(strings.ReplaceAll(ca, `Ccy="CAD"`, `Ccy="ZZZ"`)),
		"entry in another currency":   camt(strings.Replace(ca, `<Amt Ccy="CAD">15.00</Amt>`, `<Amt Ccy="USD">15.00</Amt>`, 1)),
		"entry amount":                camt(strings.Replace(ca, ">15.00<", ">-15.00<", 1)),
		"entry booking date":          camt(strings.Replace(ca, "<BookgDt><Dt>2026-10-20</Dt></BookgDt>\n        <AcctSvcrRef>RB2610200003", "<BookgDt/>\n        <AcctSvcrRef>RB2610200003", 1)),
		"entry value date":            camt(strings.Replace(ae, "<ValDt><Dt>2026-10-21</Dt>", "<ValDt><Dt>21.10.2026</Dt>", 1)),
		"transaction amount":          camt(strings.Replace(ca, ">1000.00<", ">1000.001<", 1)),
		"unbalanced":                  camt(strings.Replace(ca, ">53250.44<", ">53250.45<", 1)),
		"entries out of range":        camt(strings.Replace(strings.Replace(ca, ">7500.00<", ">92233720368547758.07<", 1), ">50000.00<", ">92233720368547758.07<", 1)),
		"no id":                       camt(strings.Replace(ca, "<Id>20261020-1234567</Id>", "", 1)),
		"text before a field":         mt940("hello\n" + mt),
		"no :20: first":               mt940(strings.Replace(mt, ":20:CA2026123101\n", "", 1)),
		"no statement in MT940":       mt940("{1:F01}\n-}\n"),
		"no :28C:":                    mt940(strings.Replace(mt, ":28C:00012/1\n", "", 1)),
		"balance":                     mt940(strings.Replace(mt, ":60F:C261230CAD10000,00", ":60F:C261230CAD10000.00", 1)),
		"balance date in MT940":       mt940(strings.Replace(mt, ":60F:C261230", ":60F:C261330", 1)),
		"balance currency":            mt940(strings.Replace(mt, ":60F:C261230CAD", ":60F:C261230ZZZ", 1)),
		"closing currency":            mt940(strings.Replace(mt, ":62F:C261231CAD", ":62F:C261231USD", 1)),
		"line":                        mt940(strings.Replace(mt, "D1200,00NMSC", "D1200.00NMSC", 1)),
		"line before the opening":     mt940(":20:X\n:25:1\n:28C:1\n:61:270102D9,95NCHGNONREF\n:60F:C261231CAD9050,00\n:62F:C270102CAD9040,05\n"),
		"line value date":             mt940(strings.Replace(mt, ":61:270102D9,95", ":61:271302D9,95", 1)),
		"line entry date":             mt940(strings.Replace(mt, ":61:2612311231CD", ":61:2612311341CD", 1)),
		"line amount":                 mt940(strings.Replace(mt, "D9,95NCHG", "D9,955NCHG", 1)),
		"unbalanced MT940":            mt940(strings.Replace(mt, ":62F:C270102CAD9040,05", ":62F:C270102CAD9040,06", 1)),
	}
	for name, parse := range cases {
		t.Run(name, func(t *testing.T) {
			if err := parse(); !errors.Is(err, ErrMalformed) {
				t.Errorf("got %v, want ErrMalformed", err)
			}
		})
	}
}
