package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// payeePort is Payees under another name, so partners can embed it.
type payeePort = Payees

// partners is a partner context that holds the partners named, and fails
// when told to; it answers nothing else.
type partners struct {
	payeePort
	names map[uuid.UUID]string
	err   error
}

func (p partners) Names(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return p.names, p.err
}

// TestSupplierNames: a payment is never shown without its payee. A
// supplier the partner context does not hold fails the page rather than
// leave its name out, as the partner context's failure does.
func TestSupplierNames(t *testing.T) {
	known, unknown := uuid.New(), uuid.New()
	pays := []Payment{{ID: uuid.New(), SupplierID: known}, {ID: uuid.New(), SupplierID: unknown}}
	failed := errors.New("partner down")
	for name, c := range map[string]struct {
		partners partners
		pays     []Payment
		want     string
	}{
		"every supplier named":        {partners{names: map[uuid.UUID]string{known: "Tyrell"}}, pays[:1], ""},
		"a supplier unknown":          {partners{names: map[uuid.UUID]string{known: "Tyrell"}}, pays, "supplier " + unknown.String() + " of payment " + pays[1].ID.String() + " is not a business partner"},
		"the partner context failing": {partners{err: failed}, pays[:1], failed.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{Payees: c.partners}
			names, err := e.supplierNames(context.Background(), c.pays)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Fatalf("error: got %q, want %q", got, c.want)
			}
			if err == nil && names[known] != "Tyrell" {
				t.Errorf("name: got %q, want Tyrell", names[known])
			}
		})
	}
}
