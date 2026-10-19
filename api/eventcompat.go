package api

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// schema is the part of a JSON Schema that decides whether a consumer of
// an event version can still read it.
type schema struct {
	Ref        string             `yaml:"$ref"`
	Type       yaml.Node          `yaml:"type"`
	Const      string             `yaml:"const"`
	Properties map[string]*schema `yaml:"properties"`
	Required   []string           `yaml:"required"`
	Items      *schema            `yaml:"items"`
	OneOf      []*schema          `yaml:"oneOf"`
	AnyOf      []*schema          `yaml:"anyOf"`
	AllOf      []*schema          `yaml:"allOf"`
}

// eventDoc is an AsyncAPI document's messages and the schemas they use.
type eventDoc struct {
	Components struct {
		Messages map[string]struct {
			Headers schema  `yaml:"headers"`
			Payload *schema `yaml:"payload"`
		} `yaml:"messages"`
		Schemas map[string]*schema `yaml:"schemas"`
	} `yaml:"components"`
}

// maxDepth bounds how deep schemas are followed, so that a recursive $ref
// ends.
const maxDepth = 32

// EventChanges lists what changed from base to head, two AsyncAPI
// documents, that breaks a consumer of an event version: within a .vN
// type only additions are allowed, so a removed event type, a removed
// field, a changed type or a newly required field needs a .vN+1 published
// alongside (ADR-0041). Messages are matched by their CloudEvents type.
// Only types, properties, required, items and the alternatives are
// compared: formats, enums, consts and headers are left to review.
func EventChanges(base, head []byte) ([]string, error) {
	b, err := parseEvents(base)
	if err != nil {
		return nil, fmt.Errorf("api: base events: %w", err)
	}
	h, err := parseEvents(head)
	if err != nil {
		return nil, fmt.Errorf("api: head events: %w", err)
	}
	c := &comparison{base: b.schemas, head: h.schemas}
	for typ, payload := range b.payloads {
		next, ok := h.payloads[typ]
		if !ok {
			c.add(typ, "event type removed")
			continue
		}
		c.compare(typ, payload, next, 0)
	}
	slices.Sort(c.changes)
	return c.changes, c.err
}

// events is a document's payload schema by event type, and its schemas.
type events struct {
	payloads map[string]*schema
	schemas  map[string]*schema
}

func parseEvents(src []byte) (events, error) {
	var doc eventDoc
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return events{}, fmt.Errorf("parse: %w", err)
	}
	out := events{payloads: map[string]*schema{}, schemas: doc.Components.Schemas}
	for name, m := range doc.Components.Messages {
		typ := eventType(name, m.Headers)
		if _, dup := out.payloads[typ]; dup {
			return events{}, fmt.Errorf("two messages publish %s", typ)
		}
		out.payloads[typ] = m.Payload
	}
	return out, nil
}

// eventType is the const of a message's type header, or its name when it
// declares none.
func eventType(name string, headers schema) string {
	for _, part := range headers.AllOf {
		if t := part.Properties["type"]; t != nil && t.Const != "" {
			return t.Const
		}
	}
	return name
}

// comparison collects the breaking changes found between two documents.
type comparison struct {
	base, head map[string]*schema
	changes    []string
	err        error
}

func (c *comparison) add(path, format string, args ...any) {
	c.changes = append(c.changes, path+": "+fmt.Sprintf(format, args...))
}

// compare adds what breaks a reader of b when it is given h.
func (c *comparison) compare(path string, b, h *schema, depth int) {
	if b == nil || depth > maxDepth {
		return
	}
	b, h = c.resolve(c.base, b), c.resolve(c.head, h)
	if b == nil || h == nil {
		if b != nil {
			c.add(path, "schema removed")
		}
		return
	}
	if bt, ht := typeOf(b), typeOf(h); bt != ht {
		c.add(path, "type changed from %s to %s", bt, ht)
	}
	for name, p := range b.Properties {
		next, ok := h.Properties[name]
		if !ok {
			c.add(path+"."+name, "field removed")
			continue
		}
		c.compare(path+"."+name, p, next, depth+1)
	}
	c.required(path, b.Required, h.Required)
	if b.Items != nil {
		c.compare(path+"[]", b.Items, h.Items, depth+1)
	}
	c.variants(path+".oneOf", b.OneOf, h.OneOf, depth)
	c.variants(path+".anyOf", b.AnyOf, h.AnyOf, depth)
	c.variants(path+".allOf", b.AllOf, h.AllOf, depth)
}

// required adds a field that is no longer required, which a reader relies
// on finding, and one newly required, which a producer of the version may
// not send.
func (c *comparison) required(path string, b, h []string) {
	for _, name := range b {
		if !slices.Contains(h, name) {
			c.add(path+"."+name, "field no longer required")
		}
	}
	for _, name := range h {
		if !slices.Contains(b, name) {
			c.add(path+"."+name, "field newly required")
		}
	}
}

// variants compares the alternatives of oneOf, anyOf or allOf by position.
func (c *comparison) variants(path string, b, h []*schema, depth int) {
	for i, s := range b {
		var next *schema
		if i < len(h) {
			next = h[i]
		}
		c.compare(fmt.Sprintf("%s[%d]", path, i), s, next, depth+1)
	}
}

// resolve follows a schema's $ref within its document's components; a ref
// elsewhere is an error, since the check could not see what it means.
func (c *comparison) resolve(schemas map[string]*schema, s *schema) *schema {
	for depth := 0; s != nil && s.Ref != "" && depth <= maxDepth; depth++ {
		name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/")
		target, found := schemas[name]
		if !ok || !found {
			if c.err == nil {
				c.err = fmt.Errorf("api: unresolved $ref %q", s.Ref)
			}
			return nil
		}
		s = target
	}
	return s
}

// typeOf is a schema's type, a list of types sorted and joined by "|".
func typeOf(s *schema) string {
	if s.Type.Kind == yaml.SequenceNode {
		var types []string
		for _, n := range s.Type.Content {
			types = append(types, n.Value)
		}
		slices.Sort(types)
		return strings.Join(types, "|")
	}
	if s.Type.Value == "" {
		return "unspecified"
	}
	return s.Type.Value
}
