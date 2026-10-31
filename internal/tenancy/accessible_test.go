package tenancy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// reader is an EntityReader answering from a map, or failing with err.
type reader struct {
	entities map[uuid.UUID]tenancy.Entity
	err      error
}

func (r reader) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	e, ok := r.entities[id]
	return e, ok, r.err
}

// TestAccessible: an existing legal entity the caller may act in is
// returned; a missing one and one outside the caller's access are the same
// reference_not_found on the field; a read failure passes through.
func TestAccessible(t *testing.T) {
	known, other := uuid.New(), uuid.New()
	r := reader{entities: map[uuid.UUID]tenancy.Entity{known: {ID: known, Code: "DE01"}, other: {ID: other}}}
	access := authz.Access{IDs: []uuid.UUID{known}}

	t.Run("accessible", func(t *testing.T) {
		le, err := tenancy.Accessible(context.Background(), r, access, known, "legal_entity_id")
		if err != nil {
			t.Fatalf("got %v, want no error", err)
		}
		if le.Code != "DE01" {
			t.Errorf("got %s, want DE01", le.Code)
		}
	})
	for name, id := range map[string]uuid.UUID{"missing": uuid.New(), "not accessible": other} {
		t.Run(name, func(t *testing.T) {
			_, err := tenancy.Accessible(context.Background(), r, access, id, "lines[0].legal_entity_id")
			var p *problem.Problem
			if !errors.As(err, &p) {
				t.Fatalf("got %v, want a problem", err)
			}
			want := problem.ReferenceNotFound("lines[0].legal_entity_id", "legal entity", "no legal entity with this id that you can access")
			if p.Code != want.Code {
				t.Errorf("code: got %s, want %s", p.Code, want.Code)
			}
			if p.Detail != want.Detail {
				t.Errorf("detail: got %q, want %q", p.Detail, want.Detail)
			}
			if len(p.Errors) != 1 || p.Errors[0] != want.Errors[0] {
				t.Errorf("field errors: got %+v, want %+v", p.Errors, want.Errors)
			}
		})
	}
	boom := errors.New("connection lost")
	if _, err := tenancy.Accessible(context.Background(), reader{err: boom}, access, known, "legal_entity_id"); !errors.Is(err, boom) {
		t.Errorf("read failure: got %v, want %v", err, boom)
	}
}

// TestCompanyCode: the adapter gives a known legal entity's company code,
// found false for one the reader does not know, and a read failure.
func TestCompanyCode(t *testing.T) {
	known := uuid.New()
	codeOf := tenancy.CompanyCode(reader{entities: map[uuid.UUID]tenancy.Entity{known: {ID: known, Code: "DE01"}}})
	if code, found, err := codeOf(context.Background(), known); err != nil || !found || code != "DE01" {
		t.Errorf("known: got %q, %v, %v, want DE01, true, nil", code, found, err)
	}
	if code, found, err := codeOf(context.Background(), uuid.New()); err != nil || found || code != "" {
		t.Errorf("missing: got %q, %v, %v, want \"\", false, nil", code, found, err)
	}
	boom := errors.New("connection lost")
	if _, _, err := tenancy.CompanyCode(reader{err: boom})(context.Background(), known); !errors.Is(err, boom) {
		t.Errorf("read failure: got %v, want %v", err, boom)
	}
}
