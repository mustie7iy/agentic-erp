package config

import "testing"

// TestHolds: a path is held when the configuration has a setting at it or
// inside it, its list items named by their lists' keys only.
func TestHolds(t *testing.T) {
	c := &Config{Companies: []Company{{Code: "1000", TaxCodes: []TaxCode{{Code: "V0", Description: "Input tax exempt", RatePercent: "0"}}}}}
	for path, want := range map[string]bool{
		"companies":                                   true,
		"companies[1000]":                             true,
		"companies[1000].tax_codes[V0]":               true,
		"companies[1000].tax_codes[V0].rate_percent":  true,
		"companies[1000].tax_codes[0]":                false,
		"companies[1000].tax_codes[Input tax exempt]": false,
		"companies[1000].tax_codes[V9]":               false,
		"companies[1000].tax_codes[V0].":              false,
		"companies[2000]":                             false,
		"compan":                                      false,
		"":                                            false,
	} {
		t.Run(path, func(t *testing.T) {
			if got := Holds(c, path); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
