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

// TestSupplierNames: a payment, or an exclusion, is never shown without
// its supplier's name. A supplier the partner context does not hold fails
// the page rather than leave its name out, as the partner context's
// failure does.
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
			names, err := supplierNames(context.Background(), c.partners, c.pays, func(p Payment) (uuid.UUID, string) { return p.SupplierID, "payment " + p.ID.String() })
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

// requestPort is DownPayments under another name, so requests can embed it.
type requestPort = DownPayments

// requests is a payables context that holds the requests numbered, and
// fails when told to; it answers nothing else.
type requests struct {
	requestPort
	numbers map[uuid.UUID]string
	err     error
}

func (r requests) Numbers(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return r.numbers, r.err
}

// TestRequestNumbers: a request's exclusion is never shown without its
// request's number. A request the payables context does not hold fails the
// page rather than leave its number out, as the payables context's failure
// does; a page of suppliers' exclusions alone asks nothing.
func TestRequestNumbers(t *testing.T) {
	known, unknown := uuid.New(), uuid.New()
	page := []Exclusion{{SupplierID: uuid.New()}, {SupplierID: uuid.New(), RequestID: &known}, {SupplierID: uuid.New(), RequestID: &unknown}}
	failed := errors.New("payables down")
	for name, c := range map[string]struct {
		requests requests
		page     []Exclusion
		want     string // the error
		number   string // the known request's number
	}{
		"every request numbered":       {requests{numbers: map[uuid.UUID]string{known: "DP1"}}, page[:2], "", "DP1"},
		"suppliers' exclusions alone":  {requests{err: failed}, page[:1], "", ""},
		"a request unknown":            {requests{numbers: map[uuid.UUID]string{known: "DP1"}}, page, "down payment request " + unknown.String() + " is not held by the payables context", ""},
		"the payables context failing": {requests{err: failed}, page[:2], failed.Error(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			numbers, err := requestNumbers(context.Background(), c.requests, c.page)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Fatalf("error: got %q, want %q", got, c.want)
			}
			if numbers[known] != c.number {
				t.Errorf("number: got %q, want %q", numbers[known], c.number)
			}
		})
	}
}
