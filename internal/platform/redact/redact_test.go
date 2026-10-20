package redact_test

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/redact"
)

// TestText: IBANs that pass mod 97 and card numbers that pass Luhn are
// replaced, however they are written; tax identifiers only of the given
// countries; names, e-mail addresses and numbers that only look alike
// pass, and the count is of the replacements.
func TestText(t *testing.T) {
	r := redact.For([]string{"DE", "US", "DE", "XX"})
	for _, c := range []struct {
		name, in, want string
		n              int
	}{
		{"an IBAN", "Pay DE89370400440532013000 now", "Pay [IBAN] now", 1},
		{"an IBAN in groups", "IBAN: DE89 3704 0044 0532 0130 00.", "IBAN: [IBAN].", 1},
		{"an IBAN in groups, then a word", "to GB29 NWBK 6016 1331 9268 19 EUR", "to [IBAN] EUR", 1},
		{"an IBAN in full groups, then a word", "NL91 ABNA 0417 1643 00 and NO93 8601 1117 947 USD", "[IBAN] and [IBAN] USD", 2},
		{"an IBAN of whole groups, then a short word", "ES91 2100 0418 4502 0005 1332 EUR", "[IBAN] EUR", 1},
		{"an IBAN, then an amount and a word", "ES91 2100 0418 4502 0005 1332 1500 EUR", "[IBAN] 1500 EUR", 1},
		{"an IBAN, then words", "ES91 2100 0418 4502 0005 1332 PAID 2026", "[IBAN] PAID 2026", 1},
		{"lower case, taken for words", "on12 this week the cost was", "on12 this week the cost was", 0},
		{"a card, then its expiry", "card 4111 1111 1111 1111 12/28", "card [CARD] 12/28", 1},
		{"a card after a number", "qty 12 4111111111111111", "qty 12 [CARD]", 1},
		{"a card after a year", "Ref 2026 4111 1111 1111 1111", "Ref 2026 [CARD]", 1},
		{"two cards in a run", "4111 1111 1111 1111 5500 0000 0000 0004", "[CARD] [CARD]", 2},
		{"a long run of digits", "1234567890123456789012345", "1234567890123456789012345", 0},
		{"an IBAN after one that is not", "AB12 3456 DE89 3704 0044 0532 0130 00", "AB12 3456 [IBAN]", 1},
		{"an IBAN begun inside the groups after another", "AA11 AA11 A11 1 1Y1 0000 AA11 11 A0 7A11 8A11 0000", "[IBAN] 0000 [IBAN] 0000", 2},
		{"an IBAN with a wrong check", "DE89370400440532013001", "DE89370400440532013001", 0},
		{"a card", "card 4111 1111 1111 1111 expires", "card [CARD] expires", 1},
		{"a card with dashes", "5500-0000-0000-0004", "[CARD]", 1},
		{"a number failing Luhn", "order 4111111111111112", "order 4111111111111112", 0},
		{"a German VAT id", "USt-IdNr. DE123456789", "USt-IdNr. [TAX_ID]", 1},
		{"a German tax number", "St.-Nr. 21/815/08150", "St.-Nr. [TAX_ID]", 1},
		{"a US EIN and SSN", "EIN 12-3456789, SSN 123-45-6789", "EIN [TAX_ID], SSN [TAX_ID]", 2},
		{"another country's VAT id", "FR40303265045", "FR40303265045", 0},
		{"names and e-mail addresses", "Anna Schmidt <anna@example.com>", "Anna Schmidt <anna@example.com>", 0},
		{"amounts and dates", "1,234.56 EUR due 2026-10-18", "1,234.56 EUR due 2026-10-18", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, n := r.Text(c.in)
			if got != c.want {
				t.Errorf("text: got %q, want %q", got, c.want)
			}
			if n != c.n {
				t.Errorf("replacements: got %d, want %d", n, c.n)
			}
		})
	}
}

// TestCountries: every country's patterns compile and catch its own
// identifier; none is caught without its country.
func TestCountries(t *testing.T) {
	for country, id := range map[string]string{
		"AE": "100123456789012", "AU": "51 824 753 556", "BR": "11.222.333/0001-81", "CA": "123456789RT0001",
		"CH": "CHE-123.456.789 MWST", "CN": "91350100M000100Y43", "DE": "DE123456789", "DK": "DK13585628",
		"ES": "ESX1234567L", "FR": "FR40303265045", "GB": "GB123456789", "IE": "IE6388047V", "IN": "27AAPFU0939F1ZV",
		"IT": "IT12345678901", "JP": "T1234567890123", "MX": "GODE561231GR8", "NL": "NL123456789B01",
		"NO": "NO123456789MVA", "PL": "PL1234567890", "SA": "300000000000003", "SE": "SE123456789001",
		"SG": "M90312345A", "US": "12-3456789", "ZA": "4123456789",
	} {
		t.Run(country, func(t *testing.T) {
			if got, _ := redact.For([]string{country}).Text(id); got != redact.TaxID {
				t.Errorf("%s: got %q, want %s", country, got, redact.TaxID)
			}
			if got, _ := redact.For(nil).Text(id); got != id {
				t.Errorf("without its country: got %q, want it unchanged", got)
			}
		})
	}
}

// TestLongRuns: a megabyte of short groups, of digits or of letters and
// digits, redacts in linear time: the search for a card or an IBAN looks
// at a few groups from each, never at the whole run.
func TestLongRuns(t *testing.T) {
	r := redact.For([]string{"DE", "US"})
	for name, unit := range map[string]string{"digits": "1 ", "groups": "AA11 "} {
		t.Run(name, func(t *testing.T) {
			in := strings.Repeat(unit, (1<<20)/len(unit))
			if out, n := r.Text(in); out != in || n != 0 {
				t.Errorf("got %d replacements, want none", n)
			}
		})
	}
}

// TestCanadianBankAccounts: for a tenant in Canada, accounts written in
// the dashed forms are replaced, before a card check could take them;
// other tenants and undashed digits keep them (T45d).
func TestCanadianBankAccounts(t *testing.T) {
	ca, other := redact.For([]string{"CA"}), redact.For([]string{"DE"})
	for _, c := range []struct {
		name, in, want string
		n              int
		r              *redact.Redactor
	}{
		{"institution-transit-account", "pay to 003-12345-1234567 today", "pay to [BANK_ACCOUNT] today", 1, ca},
		{"electronic routing form", "acct 0003-12345-123456789012.", "acct [BANK_ACCOUNT].", 1, ca},
		{"cheque order", "MICR 12345-003-1234567", "MICR [BANK_ACCOUNT]", 1, ca},
		{"passing Luhn, still an account", "003-12345-1234568", "[BANK_ACCOUNT]", 1, ca},
		{"undashed digits stay", "invoice 003123451234567", "invoice 003123451234567", 0, ca},
		{"account too short", "003-12345-123456", "003-12345-123456", 0, ca},
		{"not for a tenant outside Canada", "pay to 003-12345-1234567", "pay to 003-12345-1234567", 0, other},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, n := c.r.Text(c.in)
			if got != c.want {
				t.Errorf("text: got %q, want %q", got, c.want)
			}
			if n != c.n {
				t.Errorf("count: got %d, want %d", n, c.n)
			}
		})
	}
}
