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

// TestSetting: a setting's value is read as Diff writes it; a list item, a
// group and a path not held have none, as has every path of no
// configuration.
func TestSetting(t *testing.T) {
	c := &Config{Companies: []Company{{Code: "1000", InvoiceTolerance: Tolerance{PricePercent: 2, PriceAbsolute: "5.00"}}}}
	for path, want := range map[string]string{
		"companies[1000].invoice_tolerance.price_percent":  "2",
		"companies[1000].invoice_tolerance.price_absolute": "5.00",
		"companies[1000].invoice_tolerance":                "",
		"companies[1000]":                                  "",
		"companies[2000].invoice_tolerance.price_percent":  "",
	} {
		t.Run(path, func(t *testing.T) {
			got, ok := Setting(c, path)
			if got != want || ok != (want != "") {
				t.Errorf("got %q %v, want %q", got, ok, want)
			}
		})
	}
	if _, ok := Setting(nil, "companies[1000].invoice_tolerance.price_percent"); ok {
		t.Error("no configuration: got a setting, want none")
	}
}

// TestReadable: an activated document reads as a configuration, keys a
// newer binary added ignored; one that is no configuration is none.
func TestReadable(t *testing.T) {
	if c := Readable([]byte(`{"companies": [{"code": "1000"}], "added_later": 1}`)); c == nil || c.Companies[0].Code != "1000" {
		t.Errorf("a document: got %+v, want company 1000", c)
	}
	if c := Readable([]byte(`{"companies": "none"}`)); c != nil {
		t.Errorf("no configuration: got %+v, want none", c)
	}
}
