package procurement

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/partner"
)

// namesOf is the partner context's names as a test sets them.
type namesOf struct {
	names map[uuid.UUID]partner.Named
	err   error
}

func (n namesOf) Names(context.Context, []uuid.UUID) (map[uuid.UUID]partner.Named, error) {
	return n.names, n.err
}

// TestEventNames: a contract's event names its company by the code the
// round read when it dated the legal entity, and its supplier by number and
// name; a supplier the partner context does not hold, or the context
// failing, fails the round instead of an event that leaves it unnamed.
func TestEventNames(t *testing.T) {
	c := Contract{ID: uuid.New(), LegalEntityID: uuid.New(), SupplierID: uuid.New()}
	down := errors.New("down")
	named := namesOf{names: map[uuid.UUID]partner.Named{c.SupplierID: {Number: "BP1", Name: "Tyrell"}}}
	for name, tc := range map[string]struct {
		names namesOf
		want  string // the error
	}{
		"named":                       {named, ""},
		"the partner context failing": {namesOf{err: down}, "procurement: name the supplier of contract " + c.ID.String() + ": down"},
		"a supplier unknown": {namesOf{names: map[uuid.UUID]partner.Named{}},
			"procurement: name the supplier of contract " + c.ID.String() + ": supplier " + c.SupplierID.String() + " is not held by the partner context"},
	} {
		t.Run(name, func(t *testing.T) {
			x := &ContractExpirer{Partners: tc.names}
			payload := map[string]any{}
			got := ""
			if err := x.names().name(context.Background(), c, companyDay{code: "2000"}, payload); err != nil {
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
