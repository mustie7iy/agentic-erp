package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// extensions checks each entity's custom field schema against ADR-0046's
// restrictions.
func (v *validator) extensions(c *Config) {
	for _, entity := range slices.Sorted(maps.Keys(c.Extensions)) {
		_, issues := extensions.Compile(entity, c.Extensions[entity])
		v.issues("extensions."+entity, issues)
	}
}

// issues reports a compiler's issues, their paths relative to prefix.
func (v *validator) issues(prefix string, issues []extensions.Issue) {
	for _, is := range issues {
		p := prefix
		if is.Path != "" {
			p += "." + is.Path
		}
		v.err(p, "%s", is.Message)
	}
}

// ExtensionTransitions checks the custom field schemas against the base
// document (nil for the first version). A schema version is immutable: a
// changed schema carries the base version plus one, an unchanged one keeps
// its number, a new one starts at 1, and a schema is never removed (its
// fields are), so a version number always means the same schema.
func ExtensionTransitions(base, proposed *Config) []Finding {
	var old map[string]extensions.Schema
	if base != nil {
		old = base.Extensions
	}
	var out []Finding
	for _, entity := range slices.Sorted(maps.Keys(old)) {
		if _, ok := proposed.Extensions[entity]; !ok {
			out = append(out, Finding{SevError, "extensions." + entity, "a custom field schema cannot be removed; remove its fields instead"})
		}
	}
	for _, entity := range slices.Sorted(maps.Keys(proposed.Extensions)) {
		s, want := proposed.Extensions[entity], 1
		if o, ok := old[entity]; ok {
			want = o.Version
			if !sameSchema(o, s) {
				want++
			}
		}
		if s.Version != want {
			out = append(out, Finding{SevError, "extensions." + entity + ".version",
				fmt.Sprintf("must be %d: a schema version, once activated, always means the same schema", want)})
		}
	}
	return out
}

// sameSchema compares two schemas apart from their versions and the order
// of their required keys. Schemas hold
// only JSON-safe types, so encoding cannot fail.
func sameSchema(a, b extensions.Schema) bool {
	a.Version, b.Version = 0, 0
	a.Required, b.Required = slices.Sorted(slices.Values(a.Required)), slices.Sorted(slices.Values(b.Required)) // an order is no change
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// extensionChanges lists the custom field changes of a diff, one per field,
// with the tier ADR-0046 gives it: adding an optional field and widening
// one are tier 1; adding a required or sensitive field, narrowing,
// retyping, reclassifying or removing one are tier 2.
func extensionChanges(old, new *Config) []Change {
	var a, b map[string]extensions.Schema
	if old != nil {
		a = old.Extensions
	}
	if new != nil {
		b = new.Extensions
	}
	entities := slices.Sorted(maps.Keys(a))
	for e := range b {
		if _, ok := a[e]; !ok {
			entities = append(entities, e)
		}
	}
	slices.Sort(entities)
	var out []Change
	for _, entity := range entities {
		out = append(out, schemaChanges(entity, a[entity], b[entity])...)
	}
	return out
}

func schemaChanges(entity string, a, b extensions.Schema) []Change {
	p := "extensions." + entity
	var out []Change
	if a.Version != b.Version {
		out = append(out, Change{Path: p + ".version", Old: number(a.Version), New: number(b.Version), Tier: TierCosmetic})
	}
	ca, _ := extensions.Compile(entity, a) // invalid schemas are refused by Validate; compared as far as they read
	cb, _ := extensions.Compile(entity, b)
	keys := slices.Collect(maps.Keys(a.Properties))
	for k := range b.Properties {
		if _, ok := a.Properties[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if ch, ok := fieldChange(p+".properties."+k, ca, cb, a.Properties[k], b.Properties[k], k); ok {
			out = append(out, ch)
		}
	}
	return out
}

func fieldChange(path string, ca, cb *extensions.Compiled, pa, pb extensions.Property, key string) (Change, bool) {
	fa, inA := ca.Field(key)
	fb, inB := cb.Field(key)
	ch := Change{Path: path, Tier: TierCosmetic}
	switch {
	case !inA:
		ch.New = describe(pb, fb.Required)
		if fb.Required || fb.PII == extensions.PIISensitive {
			ch.Tier = TierOperational
		}
	case !inB:
		ch.Old, ch.Tier = describe(pa, fa.Required), TierOperational
	default:
		ch.Old, ch.New = describe(pa, fa.Required), describe(pb, fb.Required)
		if ch.Old == ch.New {
			return ch, false
		}
		if extensions.Narrows(fa, fb) {
			ch.Tier = TierOperational
		}
	}
	return ch, true
}

// describe renders a field's declaration for a change item.
func describe(p extensions.Property, required bool) string {
	b, _ := json.Marshal(struct { // only JSON-safe types
		extensions.Property
		Required bool `json:"required,omitempty"`
	}{p, required})
	return string(b)
}

func number(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// rules compiles each entity's validation rules against its standard
// fields and the custom field schema of the same document, and runs their
// unit cases (ADR-0046).
func (v *validator) rules(c *Config) {
	for _, entity := range slices.Sorted(maps.Keys(c.Rules)) {
		var schema *extensions.Compiled
		if s, ok := c.Extensions[entity]; ok {
			schema, _ = extensions.Compile(entity, s) // its issues are the extensions section's findings
		}
		_, issues := rules.Compile(entity, schema, c.Rules[entity])
		v.issues("rules."+entity, issues)
	}
}
