package extensions

import "slices"

// Narrows reports whether changing a field from old to new needs the
// approval of a narrowing (ADR-0046, tier 2): a value or a row the old
// field accepted may be refused by the new one, or its privacy class
// changes. Widening a field, or making it optional, does not narrow.
func Narrows(old, new Field) bool {
	switch {
	case new.Required && !old.Required, new.Kind != old.Kind, new.PII != old.PII:
		return true
	case new.MaxLength < old.MaxLength:
		return true
	case slices.ContainsFunc(old.Enum, func(v string) bool { return !slices.Contains(new.Enum, v) }):
		return true
	}
	return tighter(old.Minimum, new.Minimum, func(o, n int64) bool { return n > o }) ||
		tighter(old.Maximum, new.Maximum, func(o, n int64) bool { return n < o })
}

// tighter reports whether bound n refuses a value bound o accepted; a nil
// bound is no bound.
func tighter(o, n *int64, refuses func(o, n int64) bool) bool {
	switch {
	case n == nil:
		return false
	case o == nil:
		return true
	}
	return refuses(*o, *n)
}

// Field returns the compiled field with the key, if the schema has it.
func (c *Compiled) Field(key string) (Field, bool) {
	i := slices.IndexFunc(c.Fields, func(f Field) bool { return f.Key == key })
	if i < 0 {
		return Field{}, false
	}
	return c.Fields[i], true
}
