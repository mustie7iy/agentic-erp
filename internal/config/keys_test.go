package config

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// TestKeys: the keys a replica understands come from the Config type, so a
// key added to it is named without a list to keep (ADR-0038 rollout).
func TestKeys(t *testing.T) {
	keys := Keys()
	if !slices.IsSorted(keys) {
		t.Errorf("keys: got %v, want them sorted", keys)
	}
	for _, k := range []string{"version", "companies", "companies[].release_dual_limit", "companies[].house_banks[].iban",
		"extensions{}.properties{}.type", "rules{}{}.expression", "rules{}{}.cases[].row", "analytics"} {
		if !slices.Contains(keys, k) {
			t.Errorf("keys: got none %q, want it", k)
		}
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "rules{}{}.cases[].row.") {
			t.Errorf("key %q: got a key below a rule case's row, want none (its contents are data)", k)
		}
	}
	keys[0] = "changed"
	if Keys()[0] == "changed" {
		t.Error("Keys: got the shared slice, want a copy")
	}
}

// TestUsed: a document uses the keys it sets to something other than their
// zero value, which an older binary reads the same as their absence.
func TestUsed(t *testing.T) {
	b, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	keys := Keys()
	for _, k := range c.Used() {
		if !slices.Contains(keys, k) {
			t.Errorf("used key %q: got none in Keys, want every used key known", k)
		}
	}
	for name, want := range map[string]bool{"companies[].code": true, "companies[].release_dual_limit": false, "analytics": false} {
		if got := slices.Contains(c.Used(), name); got != want {
			t.Errorf("%s used: got %v, want %v", name, got, want)
		}
	}
	c.Companies[0].ReleaseDualLimit = "10000.00"
	c.Rules = map[string]map[string]rules.Rule{"supplier_invoice": {"r": {Expression: "true"}}}
	c.Analytics = &Analytics{} // not the seed: a document of its own
	used := c.Used()
	for _, k := range []string{"companies[].release_dual_limit", "rules", "rules{}{}.expression", "analytics"} {
		if !slices.Contains(used, k) {
			t.Errorf("used: got no %q, want it once set: %v", k, used)
		}
	}
	if slices.Contains(used, "rules{}{}.message") {
		t.Error("used: got rules{}{}.message, want an empty message unused")
	}
}

// TestKeysSkipFieldsYAMLSkips: unexported fields and those tagged "-" are
// no keys; arrays add "[]" as slices do.
func TestKeysSkipFieldsYAMLSkips(t *testing.T) {
	type doc struct {
		Named [1]struct {
			A int `yaml:"a"`
		} `yaml:"named"`
		Skipped  int `yaml:"-"`
		internal int
	}
	known := map[string]bool{}
	typeKeys(reflect.TypeFor[doc](), "", known)
	if want := map[string]bool{"named": true, "named[].a": true}; !reflect.DeepEqual(known, want) {
		t.Errorf("keys: got %v, want %v", known, want)
	}
	used := map[string]bool{}
	d := doc{Skipped: 1, internal: 1}
	d.Named[0].A = 1
	valueKeys(reflect.ValueOf(d), "", used)
	if want := map[string]bool{"named": true, "named[].a": true}; !reflect.DeepEqual(used, want) {
		t.Errorf("used: got %v, want %v", used, want)
	}
}
