package pagination

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestSize(t *testing.T) {
	if n, err := Size(0, false); n != DefaultSize || err != nil {
		t.Fatal(n, err)
	}
	if n, err := Size(500, true); n != 500 || err != nil {
		t.Fatal(n, err)
	}
	for _, bad := range []int32{0, -1, 501} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			if _, err := Size(bad, true); problem.From(err).Status != 400 {
				t.Errorf("got status %d (%v), want 400", problem.From(err).Status, err)
			}
		})
	}
}

func TestTokens(t *testing.T) {
	if _, err := NewCodec([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	c, _ := NewCodec([]byte(strings.Repeat("k", 32)))
	type cursor struct {
		At string `json:"at"`
		ID string `json:"id"`
	}
	tok, err := c.Encode("listOperations|status=running", cursor{At: "2026-10-09", ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var got cursor
	if err := c.Decode("listOperations|status=running", tok, &got); err != nil || got.ID != "x" {
		t.Fatal(got, err)
	}
	other, _ := NewCodec([]byte(strings.Repeat("z", 32)))
	payload, sig, _ := strings.Cut(tok, ".")
	for name, f := range map[string]func() error{
		"other filters":  func() error { return c.Decode("listOperations|status=failed", tok, &got) },
		"other key":      func() error { return other.Decode("listOperations|status=running", tok, &got) },
		"no signature":   func() error { return c.Decode("s", payload, &got) },
		"bad base64":     func() error { return c.Decode("s", "!!."+sig, &got) },
		"bad signature":  func() error { return c.Decode("s", payload+".!!", &got) },
		"not the struct": func() error { tok2, _ := c.Encode("s", []int{1}); return c.Decode("s", tok2, &got) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := f(); problem.From(err).Code != "request.invalid_page_token" {
				t.Errorf("got %v, want request.invalid_page_token", err)
			}
		})
	}
	if _, err := c.Encode("s", make(chan int)); err == nil {
		t.Fatal("unencodable cursor accepted")
	}
}

func TestKeyset(t *testing.T) {
	c, _ := NewCodec([]byte(strings.Repeat("k", 32)))
	sort := Sort{Default: "code", Fields: []Field{{Name: "code", Column: "code", Type: "text"}, {Name: "created_at", Column: "created_at", Type: "timestamptz"}}}
	for _, bad := range []string{"name", "code sideways", "created_at desc extra"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := c.Keyset(sort, "s", Request{OrderBy: bad}); problem.From(err).Code != "request.invalid_order_by" {
				t.Errorf("got %v, want request.invalid_order_by", err)
			}
		})
	}
	if _, err := c.Keyset(sort, "s", Request{PageSize: 0, PageSizeSet: true}); err == nil {
		t.Error("page size 0 accepted")
	}
	k, err := c.Keyset(sort, "s", Request{OrderBy: "  created_at   desc ", PageSize: 2, PageSizeSet: true})
	if err != nil || k.Field() != "created_at" {
		t.Fatal(k, err)
	}
	q, args := k.SQL("SELECT * FROM t WHERE a = $1", []any{"x"})
	if want := "SELECT * FROM t WHERE a = $1 ORDER BY created_at DESC, id DESC LIMIT $2"; q != want {
		t.Errorf("first page SQL: got %s, want %s", q, want)
	}
	if len(args) != 3 {
		t.Fatalf("first page args: got %v, want the mode and 2 of them", args)
	}
	if args[0] != pgx.QueryExecModeCacheDescribe {
		t.Errorf("first page mode: got %v, want %v (planned for its arguments every call)", args[0], pgx.QueryExecModeCacheDescribe)
	}
	if args[2] != 3 {
		t.Errorf("first page limit: got %v, want 3", args[2])
	}
	type row struct {
		v  string
		id uuid.UUID
	}
	rows := []row{{"c", uuid.New()}, {"b", uuid.New()}, {"a", uuid.New()}}
	page, tok := Page(k, rows, func(r row) (string, uuid.UUID) { return r.v, r.id })
	if len(page) != 2 {
		t.Errorf("page: got %d rows, want 2", len(page))
	}
	if tok == "" {
		t.Fatal("next page token: got none, want one")
	}
	next, err := c.Keyset(sort, "s", Request{OrderBy: "created_at desc", PageToken: tok, PageTokenSet: true})
	if err != nil {
		t.Fatal(err)
	}
	q, args = next.SQL("SELECT * FROM t WHERE true", nil)
	if want := "SELECT * FROM t WHERE true AND (created_at, id) < (CAST($1::text AS timestamptz), $2) ORDER BY created_at DESC, id DESC LIMIT $3"; q != want {
		t.Errorf("next page SQL: got %s, want %s", q, want)
	}
	if len(args) != 4 {
		t.Fatalf("next page args: got %v, want the mode and 3 of them", args)
	}
	if args[0] != pgx.QueryExecModeCacheDescribe {
		t.Errorf("next page mode: got %v, want %v", args[0], pgx.QueryExecModeCacheDescribe)
	}
	if args[1] != "b" {
		t.Errorf("next page cursor value: got %v, want b", args[1])
	}
	if args[2] != rows[1].id {
		t.Errorf("next page cursor id: got %v, want %v", args[2], rows[1].id)
	}
	if args[3] != DefaultSize+1 {
		t.Errorf("next page limit: got %v, want %d", args[3], DefaultSize+1)
	}
	// A token is bound to its order.
	if _, err := c.Keyset(sort, "s", Request{OrderBy: "created_at asc", PageToken: tok, PageTokenSet: true}); problem.From(err).Code != "request.invalid_page_token" {
		t.Fatal(err)
	}
	last, tok := Page(next, rows, func(r row) (string, uuid.UUID) { return r.v, r.id })
	if len(last) != 3 {
		t.Errorf("last page: got %d rows, want 3", len(last))
	}
	if tok != "" {
		t.Errorf("last page token: got %q, want none", tok)
	}
}
