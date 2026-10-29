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

// requestPort and itemPort are DownPayments and OpenItems under other
// names, so requests and entries can embed them.
type (
	requestPort = DownPayments
	itemPort    = OpenItems
)

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

// entries is a ledger context that holds the journal entries numbered, and
// fails when told to; it answers nothing else.
type entries struct {
	itemPort
	numbers map[uuid.UUID]string
	err     error
}

func (e entries) Numbers(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return e.numbers, e.err
}

// TestDocumentNumbers: a row is never shown without the number of a
// document it references. A document its context does not hold fails the
// page rather than leave its number out, as the context's failure does; a
// page that references none asks nothing.
func TestDocumentNumbers(t *testing.T) {
	known, unknown := uuid.New(), uuid.New()
	failed := errors.New("payables down")
	for name, c := range map[string]struct {
		requests requests
		kind     documentKind
		ids      []uuid.UUID
		want     string // the error
		number   string // the known document's number
	}{
		"every document numbered": {requests{numbers: map[uuid.UUID]string{known: "DP1"}}, downPaymentRequest, []uuid.UUID{known}, "", "DP1"},
		"none referenced":         {requests{err: failed}, downPaymentRequest, nil, "", ""},
		"a document unknown":      {requests{numbers: map[uuid.UUID]string{known: "DP1"}}, downPaymentRequest, []uuid.UUID{known, unknown}, "down payment request " + unknown.String() + " is not held by the payables context", ""},
		"the context failing":     {requests{err: failed}, downPaymentRequest, []uuid.UUID{known}, failed.Error(), ""},
		"a journal entry unknown": {requests{numbers: map[uuid.UUID]string{}}, journalEntry, []uuid.UUID{unknown}, "journal entry " + unknown.String() + " is not held by the ledger context", ""},
	} {
		t.Run(name, func(t *testing.T) {
			numbers, err := documentNumbers(context.Background(), c.requests.Numbers, c.ids, c.kind)
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

// TestPaymentNumbers: a page's payments are given the numbers of the
// requests they pay and of their items' journal entries, from each
// context in turn; either failing, or not holding a document, fails the
// page.
func TestPaymentNumbers(t *testing.T) {
	request, entry, unknown := uuid.New(), uuid.New(), uuid.New()
	page := []Payment{{ID: uuid.New(), Kind: KindDownPayment, RequestID: &request}, {ID: uuid.New(), Kind: KindItems, Items: []Item{{EntryID: entry}}}}
	held := requests{numbers: map[uuid.UUID]string{request: "DP1"}}
	ledgerHolds := entries{numbers: map[uuid.UUID]string{entry: "5100000001"}}
	payablesDown, ledgerDown := errors.New("payables down"), errors.New("ledger down")
	for name, c := range map[string]struct {
		requests requests
		entries  entries
		page     []Payment
		want     string // the error
	}{
		"both numbered":        {held, ledgerHolds, page, ""},
		"the payables failing": {requests{err: payablesDown}, ledgerHolds, page, payablesDown.Error()},
		"the ledger failing":   {held, entries{err: ledgerDown}, page, ledgerDown.Error()},
		"an entry unknown": {held, ledgerHolds, append(page[:2:2], Payment{Items: []Item{{EntryID: unknown}}}),
			"journal entry " + unknown.String() + " is not held by the ledger context"},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{DownPayments: c.requests, OpenItems: c.entries}
			numbers, err := e.paymentNumbers(context.Background(), c.page)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Fatalf("error: got %q, want %q", got, c.want)
			}
			if err != nil {
				return
			}
			if numbers[request] != "DP1" {
				t.Errorf("the request's number: got %q, want DP1", numbers[request])
			}
			if numbers[entry] != "5100000001" {
				t.Errorf("the entry's number: got %q, want 5100000001", numbers[entry])
			}
		})
	}
}
