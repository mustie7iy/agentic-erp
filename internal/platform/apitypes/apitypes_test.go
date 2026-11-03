package apitypes_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

func TestPtr(t *testing.T) {
	if got := apitypes.Ptr[string](gen.OptString{}); got != nil {
		t.Errorf("absent: got %v, want nil", *got)
	}
	if got := apitypes.Ptr[string](gen.NewOptString("x")); got == nil || *got != "x" {
		t.Errorf("present: got %v, want a pointer to x", got)
	}
}

func TestOptUUID(t *testing.T) {
	if got := apitypes.OptUUID(nil); got.Set {
		t.Errorf("nil: got %v, want unset", got)
	}
	id := uuid.New()
	if got := apitypes.OptUUID(&id); got != gen.NewOptUUID(id) {
		t.Errorf("value: got %v, want %v", got, id)
	}
}

func TestOptDate(t *testing.T) {
	if got := apitypes.OptDate(nil); got.Set {
		t.Errorf("nil: got %v, want unset", got)
	}
	d := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	if got := apitypes.OptDate(&d); got != gen.NewOptDate(d) {
		t.Errorf("value: got %v, want %v", got, d)
	}
}

func TestOptInt32(t *testing.T) {
	if got := apitypes.OptInt32(nil); got.Set {
		t.Errorf("nil: got %v, want unset", got)
	}
	n := int32(3)
	if got := apitypes.OptInt32(&n); got != gen.NewOptInt32(3) {
		t.Errorf("value: got %v, want 3", got)
	}
}

func TestOptString(t *testing.T) {
	if got := apitypes.OptString(nil); got.Set {
		t.Errorf("nil: got %v, want unset", got)
	}
	s := "x"
	if got := apitypes.OptString(&s); got != gen.NewOptString("x") {
		t.Errorf("value: got %v, want x", got)
	}
}

func TestOptNilString(t *testing.T) {
	if got := apitypes.OptNilString(gen.OptNilString{}); got != nil {
		t.Errorf("absent: got %v, want nil", got)
	}
	if got := apitypes.OptNilString(gen.OptNilString{Set: true, Null: true}); got == nil || *got != nil {
		t.Errorf("null: got %v, want a pointer to nil", got)
	}
	if got := apitypes.OptNilString(gen.NewOptNilString("x")); got == nil || *got == nil || **got != "x" {
		t.Errorf("value: got %v, want a pointer to a pointer to x", got)
	}
}

func TestOptDates(t *testing.T) {
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if got := apitypes.OptDate(nil); got.Set {
		t.Errorf("OptDate(nil): got %v, want unset", got)
	}
	if got := apitypes.OptDate(&day); !got.Set || !got.Value.Equal(day) {
		t.Errorf("OptDate(day): got %v, want %s", got, day)
	}
	if got := apitypes.OptNilDate(gen.OptNilDate{}); got != nil {
		t.Errorf("absent: got %v, want nil", got)
	}
	if got := apitypes.OptNilDate(gen.OptNilDate{Set: true, Null: true}); got == nil || *got != nil {
		t.Errorf("null: got %v, want a pointer to nil", got)
	}
	if got := apitypes.OptNilDate(gen.NewOptNilDate(day)); got == nil || *got == nil || !(*got).Equal(day) {
		t.Errorf("a date: got %v, want %s", got, day)
	}
}

func TestObjects(t *testing.T) {
	if got := apitypes.DecodeObject(nil); got != nil {
		t.Errorf("from nil: got %v, want nil", got)
	}
	x := gen.Extensions{"n": jx.Raw(`1`), "o": jx.Raw(`{"k":"v"}`), "z": jx.Raw(`null`)}
	want := map[string]any{"n": 1.0, "o": map[string]any{"k": "v"}, "z": nil}
	got := apitypes.DecodeObject(x)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("from: got %v, want %v", got, want)
	}
	if back := apitypes.EncodeObject(got); !reflect.DeepEqual(back, map[string]jx.Raw{"n": jx.Raw(`1`), "o": jx.Raw(`{"k":"v"}`), "z": jx.Raw(`null`)}) {
		t.Errorf("round trip: got %v, want %v", back, x)
	}
	if got := apitypes.EncodeObject(nil); got == nil || len(got) != 0 {
		t.Errorf("to from nil: got %v, want an empty object", got)
	}
}

func TestMoney(t *testing.T) {
	if got := apitypes.Money(-123456, "EUR", 2); got != (gen.Money{Amount: "-1234.56", Currency: "EUR"}) {
		t.Errorf("got %v, want -1234.56 EUR", got)
	}
}

