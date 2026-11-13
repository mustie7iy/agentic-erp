package regulatoryagent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// segment is one step of a configuration path: a field's name and the
// keys of the list items under it ("companies[3000]", "tax_codes[GO5]",
// "rates[2025-01-01]").
var segment = regexp.MustCompile(`^([a-z_][a-z0-9_]*)((?:\[[^\[\]]+\])*)(?:\.|$)`)

// key is one bracketed key of a segment.
var key = regexp.MustCompile(`\[([^\[\]]+)\]`)

// holds reports whether the configuration document holds the path, as
// configuration paths name settings (config.Diff): fields by name, the
// items of a list by a key in brackets. The agent does not know each
// list's key field, so a list item is found by any of its own values
// (its code, number, key or date); an item keyed by several values at
// once (a number range's company, type and year) is not found by them.
func holds(doc map[string]any, path string) bool {
	if path == "" || strings.HasSuffix(path, ".") {
		return false
	}
	var at any = doc
	for path != "" {
		m := segment.FindStringSubmatch(path)
		if m == nil {
			return false
		}
		path = path[len(m[0]):]
		obj, ok := at.(map[string]any)
		if !ok {
			return false
		}
		if at, ok = obj[m[1]]; !ok {
			return false
		}
		for _, k := range key.FindAllStringSubmatch(m[2], -1) {
			if at, ok = item(at, k[1]); !ok {
				return false
			}
		}
	}
	return true
}

// item is the element of a list, or the entry of an object, that the key
// names: an element that is the key itself or has a value that is.
func item(in any, k string) (any, bool) {
	switch x := in.(type) {
	case map[string]any:
		v, ok := x[k]
		return v, ok
	case []any:
		for _, el := range x {
			if scalar(el) == k {
				return el, true
			}
			if obj, ok := el.(map[string]any); ok {
				for _, v := range obj {
					if scalar(v) == k {
						return el, true
					}
				}
			}
		}
	}
	return nil, false
}

// scalar is a JSON scalar as text, its numbers as written (the document
// is decoded with json.Number); anything else is no key.
func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number, bool:
		return fmt.Sprint(x)
	}
	return "\x00"
}
