package idref

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestResolve: a row is never shown without the name of an object it
// references. An object its context does not hold fails the page rather
// than leave its name out, as the context's failure does; a page that
// references none asks nothing.
func TestResolve(t *testing.T) {
	known, unknown := uuid.New(), uuid.New()
	failed := errors.New("payables down")
	request := Kind{Name: "down payment request", Context: "payables"}
	holds := func(named map[uuid.UUID]string, err error) func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
		return func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) { return named, err }
	}
	for name, c := range map[string]struct {
		lookup func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error)
		kind   Kind
		ids    []uuid.UUID
		want   string // the error
		number string // the known object's name
	}{
		"every object named":  {holds(map[uuid.UUID]string{known: "DP1"}, nil), request, []uuid.UUID{known}, "", "DP1"},
		"none referenced":     {holds(nil, failed), request, nil, "", ""},
		"an object unknown":   {holds(map[uuid.UUID]string{known: "DP1"}, nil), request, []uuid.UUID{known, unknown}, "down payment request " + unknown.String() + " is not held by the payables context", ""},
		"the context failing": {holds(nil, failed), request, []uuid.UUID{known}, failed.Error(), ""},
		"a house bank unknown": {holds(map[uuid.UUID]string{}, nil), Kind{Name: "house bank", Context: "payments"}, []uuid.UUID{unknown},
			"house bank " + unknown.String() + " is not held by the payments context", ""},
	} {
		t.Run(name, func(t *testing.T) {
			named, err := Resolve(context.Background(), c.lookup, c.ids, c.kind)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Fatalf("error: got %q, want %q", got, c.want)
			}
			if named[known] != c.number {
				t.Errorf("name: got %q, want %q", named[known], c.number)
			}
		})
	}
}

// TestOf: the ids rows reference, in order, a row referencing none left
// out.
func TestOf(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rows := []*uuid.UUID{&a, nil, &b}
	if got := Of(rows, func(id *uuid.UUID) *uuid.UUID { return id }); !slices.Equal(got, []uuid.UUID{a, b}) {
		t.Errorf("got %v, want %v", got, []uuid.UUID{a, b})
	}
}
