package procurement

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// entitiesOf is the tenancy context as a test sets it.
type entitiesOf struct {
	entities map[uuid.UUID]tenancy.Entity
	err      error
}

func (e entitiesOf) Entity(_ context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	le, ok := e.entities[id]
	return le, ok, e.err
}

func (entitiesOf) OrgUnit(context.Context, uuid.UUID) (tenancy.Unit, bool, error) {
	return tenancy.Unit{}, false, nil
}

// namesOf is the partner context's names as a test sets them.
type namesOf struct {
	names map[uuid.UUID]partner.Named
	err   error
}

func (n namesOf) Names(context.Context, []uuid.UUID) (map[uuid.UUID]partner.Named, error) {
	return n.names, n.err
}

// TestEventNames: a contract's event names its company by code and its
// supplier by number and name; a legal entity or supplier its context does
// not hold, or the context failing, fails the round instead of an event
// that leaves it unnamed.
func TestEventNames(t *testing.T) {
	c := Contract{ID: uuid.New(), LegalEntityID: uuid.New(), SupplierID: uuid.New()}
	down := errors.New("down")
	held := entitiesOf{entities: map[uuid.UUID]tenancy.Entity{c.LegalEntityID: {Code: "2000"}}}
	named := namesOf{names: map[uuid.UUID]partner.Named{c.SupplierID: {Number: "BP1", Name: "Tyrell"}}}
	for name, tc := range map[string]struct {
		entities entitiesOf
		names    namesOf
		want     string // the error
	}{
		"named":                       {held, named, ""},
		"the tenancy context failing": {entitiesOf{err: down}, named, "procurement: name legal entity " + c.LegalEntityID.String() + " of contract " + c.ID.String() + ": down"},
		"a legal entity unknown": {entitiesOf{}, named,
			"procurement: name legal entity " + c.LegalEntityID.String() + " of contract " + c.ID.String() + ": not held by the tenancy context"},
		"the partner context failing": {held, namesOf{err: down}, "procurement: name the supplier of contract " + c.ID.String() + ": down"},
		"a supplier unknown": {held, namesOf{names: map[uuid.UUID]partner.Named{}},
			"procurement: name the supplier of contract " + c.ID.String() + ": supplier " + c.SupplierID.String() + " is not held by the partner context"},
	} {
		t.Run(name, func(t *testing.T) {
			x := &ContractExpirer{LegalEntities: tc.entities, Partners: tc.names}
			payload := map[string]any{}
			got := ""
			if err := x.names().name(context.Background(), c, payload); err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("error: got %q, want %q", got, tc.want)
			}
			if tc.want != "" {
				return
			}
			for field, want := range map[string]string{"legal_entity_code": "2000", "supplier_number": "BP1", "supplier_name": "Tyrell"} {
				if payload[field] != want {
					t.Errorf("%s: got %v, want %s", field, payload[field], want)
				}
			}
		})
	}
}
