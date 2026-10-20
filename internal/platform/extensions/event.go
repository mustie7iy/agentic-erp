package extensions

import (
	"context"
	"maps"
)

// Masked is what an event carries in place of a custom field's value when
// the field is personal or sensitive (ADR-0046).
const Masked = "[MASKED]"

// Event adds a row's custom fields to the payload of one of its events:
// `extensions`, the row's extensions as written with every personal and
// sensitive value masked, and `extensions_schema`, the version they were
// written under (nil without a schema). Masking reads the active schema
// and shows only the values of fields it declares `x-pii: none`: a key it
// does not declare, such as one written before the schema or under an
// older version, is masked, so no value is shown that the schema does not
// classify as public. Webhooks deliver these events, so they inherit the
// masking.
func (w Writes) Event(ctx context.Context, payload, ext map[string]any, version *int) error {
	c, err := w.schema(ctx)
	if err != nil {
		return err
	}
	payload["extensions"], payload["extensions_schema"] = c.Mask(ext), version
	return nil
}

// Mask returns a copy of ext with the value of every key the schema does
// not declare `x-pii: none` replaced by Masked; nil is an empty object.
func (c *Compiled) Mask(ext map[string]any) map[string]any {
	out := maps.Clone(ext)
	if out == nil {
		out = map[string]any{}
	}
	for k := range out {
		if f, ok := c.Field(k); !ok || f.PII != PIINone {
			out[k] = Masked
		}
	}
	return out
}