func TestPercent(t *testing.T) {
	for _, c := range []struct {
		name        string
		part, whole money.Minor
		signed, abs string // "" for none
	}{
		{"a fifth", 200, 1000, "20.00", "20.00"},
		{"signs differ", 200, -1000, "-20.00", "20.00"},
		{"both negative", -1, -3, "33.33", "33.33"},
		{"a twentieth of a hundredth", 1, 200000, "0.00", "0.00"},
		{"a third, negative", -1, 3, "-33.33", "33.33"},
		{"below half a hundredth, negative", -1, 100000, "0.00", "0.00"},
		{"half a hundredth, negative, away from zero", -1, 20000, "-0.01", "0.01"},
		{"of nothing", 5, 0, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := func(d gen.OptDecimal) string { return string(d.Or("")) }
			if got := text(apitypes.Percent(c.part, c.whole)); got != c.signed {
				t.Errorf("Percent(%d, %d): got %q, want %q", c.part, c.whole, got, c.signed)
			}
			if got := text(apitypes.AbsPercent(c.part, c.whole)); got != c.abs {
				t.Errorf("AbsPercent(%d, %d): got %q, want %q", c.part, c.whole, got, c.abs)
			}
		})
	}
}

func TestPage(t *testing.T) {
	got := apitypes.Page(gen.NewOptString("name"), gen.NewOptInt32(20), gen.NewOptString("tok"))
	want := pagination.Request{OrderBy: "name", PageSize: 20, PageSizeSet: true, PageToken: "tok", PageTokenSet: true}
	if got != want {
		t.Errorf("set: got %+v, want %+v", got, want)
	}
	if got := apitypes.Page(gen.OptString{}, gen.OptInt32{}, gen.OptString{}); got != (pagination.Request{}) {
		t.Errorf("unset: got %+v, want the zero request", got)
	}
}

// TestPageOf: rows past the page are dropped and give the next page's
// token; the last page has none and an empty one still has items.
func TestPageOf(t *testing.T) {
	codec, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := codec.Keyset(pagination.Sort{Default: "n", Fields: []pagination.Field{{Name: "n", Column: "n", Type: "text"}}},
		"test", pagination.Request{PageSize: 2, PageSizeSet: true})
	if err != nil {
		t.Fatal(err)
	}
	key := func(s string) (string, uuid.UUID) { return s, uuid.Nil }
	upper := strings.ToUpper
	cases := []struct {
		name     string
		rows     []string
		want     []string
		wantNext bool
	}{
		{"more follow", []string{"a", "b", "c"}, []string{"A", "B"}, true},
		{"last page", []string{"a"}, []string{"A"}, false},
		{"empty", nil, []string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, next := apitypes.PageOf(ks, c.rows, key, upper)
			if !reflect.DeepEqual(items, c.want) {
				t.Errorf("items: got %#v, want %#v", items, c.want)
			}
			if next.Set != c.wantNext {
				t.Errorf("next page token set: got %t, want %t", next.Set, c.wantNext)
			}
		})
	}
}

// TestPageWith: what the rows reference is read for the page's rows
// alone, never the row past the page, and shown with each; its failure
// fails the page.
func TestPageWith(t *testing.T) {
	codec, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := codec.Keyset(pagination.Sort{Default: "n", Fields: []pagination.Field{{Name: "n", Column: "n", Type: "text"}}},
		"test", pagination.Request{PageSize: 2, PageSizeSet: true})
	if err != nil {
		t.Fatal(err)
	}
	key := func(s string) (string, uuid.UUID) { return s, uuid.Nil }
	show := func(s, refs string) string { return s + "@" + refs }
	t.Run("named", func(t *testing.T) {
		items, next, err := apitypes.PageWith(ks, []string{"a", "b", "c"}, key, func(rows []string) (string, error) { return strings.Join(rows, ""), nil }, show)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"a@ab", "b@ab"}; !reflect.DeepEqual(items, want) {
			t.Errorf("items: got %#v, want %#v", items, want)
		}
		if !next.Set {
			t.Errorf("next page token set: got %t, want true", next.Set)
		}
	})
	t.Run("last page", func(t *testing.T) {
		_, next, err := apitypes.PageWith(ks, []string{"a"}, key, func([]string) (string, error) { return "", nil }, show)
		if err != nil || next.Set {
			t.Errorf("got next %v and error %v, want neither", next, err)
		}
	})
	t.Run("refs failing", func(t *testing.T) {
		failed := errors.New("context down")
		if _, _, err := apitypes.PageWith(ks, []string{"a"}, key, func([]string) (string, error) { return "", failed }, show); !errors.Is(err, failed) {
			t.Errorf("error: got %v, want %v", err, failed)
		}
	})
}

func TestETagOf(t *testing.T) {
	v := struct{ A string }{"x"}
	if got := apitypes.ETagOf(v); got != gen.NewOptString(etag.Of(&v)) {
		t.Errorf("got %v, want the representation's tag", got)
	}
}
