// Package idref names the objects a page of rows references by id: a
// document's number, a house bank's code, a bank account. A response that
// returns an id also returns what its consumer knows the object by
// (REVIEW.md, API-consumer classes: an identifier without the name or
// reference its consumer needs; ADR-0038 As built), read from the context
// that holds the objects in one call per page (ADR-0051 §2 for a context
// reading another through its port). It began in payments and moved here
// when the cash context's memo records needed it too.
package idref

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Kind is a referenced object's kind and the context that holds it, as an
// error names an object missing from it.
type Kind struct{ Name, Context string }

// Of are the ids rows reference, ref giving a row's or nil, in the rows'
// order.
func Of[T any](rows []T, ref func(T) *uuid.UUID) []uuid.UUID {
	var ids []uuid.UUID
	for _, row := range rows {
		if id := ref(row); id != nil {
			ids = append(ids, *id)
		}
	}
	return ids
}

// Resolve gives what names the objects ids (of the kind) from lookup, the
// context that holds them: one call for the page and none for a page that
// references none. An object the context does not hold is an error (the
// response fails, 500), never a row shown without its name.
func Resolve[V any](ctx context.Context, lookup func(context.Context, []uuid.UUID) (map[uuid.UUID]V, error), ids []uuid.UUID,
	kind Kind) (map[uuid.UUID]V, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]V{}, nil
	}
	named, err := lookup(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := named[id]; !ok {
			return nil, fmt.Errorf("%s %s is not held by the %s context", kind.Name, id, kind.Context)
		}
	}
	return named, nil
}
