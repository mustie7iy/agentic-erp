package pagination

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// PlanPerCall is the pgx Query option every listing runs with: passed as
// the first argument, it makes pgx run the query as an unnamed statement,
// which PostgreSQL plans for its arguments every time. Listings filter
// with optional conditions ($n IS NULL OR column = $n). Run as a cached
// prepared statement, PostgreSQL may switch to a generic plan after five
// runs; that plan cannot drop the conditions that do not apply nor use the
// index of the sort, so every page read the whole table (the nightly
// soak's drift). Planning each call costs far less than that. It works
// only with Query (a Batch does not read the mode) and needs pgx's
// description cache (description_cache_capacity above zero). Keyset.SQL
// adds it; a listing that pages by other means passes it itself.
const PlanPerCall = pgx.QueryExecModeCacheDescribe

// SQL completes a query whose WHERE clause is where (args numbered from $1)
// with the cursor condition, the order and the limit. It fetches one row
// more than the page so Page can tell whether another page follows. The
// arguments it returns start with PlanPerCall, for pgx's Query.
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
	return query + fmt.Sprintf(" ORDER BY %s %s, id %s LIMIT $%d", col, dir, dir, len(args)), append([]any{PlanPerCall}, args...)
}

// Instant is a timestamptz sort value as a cursor holds it: RFC 3339 in
// UTC with the fraction PostgreSQL keeps (microseconds), which the
// cursor's cast parses back to the same instant. Every listing sorted by a
// time keys its rows with it, so a cursor's text has one form.
func Instant(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

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

// InMemory pages rows that are read whole rather than by a statement:
// it sorts them by a text key, then id, compared byte by byte as the
// cursor is, in the keyset's direction, keeps those after the cursor and
// pages them as Page does. A listing computed rather than stored still
// answers a page at a time, whatever order it was computed in.
func InMemory[T any](k *Keyset, rows []T, key func(T) (string, uuid.UUID)) ([]T, string) {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b T) int {
		va, ia := key(a)
		vb, ib := key(b)
		c := cmp.Or(strings.Compare(va, vb), strings.Compare(ia.String(), ib.String()))
		if k.desc {
			return -c
		}
		return c
	})
	start := 0
	if k.after != nil {
		for start < len(sorted) && !k.past(key(sorted[start])) {
			start++
		}
	}
	sorted = sorted[start:]
	return Page(k, sorted[:min(len(sorted), k.size+1)], key)
}

// past reports whether a row's key lies after the cursor in the order.
func (k *Keyset) past(v string, id uuid.UUID) bool {
	c := strings.Compare(v, k.after.V)
	if c == 0 {
		c = strings.Compare(id.String(), k.after.ID.String())
	}
	if k.desc {
		return c < 0
	}
	return c > 0
}
