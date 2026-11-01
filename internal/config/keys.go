package config

import (
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Keys and Used name the document's keys as paths, derived from the
// Config type rather than listed by hand, so a key added to the type is
// named without anyone remembering to (ADR-0038 rollout, 06 §6.4). A
// struct field is its YAML name after a dot, a list's items add "[]" and a
// map's values "{}": companies[].release_dual_limit, rules{}{}.expression.
// What lies below a field of free-form type (a rule case's row) is data,
// not keys.

var keys = sync.OnceValue(func() []string {
	out := map[string]bool{}
	typeKeys(reflect.TypeFor[Config](), "", out)
	return sortedKeys(out)
})

// Keys is every key this binary's configuration document can hold, sorted:
// what a replica of this version understands.
func Keys() []string { return slices.Clone(keys()) }

// Used is the keys the document sets to a value other than their zero
// value, sorted. A key at its zero value means what its absence means, so
// a binary that does not know it reads the document the same way.
func (c *Config) Used() []string {
	out := map[string]bool{}
	valueKeys(reflect.ValueOf(c).Elem(), "", out)
	return sortedKeys(out)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// field is a struct field's key: its YAML name, or "" when YAML skips it
// (unexported, or tagged "-"). TestConfigFieldsAreNamed keeps every
// exported field named and not inline.
func field(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if !f.IsExported() || name == "-" {
		return ""
	}
	return name
}

// typeKeys adds the keys below path that t can hold. The document's types
// are not recursive, so the walk ends.
func typeKeys(t reflect.Type, path string, out map[string]bool) {
	switch t.Kind() {
	case reflect.Pointer:
		typeKeys(t.Elem(), path, out)
	case reflect.Slice, reflect.Array:
		typeKeys(t.Elem(), path+"[]", out)
	case reflect.Map:
		typeKeys(t.Elem(), path+"{}", out)
	case reflect.Struct:
		for f := range t.Fields() {
			if name := field(f); name != "" {
				out[join(path, name)] = true
				typeKeys(f.Type, join(path, name), out)
			}
		}
	default: // a scalar, or free-form data (an interface)
	}
}

// valueKeys adds the keys below path that v sets.
func valueKeys(v reflect.Value, path string, out map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			valueKeys(v.Elem(), path, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			valueKeys(v.Index(i), path+"[]", out)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			valueKeys(it.Value(), path+"{}", out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if name, fv := field(v.Type().Field(i)), v.Field(i); name != "" && !fv.IsZero() {
				out[join(path, name)] = true
				valueKeys(fv, join(path, name), out)
			}
		}
	default:
	}
}
