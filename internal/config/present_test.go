package config

import "testing"

// TestPresent: a path has a value when the configuration sets it, or
// sets something below it; a path that is only the beginning of another
// name, an empty setting, an unknown item and the extensions have none.
func TestPresent(t *testing.T) {
	c := &Config{
		Companies: []Company{{Code: "3000", Currency: "CAD", ReleaseDualLimit: "4000.00",
			ChartOfAccounts: []Account{{Number: "1000", Name: "Bank"}, {Number: "10001", Name: "Bank, second"}}}},
		Payments: Payments{ReleaseRole: "Z_TREASURY_MGR"},
	}
	cases := map[string]bool{
		"companies[3000].release_dual_limit":           true, // a setting
		"companies[3000].currency":                     true, //
		"companies[3000]":                              true, // a list item
		"companies":                                    true, // a list
		"companies[3000].chart_of_accounts[1000].name": true, //
		"companies[3000].chart_of_accounts[1000]":      true, //
		"companies[3000].chart_of_accounts[10001]":     true, //
		"payments":                                    true,  // a section
		"payments.release_role":                       true,  //
		"companies[3000].release_dual":                false, // the beginning of a setting's name
		"companies[3000].chart_of_accounts[100]":      false, // the beginning of an item's key
		"companies[300]":                              false, //
		"companies[4000]":                             false, // an item the list does not hold
		"companies[3000].service_posting_limit":       false, // a setting left empty
		"companies[3000].manual_entry_approval_limit": false, // a setting that is the empty text
		"pay":                     false,
		"extensions":              false, // compared apart, not looked at
		"":                        false,
		"companies[3000].unknown": false,
	}
	paths := make([]string, 0, len(cases))
	for p := range cases {
		paths = append(paths, p)
	}
	got := c.Present(paths)
	for p, want := range cases {
		t.Run(p, func(t *testing.T) {
			if got[p] != want {
				t.Errorf("got %v, want %v", got[p], want)
			}
		})
	}
	if len(got) != len(cases) {
		t.Errorf("answers: got %d, want one per path, %d", len(got), len(cases))
	}
	t.Run("a nil configuration", func(t *testing.T) {
		var none *Config
		if got := none.Present([]string{"companies"}); got["companies"] {
			t.Error("got a value at companies, want none")
		}
	})
}
