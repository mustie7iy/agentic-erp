package api

import (
	"fmt"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// extensibleEnum is the key an open enumeration's values are listed under
// (the contract's "Enumerations are open" rule).
const extensibleEnum = "x-extensible-enum"

// EnumRemovals lists the values of open enumerations (x-extensible-enum)
// that base, a contract document, lists and head no longer does, each as
// "x-extensible-enum at `<location>` removed the value `<value>`". New
// values are added within /v1, but a documented one a client may send or
// branch on is not withdrawn there: oasdiff does not read the extension,
// so batch 65 removed `method_not_allowed` and no gate noticed (06 §6.5,
// ci:batch65). A location is the path of keys down to the schema, a list's
// element named by its `name` (a parameter) or else numbered, so that an
// element added before another does not move it. An enumeration whose
// schema is gone from head is left to oasdiff, which reports the removed
// parameter, property or operation itself.
func EnumRemovals(base, head []byte) ([]string, error) {
	b, err := parseEnums(base)
	if err != nil {
		return nil, fmt.Errorf("api: base contract: %w", err)
	}
	h, err := parseEnums(head)
	if err != nil {
		return nil, fmt.Errorf("api: head contract: %w", err)
	}
	var removed []string
	for at, values := range b.values {
		if !h.schemas[at] {
			continue
		}
		for _, v := range values {
			if !slices.Contains(h.values[at], v) {
				removed = append(removed, fmt.Sprintf("%s at `%s` removed the value `%s`", extensibleEnum, at, v))
			}
		}
	}
	slices.Sort(removed)
	return removed, nil
}

// enums is a document's open enumerations by location, and every location
// that is a mapping (a schema an enumeration could sit in).
type enums struct {
	values  map[string][]string
	schemas map[string]bool
}

func parseEnums(src []byte) (enums, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return enums{}, fmt.Errorf("parse: %w", err)
	}
	out := enums{values: map[string][]string{}, schemas: map[string]bool{}}
	out.walk(&doc, "#")
	return out, nil
}

// walk records the enumerations under n, which is at the location at. A
// YAML alias is not followed: the contract shares schemas by $ref, and an
// alias's target is walked where it is anchored.
func (e enums) walk(n *yaml.Node, at string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			e.walk(c, at)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			e.walk(c, at+"/"+element(c, i))
		}
	case yaml.MappingNode:
		e.schemas[at] = true
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if key.Value == extensibleEnum && value.Kind == yaml.SequenceNode {
				for _, v := range value.Content {
					e.values[at] = append(e.values[at], v.Value)
				}
				continue
			}
			e.walk(value, at+"/"+key.Value)
		}
	case yaml.ScalarNode, yaml.AliasNode:
		// A scalar holds no enumeration; an alias's target is walked where
		// it is anchored.
	}
}

// element names a list's element: by its name when it is a mapping with
// one (a parameter, with where it is sent), by its position otherwise.
func element(n *yaml.Node, i int) string {
	name, in := "", ""
	if n.Kind == yaml.MappingNode {
		for k := 0; k+1 < len(n.Content); k += 2 {
			switch n.Content[k].Value {
			case "name":
				name = n.Content[k+1].Value
			case "in":
				in = n.Content[k+1].Value
			}
		}
	}
	switch {
	case name == "":
		return strconv.Itoa(i)
	case in == "":
		return name
	}
	return name + ":" + in
}
