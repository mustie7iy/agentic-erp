package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yaml truncates a floating-point number into an integer field (net_days:
// 1.5 reads as 1), so Parse refuses one wherever it stands, naming the path,
// however it got there: written out, tagged, through an alias or a merge key.
func TestParseRefusesFloatsInIntegerFields(t *testing.T) {
	for _, c := range []struct{ name, doc, path string }{
		{"fraction", "payment_terms: [{key: n, net_days: 1.5}]", "payment_terms[0].net_days"},
		{"integral float", "version: 1.0", "version"},
		{"exponent", "payments: {release_approvals: 1e0}", "payments.release_approvals"},
		{"float tag", "payments: {bank_change_hold_days: !!float 5}", "payments.bank_change_hold_days"},
		{"int64 field", "number_ranges: [{company: A, first: 2.5}]", "number_ranges[0].first"},
		{"nested slice", "payment_terms: [{key: n, net_days: 1, discounts: [{days: 0.5}]}]", "payment_terms[0].discounts[0].days"},
		{"alias", "payment_terms: [{key: &d 1.5, net_days: *d}]", "payment_terms[0].net_days"},
		{"merge key", "payment_terms: [{key: a, net_days: 1}, {key: b, <<: {net_days: 1.5}}]", "payment_terms[1].net_days"},
		{"merge list", "payment_terms: [{key: b, <<: [{key: c}, {net_days: 2.5}]}]", "payment_terms[0].net_days"},
		{"aliased merge", "payment_terms: [{key: c, <<: &m {net_days: 2.5}}, {key: b, <<: *m}]", "payment_terms[0].net_days"},
		{"nested struct", "purchasing: {contract_expiry_notice_days: 7.5}", "purchasing.contract_expiry_notice_days"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if err == nil {
				t.Fatalf("got no error, want %s refused", c.path)
			}
			if !strings.Contains(err.Error(), c.path+": ") {
				t.Errorf("got %q, want it to name %s", err, c.path)
			}
		})
	}
}

// Whole numbers, and floats in fields that are not integers, still read.
func TestParseAcceptsIntegers(t *testing.T) {
	for _, doc := range []string{
		"payment_terms: [{key: 1.5, net_days: 30}]",
		"payment_terms: [{key: a, net_days: 0x1E}]",
		"payment_terms: [{key: &k a, net_days: &d 3}, {key: b, net_days: *d}]",
		"payment_terms: [{key: a, <<: {net_days: 3}}]",
		"payment_terms: [{key: a, <<: [{net_days: 3}]}]",
		"payment_terms: [{key: a, net_days: ~}]",
	} {
		t.Run(doc, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err != nil {
				t.Errorf("got %v, want it read", err)
			}
		})
	}
}

// Config has no maps or pointers today; the walk reads them as yaml does,
// so a field added with one is covered without changing it.
func TestFloatInMapsAndPointers(t *testing.T) {
	type entry struct {
		Days *int `yaml:"days,omitempty"`
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("a: {days: 1}\nb: {days: 2.5}\n"), &root); err != nil {
		t.Fatal(err)
	}
	if _, path := floatIn(&root, reflect.TypeFor[*map[string]entry](), ""); path != "b.days" {
		t.Errorf("got path %q, want b.days", path)
	}
}
