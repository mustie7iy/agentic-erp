package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var placeholder = regexp.MustCompile(`\{\{([a-z][a-z0-9_]*)\}\}`)

// fill replaces {{name}} in every string of v with the saved value; a name
// not saved is an error, so a case never runs on a half-filled call.
func fill(v any, saved map[string]string) (any, error) {
	switch v := v.(type) {
	case string:
		var missing string
		out := placeholder.ReplaceAllStringFunc(v, func(m string) string {
			name := placeholder.FindStringSubmatch(m)[1]
			value, ok := saved[name]
			if !ok {
				missing = name
			}
			return value
		})
		if missing != "" {
			return nil, fmt.Errorf("eval: {{%s}} was not saved", missing)
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i := range v {
			var err error
			if out[i], err = fill(v[i], saved); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			var err error
			if out[k], err = fill(e, saved); err != nil {
				return nil, err
			}
		}
		return based(out)
	}
	return v, nil
}

// based merges a filled map onto its $base, a saved JSON object, so that a
// call can send a whole document read earlier with a few settings changed
// (a configuration change request's proposed document) without the suite
// copying it. Maps merge key by key; anything else replaces.
func based(m map[string]any) (any, error) {
	b, ok := m["$base"]
	if !ok {
		return m, nil
	}
	delete(m, "$base")
	s, _ := b.(string) // fill keeps a string a string; anything else is not JSON text
	doc, err := decode([]byte(s))
	base, isMap := doc.(map[string]any)
	if err != nil || !isMap {
		return nil, fmt.Errorf("eval: $base is not a saved JSON object: %q", s)
	}
	return merge(base, m), nil
}

func merge(base, over map[string]any) map[string]any {
	for k, v := range over {
		bm, ok1 := base[k].(map[string]any)
		om, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			v = merge(bm, om)
		}
		base[k] = v
	}
	return base
}

// Fill is fill for replays: a turn's input and text, from the saved values
// and the replay's own.
func Fill(v any, saved map[string]string) (any, error) { return fill(v, saved) }

// fillString is fill for a path or a text.
func fillString(s string, saved map[string]string) (string, error) {
	v, err := fill(s, saved)
	if err != nil {
		return "", err
	}
	return v.(string), nil // fill keeps a string a string
}

// at is the value at a dotted path of a decoded JSON answer: object keys,
// list indexes, "#" for a list's length and "?field=value" for a list's
// first element whose field is value; ok false when the path does not lead
// anywhere.
func at(doc any, path string) (any, bool) {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		var ok bool
		switch v := cur.(type) {
		case map[string]any:
			cur, ok = v[part]
		case []any:
			cur, ok = inList(v, part)
		}
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// inList is one step of a path into a list: its length, an element by
// index, or the first element whose field has a value.
func inList(v []any, part string) (any, bool) {
	if part == "#" {
		return json.Number(strconv.Itoa(len(v))), true
	}
	if cond, ok := strings.CutPrefix(part, "?"); ok {
		field, want, ok := strings.Cut(cond, "=")
		if !ok {
			return nil, false
		}
		return where(v, field, want)
	}
	i, err := strconv.Atoi(part)
	if err != nil || i < 0 || i >= len(v) {
		return nil, false
	}
	return v[i], true
}

// same reports whether a decoded JSON value equals a suite's expected one,
// both compared as their JSON: 3 and 3.0 match, as do "3" and "3" only.
func same(got, want any) bool {
	g, _ := json.Marshal(got)    // decoded JSON always encodes
	w, err := json.Marshal(want) // a YAML value of the suite
	return err == nil && string(g) == string(w)
}

// text is a saved value as text.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v) // decoded JSON always encodes
	return string(b)
}

// where is the first element of list whose field, as text, is want.
func where(list []any, field, want string) (any, bool) {
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			if v, ok := m[field]; ok && text(v) == want {
				return e, true
			}
		}
	}
	return nil, false
}
