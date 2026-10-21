package countrypack

import (
	"os"
	"testing"
)

// Arbitrary pack files never panic the parser or the validator.
func FuzzParseValidate(f *testing.F) {
	read := func(name string) []byte {
		b, _ := os.ReadFile("testdata/packs/zz/2026.1/" + name) // a missing seed is just an empty one
		return b
	}
	f.Add(read("pack.yaml"), read("accounts.yaml"), read("tax.yaml"), read("numbering.yaml"))
	f.Add([]byte("country: ZZ\nversion: 2026.1\n"), []byte("accounts: [{number: 1}]\n"),
		[]byte("tax_codes: [{code: V, rates: [{rate_percent: 1e9, valid_from: 2026-01-01}]}]\n"), []byte("number_ranges: [{first: 2}]\n"))
	f.Fuzz(func(t *testing.T, manifest, accounts, tax, numbering []byte) {
		p, err := Parse(map[string][]byte{"pack.yaml": manifest, "accounts.yaml": accounts, "tax.yaml": tax,
			"calendar.yaml": nil, "numbering.yaml": numbering, "payments.yaml": nil})
		if err != nil {
			return
		}
		Validate(p)
	})
}
