package procurement

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// entitiesOf is the tenancy context's legal entities as a test sets them.
type entitiesOf struct {
	codes map[uuid.UUID]string
	err   error
}

func (x entitiesOf) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	code, ok := x.codes[id]
	return tenancy.Entity{ID: id, Code: code}, ok, x.err
}

func (entitiesOf) OrgUnit(context.Context, uuid.UUID) (tenancy.Unit, bool, error) {
	return tenancy.Unit{}, false, nil
}

// TestNamedEvents (review:batch28): an event names its company by code
// beside legal_entity_id and its supplier by number beside supplier_id,
// given as an id or as a pointer to one, and never by the supplier's name
// (product owner 2026-11-05); what the payload already names, or does not
// identify, is left as it is; a company or supplier the directory does
// not hold, or the directory failing, fails the write.
func TestNamedEvents(t *testing.T) {
	le, supplier := uuid.New(), uuid.New()
	down := errors.New("down")
	entities := entitiesOf{codes: map[uuid.UUID]string{le: "2000"}}
	partners := namesOf{names: map[uuid.UUID]partner.Named{supplier: {Number: "BP1", Name: "Tyrell"}}}
	for name, tc := range map[string]struct {
		entities entitiesOf
		partners namesOf
		payload  map[string]any
		want     map[string]any // the fields named
		err      string
	}{
		"company and supplier": {entities, partners, map[string]any{"legal_entity_id": le, "supplier_id": supplier},
			map[string]any{"legal_entity_code": "2000", "supplier_number": "BP1"}, ""},
		"a supplier by pointer": {entities, partners, map[string]any{"legal_entity_id": le, "supplier_id": &supplier},
			map[string]any{"legal_entity_code": "2000", "supplier_number": "BP1"}, ""},
		"no supplier suggested": {entities, partners, map[string]any{"legal_entity_id": le, "supplier_id": (*uuid.UUID)(nil)},
			map[string]any{"legal_entity_code": "2000", "supplier_number": nil}, ""},
		"already named": {entitiesOf{err: down}, namesOf{err: down},
			map[string]any{"legal_entity_id": le, "legal_entity_code": "X", "supplier_id": supplier, "supplier_number": "Y"},
			map[string]any{"legal_entity_code": "X", "supplier_number": "Y"}, ""},
		"no company": {entitiesOf{err: down}, partners, map[string]any{"rfq_id": le}, map[string]any{"legal_entity_code": nil}, ""},
		"the directory failing": {entitiesOf{err: down}, partners, map[string]any{"legal_entity_id": le}, nil,
			"procurement: name the company " + le.String() + " of an event: down"},
		"a company unknown": {entitiesOf{}, partners, map[string]any{"legal_entity_id": le}, nil,
			"procurement: name the company " + le.String() + " of an event: no such legal entity"},
		"a supplier unknown": {entities, namesOf{names: map[uuid.UUID]partner.Named{}}, map[string]any{"supplier_id": supplier}, nil,
			"procurement: name the supplier " + supplier.String() + " of an event: supplier " + supplier.String() + " is not held by the partner context"},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{LegalEntities: tc.entities, Partners: tc.partners}
			got := ""
			if err := e.nameEvent(context.Background(), tc.payload); err != nil {
				got = err.Error()
			}
			if got != tc.err {
				t.Fatalf("error: got %q, want %q", got, tc.err)
			}
			for field, want := range tc.want {
				if tc.payload[field] != want {
					t.Errorf("%s: got %v, want %v", field, tc.payload[field], want)
				}
			}
			if _, ok := tc.payload["supplier_name"]; ok {
				t.Error("supplier_name: got one, want none (product owner 2026-11-05)")
			}
		})
	}
}
