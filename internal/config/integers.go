package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// notWhole returns an error naming the first floating-point number given
// for an integer field anywhere in the document, or nil. yaml truncates one
// silently into an integer (net_days: 1.5 reads as 1), so a wrong document
// would otherwise be accepted without notice. The document is walked
// against Config's types as yaml decoded it: aliases followed, merge keys
// read into the mapping that holds them. A float-tagged whole number (1.0,
// 1e0, !!float 5) is refused too: the field asks for an integer.
func notWhole(b []byte) error {
	var root yaml.Node
	_ = yaml.Unmarshal(b, &root) // Parse decoded the same bytes already (both read the first document), so this cannot fail
	n, path := floatIn(&root, reflect.TypeFor[Config](), "")
	if n == nil {
		return nil
	}
	return fmt.Errorf("configuration: line %d: %s: %s is not a whole number", n.Line, path, n.Value)
}

// floatIn walks n as a value of type t at path and returns the first
// float-tagged scalar standing in an integer field, with its path.
func floatIn(n *yaml.Node, t reflect.Type, path string) (*yaml.Node, string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch n.Kind {
	case yaml.DocumentNode:
		return floatIn(n.Content[0], t, path)
	case yaml.AliasNode:
		return floatIn(n.Alias, t, path)
	case yaml.ScalarNode:
		if integer(t) && n.ShortTag() == "!!float" {
			return n, path
		}
	case yaml.SequenceNode:
		if t.Kind() == reflect.Slice {
			return floatInSequence(n, t.Elem(), path)
		}
	case yaml.MappingNode:
		return floatInMapping(n, t, path)
	}
	return nil, ""
}

func integer(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

func floatInSequence(n *yaml.Node, elem reflect.Type, path string) (*yaml.Node, string) {
	for i, c := range n.Content {
		if f, p := floatIn(c, elem, path+"["+strconv.Itoa(i)+"]"); f != nil {
			return f, p
		}
	}
	return nil, ""
}

// floatInMapping walks a struct's fields or a map's values. A merge key
// (<<) holds a mapping, or a list of them, read into the same value, so it
// is walked with the mapping's own type and path.
func floatInMapping(n *yaml.Node, t reflect.Type, path string) (*yaml.Node, string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var f *yaml.Node
		var p string
		switch vt, ok := valueType(t, k.Value); {
		case k.ShortTag() == "!!merge":
			f, p = floatInMerge(v, t, path)
		case ok:
			f, p = floatIn(v, vt, join(path, k.Value))
		}
		if f != nil {
			return f, p
		}
	}
	return nil, ""
}

func floatInMerge(v *yaml.Node, t reflect.Type, path string) (*yaml.Node, string) {
	if v.Kind != yaml.SequenceNode {
		return floatIn(v, t, path)
	}
	for _, m := range v.Content {
		if f, p := floatIn(m, t, path); f != nil {
			return f, p
		}
	}
	return nil, ""
}

// valueType is the type a mapping key's value decodes into: the struct
// field the key names in its yaml tag, or a map's element type. A key that
// names nothing was refused by the strict decode already. Only the tag
// name is matched: TestConfigFieldsAreNamed keeps every field named.
func valueType(t reflect.Type, key string) (reflect.Type, bool) {
	if t.Kind() == reflect.Map {
		return t.Elem(), true
	}
	for i := range t.NumField() { // a mapping decodes only into a map or a struct
		f := t.Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); name == key {
			return f.Type, true
		}
	}
	return nil, false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
