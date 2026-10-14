package apitypes_test

import (
	"reflect"
	"testing"

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
	if got := apitypes.Money(money.Minor(-123456), "EUR", 2); got != (gen.Money{Amount: "-1234.56", Currency: "EUR"}) {
		t.Errorf("got %v, want -1234.56 EUR", got)
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

func TestETagOf(t *testing.T) {
	v := struct{ A string }{"x"}
	if got := apitypes.ETagOf(v); got != gen.NewOptString(etag.Of(&v)) {
		t.Errorf("got %v, want the representation's tag", got)
	}
}
