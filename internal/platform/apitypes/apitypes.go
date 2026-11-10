// Package apitypes converts between domain values and the types ogen
// generates from the contract (api/gen): optional fields, custom-field
// extensions, money and list paging (ADR-0014, docs/design/04-api-design.md
// §4.4–4.5). It holds only the conversions every context needs alike; a
// context keeps the ones particular to its own resources. It imports no
// context, so contexts may use it without the cycle httpapi would bring.
package apitypes

import (
	"encoding/json"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// Ptr is an optional request field as a pointer: nil when it was absent.
func Ptr[T any](o interface{ Get() (T, bool) }) *T {
	if v, ok := o.Get(); ok {
		return &v
	}
	return nil
}

// SamePtr tells whether two optional values are the same: both absent, or
// both present and equal.
func SamePtr[T comparable](a, b *T) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// OptUUID is an optional response field from a pointer: unset when nil.
func OptUUID(v *uuid.UUID) gen.OptUUID {
	if v == nil {
		return gen.OptUUID{}
	}
	return gen.NewOptUUID(*v)
}

// OptString is an optional response field from a pointer: unset when nil.
func OptString(v *string) gen.OptString {
	if v == nil {
		return gen.OptString{}
	}
	return gen.NewOptString(*v)
}

// OptText is an optional response field from a string: unset when empty.
func OptText(v string) gen.OptString {
	if v == "" {
		return gen.OptString{}
	}
	return gen.NewOptString(v)
}

// OptDate is an optional date field from a pointer: unset when nil.
func OptDate(v *time.Time) gen.OptDate {
	if v == nil {
		return gen.OptDate{}
	}
	return gen.NewOptDate(*v)
}

// OptDateTime is an optional instant field from a pointer: unset when nil.
func OptDateTime(v *time.Time) gen.OptDateTime {
	if v == nil {
		return gen.OptDateTime{}
	}
	return gen.NewOptDateTime(*v)
}

// OptInt32 is an optional integer field from a pointer: unset when nil.
func OptInt32(v *int32) gen.OptInt32 {
	if v == nil {
		return gen.OptInt32{}
	}
	return gen.NewOptInt32(*v)
}

// OptNilString maps a merge-patch field: absent → nil, null → pointer to
// nil, a value → pointer to a pointer to it.
func OptNilString(s gen.OptNilString) **string {
	if !s.Set {
		return nil
	}
	var v *string
	if !s.Null {
		v = &s.Value
	}
	return &v
}

// OptNilDate maps a merge-patch date: absent → nil, null → pointer to nil,
// a value → pointer to a pointer to it.
func OptNilDate(s gen.OptNilDate) **time.Time {
	if !s.Set {
		return nil
	}
	var v *time.Time
	if !s.Null {
		v = &s.Value
	}
	return &v
}

// OptNilUUID maps a merge-patch id: absent → nil, null → pointer to nil,
// a value → pointer to a pointer to it.
func OptNilUUID(s gen.OptNilUUID) **uuid.UUID {
	if !s.Set {
		return nil
	}
	var v *uuid.UUID
	if !s.Null {
		v = &s.Value
	}
	return &v
}

// DecodeObject decodes a free-form JSON object from a request, such as
// custom fields (ADR-0009, gen.Extensions): nil when there was none.
func DecodeObject(x map[string]jx.Raw) map[string]any {
	if x == nil {
		return nil
	}
	out := make(map[string]any, len(x))
	for k, raw := range x {
		var v any
		_ = json.Unmarshal(raw, &v) // the generated decoder only passes valid JSON
		out[k] = v
	}
	return out
}

// EncodeObject encodes a free-form JSON object for a response, such as
// custom fields or connector settings; always an object, empty when there
// are no members. The result is assignable to any generated map type of
// raw JSON (gen.Extensions, gen.ConnectorSettings…).
func EncodeObject(m map[string]any) map[string]jx.Raw {
	out := make(map[string]jx.Raw, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v) // values decoded from JSON always encode
		out[k] = jx.Raw(b)
	}
	return out
}

// Money is an amount in the API's representation (ADR-0005).
func Money(amount money.Minor, currency string, exp int) gen.Money {
	return gen.Money{Amount: amount.Plain(exp), Currency: currency}
}

// Page is a list request's paging parameters (04-api-design §4.5).
func Page(orderBy gen.OptString, size gen.OptInt32, token gen.OptString) pagination.Request {
	return pagination.Request{OrderBy: orderBy.Value, PageSize: size.Value, PageSizeSet: size.Set, PageToken: token.Value, PageTokenSet: token.Set}
}

// PageOf finishes a keyset-paged list (04-api-design §4.5): it trims rows
// to the page, converts each to its API representation and returns the
// next page's token, unset on the last page. Items is never nil, so an
// empty page encodes as [].
func PageOf[R, A any](k *pagination.Keyset, rows []R, key func(R) (string, uuid.UUID), toAPI func(R) A) ([]A, gen.OptString) {
	rows, next := pagination.Page(k, rows, key)
	items := make([]A, len(rows))
	for i, r := range rows {
		items[i] = toAPI(r)
	}
	var tok gen.OptString
	if next != "" {
		tok.SetTo(next)
	}
	return items, tok
}

// PageWith is PageOf for rows shown with what they reference (the names
// of the objects they name by id, ADR-0038 As built): refs reads it for the
// page's rows alone, never the row past the page, in one call, and toAPI
// shows each row with it. refs failing fails the page.
func PageWith[R, X, A any](k *pagination.Keyset, rows []R, key func(R) (string, uuid.UUID), refs func([]R) (X, error),
	toAPI func(R, X) A) ([]A, gen.OptString, error) {
	rows, next := pagination.Page(k, rows, key)
	x, err := refs(rows)
	if err != nil {
		return nil, gen.OptString{}, err
	}
	items := make([]A, len(rows))
	for i, r := range rows {
		items[i] = toAPI(r, x)
	}
	var tok gen.OptString
	if next != "" {
		tok.SetTo(next)
	}
	return items, tok, nil
}

// ETagOf is the entity tag of a resource without a version, derived from
// its representation (04-api-design §4.7).
func ETagOf[T any](v T) gen.OptString { return gen.NewOptString(etag.Of(&v)) }
