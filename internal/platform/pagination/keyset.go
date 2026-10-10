package pagination

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Field is a sortable field of a listing: its name in order_by, the column
// it sorts on and the column's SQL type (the cursor value is cast to it).
// Sort columns are NOT NULL; ties are broken by id.
type Field struct {
	Name, Column, Type string
}

// Sort is the order_by a listing accepts: its fields and the default.
type Sort struct {
	Fields      []Field
	Default     string // field name used without order_by
	DefaultDesc bool
}

// Keyset is a parsed listing request: order, page size and cursor. Every
// page is an index range scan on (sort column, id).
type Keyset struct {
	codec *Codec
	scope string
	field Field
	desc  bool
	size  int
	after *keysetCursor
}

type keysetCursor struct {
	V  string    `json:"v"`
	ID uuid.UUID `json:"i"`
}

// Request is the paging part of a list call.
type Request struct {
	OrderBy      string
	PageSize     int32
	PageSizeSet  bool
	PageToken    string
	PageTokenSet bool
}

// Keyset validates order_by, the page size and the page token. scope names
// the listing and its filters; the order is added to it.
func (c *Codec) Keyset(s Sort, scope string, r Request) (*Keyset, error) {
	size, err := Size(r.PageSize, r.PageSizeSet)
	if err != nil {
		return nil, err
	}
	k := &Keyset{codec: c, size: size, desc: s.DefaultDesc}
	name := s.Default
	if spec := strings.Join(strings.Fields(r.OrderBy), " "); spec != "" {
		f, dir, _ := strings.Cut(spec, " ")
		switch dir {
		case "", "asc":
			k.desc = false
		case "desc":
			k.desc = true
		default:
			return nil, badOrder(s)
		}
		name = f
	}
	found := false
	for _, f := range s.Fields {
		if f.Name == name {
			k.field, found = f, true
		}
	}
	if !found {
		return nil, badOrder(s)
	}
	k.scope = fmt.Sprintf("%s|%s %t", scope, k.field.Name, k.desc)
	if r.PageTokenSet {
		k.after = &keysetCursor{}
		if err := c.Decode(k.scope, r.PageToken, k.after); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func badOrder(s Sort) error {
	names := make([]string, len(s.Fields))
	for i, f := range s.Fields {
		names[i] = f.Name
	}
	accepted := strings.Join(names, ", ")
	return problem.BadRequest("request.invalid_order_by", "order_by accepts one of: "+accepted+" (asc or desc).",
		problem.FieldError{Field: "order_by", Code: "unsupported", Message: accepted})
}

// Field is the name of the field the listing is sorted by.
func (k *Keyset) Field() string { return k.field.Name }

// SQL completes a query whose WHERE clause is where (args numbered from $1)
// with the cursor condition, the order and the limit. It fetches one row
// more than the page so Page can tell whether another page follows.
func (k *Keyset) SQL(query string, args []any) (string, []any) {
	cmp, dir := ">", "ASC"
	if k.desc {
		cmp, dir = "<", "DESC"
	}
	col := k.field.Column
	if k.after != nil {
		args = append(args, k.after.V, k.after.ID)
		query += fmt.Sprintf(" AND (%s, id) %s (CAST($%d::text AS %s), $%d)", col, cmp, len(args)-1, k.field.Type, len(args))
	}
	args = append(args, k.size+1)
	return query + fmt.Sprintf(" ORDER BY %s %s, id %s LIMIT $%d", col, dir, dir, len(args)), args
}

// Page trims rows to the page and returns the token of the next page, or ""
// on the last one. key returns a row's sort value as text in the form the
// column's type parses, and its id.
func Page[T any](k *Keyset, rows []T, key func(T) (string, uuid.UUID)) ([]T, string) {
	if len(rows) <= k.size {
		return rows, ""
	}
	rows = rows[:k.size]
	v, id := key(rows[k.size-1])
	tok, _ := k.codec.Encode(k.scope, keysetCursor{V: v, ID: id}) // strings and a uuid always encode
	return rows, tok
}
