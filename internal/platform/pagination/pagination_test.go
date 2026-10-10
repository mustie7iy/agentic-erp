package pagination

import (
	"strings"
	"testing"

	"github.com/google/uuid"

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
		if _, err := Size(bad, true); problem.From(err).Status != 400 {
			t.Errorf("%d accepted", bad)
		}
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
		if err := f(); problem.From(err).Code != "request.invalid_page_token" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.Encode("s", make(chan int)); err == nil {
		t.Fatal("unencodable cursor accepted")
	}
}

func TestKeyset(t *testing.T) {
	c, _ := NewCodec([]byte(strings.Repeat("k", 32)))
	sort := Sort{Default: "code", Fields: []Field{{Name: "code", Column: "code", Type: "text"}, {Name: "created_at", Column: "created_at", Type: "timestamptz"}}}
	for _, bad := range []string{"name", "code sideways", "created_at desc extra"} {
		if _, err := c.Keyset(sort, "s", Request{OrderBy: bad}); problem.From(err).Code != "request.invalid_order_by" {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := c.Keyset(sort, "s", Request{PageSize: 0, PageSizeSet: true}); err == nil {
		t.Error("page size 0 accepted")
	}
	k, err := c.Keyset(sort, "s", Request{OrderBy: "  created_at   desc ", PageSize: 2, PageSizeSet: true})
	if err != nil || k.Field() != "created_at" {
		t.Fatal(k, err)
	}
	q, args := k.SQL("SELECT * FROM t WHERE a = $1", []any{"x"})
	if q != "SELECT * FROM t WHERE a = $1 ORDER BY created_at DESC, id DESC LIMIT $2" || len(args) != 2 || args[1] != 3 {
		t.Fatalf("%s %v", q, args)
	}
	type row struct {
		v  string
		id uuid.UUID
	}
	rows := []row{{"c", uuid.New()}, {"b", uuid.New()}, {"a", uuid.New()}}
	page, tok := Page(k, rows, func(r row) (string, uuid.UUID) { return r.v, r.id })
	if len(page) != 2 || tok == "" {
		t.Fatal(page, tok)
	}
	next, err := c.Keyset(sort, "s", Request{OrderBy: "created_at desc", PageToken: tok, PageTokenSet: true})
	if err != nil {
		t.Fatal(err)
	}
	q, args = next.SQL("SELECT * FROM t WHERE true", nil)
	if q != "SELECT * FROM t WHERE true AND (created_at, id) < (CAST($1::text AS timestamptz), $2) ORDER BY created_at DESC, id DESC LIMIT $3" ||
		args[0] != "b" || args[1] != rows[1].id || args[2] != DefaultSize+1 {
		t.Fatalf("%s %v", q, args)
	}
	// A token is bound to its order.
	if _, err := c.Keyset(sort, "s", Request{OrderBy: "created_at asc", PageToken: tok, PageTokenSet: true}); problem.From(err).Code != "request.invalid_page_token" {
		t.Fatal(err)
	}
	if last, tok := Page(next, rows, func(r row) (string, uuid.UUID) { return r.v, r.id }); len(last) != 3 || tok != "" {
		t.Fatal(last, tok)
	}
}
