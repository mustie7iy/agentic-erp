package payments

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// payeePort is Payees under another name, so partners can embed it.
type payeePort = Payees

// partners is a partner context that holds the partners named and the
// bank accounts given, and fails when told to (err for names, accountsErr
// for bank accounts); it answers nothing else.
type partners struct {
	payeePort
	names       map[uuid.UUID]partner.Named
	err         error
	accounts    map[uuid.UUID]partner.BankAccount
	accountsErr error
}

func (p partners) Names(context.Context, []uuid.UUID) (map[uuid.UUID]partner.Named, error) {
	return p.names, p.err
}

func (p partners) BankAccounts(context.Context, []uuid.UUID) (map[uuid.UUID]partner.BankAccount, error) {
	return p.accounts, p.accountsErr
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
		"every supplier named":        {partners{names: map[uuid.UUID]partner.Named{known: {Number: "BP1", Name: "Tyrell"}}}, pays[:1], ""},
		"a supplier unknown":          {partners{names: map[uuid.UUID]partner.Named{known: {Number: "BP1", Name: "Tyrell"}}}, pays, "supplier " + unknown.String() + " of payment " + pays[1].ID.String() + " is not a business partner"},
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
			if err == nil && names[known].Name != "Tyrell" {
				t.Errorf("name: got %q, want Tyrell", names[known].Name)
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

// TestPaymentNumbers: a page's payments are given the numbers of the
// requests they pay, of their postings and of their items' journal
// entries, from each context in turn; either failing, or not holding a
// document, fails the page.
func TestPaymentNumbers(t *testing.T) {
	request, entry, posting, unknown := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	page := []Payment{{ID: uuid.New(), Kind: KindDownPayment, RequestID: &request}, {ID: uuid.New(), Kind: KindItems, Items: []Item{{EntryID: entry}}, JournalEntryID: &posting}}
	held := requests{numbers: map[uuid.UUID]string{request: "DP1"}}
	ledgerHolds := entries{numbers: map[uuid.UUID]string{entry: "5100000001", posting: "1500000001"}}
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
		"a posting unknown": {held, ledgerHolds, append(page[:2:2], Payment{JournalEntryID: &unknown}),
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
			if numbers[posting] != "1500000001" {
				t.Errorf("the posting's number: got %q, want 1500000001", numbers[posting])
			}
		})
	}
}

// TestPaymentRefs: a page's payments are given their suppliers' names,
// their documents' numbers, their bank accounts and their house banks'
// codes; any of the contexts failing, or not holding what a payment
// references, fails the page. A page whose payments name no house bank,
// in a run without one, asks for none; a page whose bank accounts are not
// shown (T45e) reads none, so the partner context's accounts failing does
// not matter to it.
func TestPaymentRefs(t *testing.T) {
	supplier, account, unknown := uuid.New(), uuid.New(), uuid.New()
	page := []Payment{{ID: uuid.New(), SupplierID: supplier, BankAccountID: account}}
	held := partners{names: map[uuid.UUID]partner.Named{supplier: {Number: "BP1", Name: "Tyrell"}}, accounts: map[uuid.UUID]partner.BankAccount{account: {ID: account, AccountLast4: "4321"}}}
	partnerDown, ledgerDown := errors.New("partner down"), errors.New("ledger down")
	bank, posting := uuid.New(), uuid.New()
	posted := []Payment{{ID: uuid.New(), SupplierID: supplier, BankAccountID: account, JournalEntryID: &posting}}
	for name, c := range map[string]struct {
		partners partners
		page     []Payment
		run      Run
		hidden   bool   // the bank accounts not shown
		want     string // the error
	}{
		"every reference named":        {held, page, Run{}, false, ""},
		"the suppliers' names failing": {partners{err: partnerDown}, page, Run{}, false, partnerDown.Error()},
		"the ledger failing":           {held, posted, Run{}, false, ledgerDown.Error()},
		"the bank accounts failing":    {partners{names: held.names, accountsErr: partnerDown}, page, Run{}, false, partnerDown.Error()},
		"a bank account unknown": {partners{names: held.names, accounts: map[uuid.UUID]partner.BankAccount{unknown: {}}}, page, Run{}, false,
			"bank account " + account.String() + " is not held by the partner context"},
		"the bank accounts not shown":       {partners{names: held.names, accountsErr: partnerDown}, page, Run{}, true, ""},
		"the house banks outside a request": {held, page, Run{HouseBankID: &bank}, false, reqctx.ErrOutsideRequest.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{Payees: c.partners, DownPayments: requests{}, OpenItems: entries{err: ledgerDown}}
			refs, err := e.paymentRefs(context.Background(), c.run, c.page, !c.hidden)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Fatalf("error: got %q, want %q", got, c.want)
			}
			switch {
			case err != nil:
			case c.hidden && refs.accounts != nil:
				t.Errorf("the bank accounts: got %v, want none read", refs.accounts)
			case !c.hidden && refs.accounts[account].AccountLast4 != "4321":
				t.Errorf("the bank account: got %+v, want its last4 4321", refs.accounts[account])
			}
		})
	}
}

// TestRefuse: a release refused because of a supplier's payment names the
// supplier by number and name, with its id; the partner context failing,
// or not holding the supplier, fails the release instead of a refusal
// that leaves the supplier unnamed.
func TestRefuse(t *testing.T) {
	supplier := uuid.New()
	down := errors.New("partner down")
	detail := func(s string) string { return "The payment to supplier " + s + " is refused." }
	for name, c := range map[string]struct {
		partners partners
		want     string // the error
	}{
		"named": {partners{names: map[uuid.UUID]partner.Named{supplier: {Number: "BP1", Name: "Tyrell"}}},
			"payments.proposal_outdated (409): The payment to supplier BP1 Tyrell (" + supplier.String() + ") is refused."},
		"the partner context failing": {partners{err: down}, "payments: name supplier " + supplier.String() + ": partner down"},
		"a supplier unknown": {partners{names: map[uuid.UUID]partner.Named{}},
			"payments: name supplier " + supplier.String() + ": supplier " + supplier.String() + " is not held by the partner context"},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{Payees: c.partners}
			if got := e.refuse(context.Background(), "payments.proposal_outdated", supplier, detail).Error(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestNameTooLarge: a proposal failing on one supplier's totals names the
// supplier by number and name, with its id, as a release's refusals do;
// the partner context failing is an error (the job is retried), a supplier
// it does not hold still fails the run (a problem) naming it by id, and
// any other error passes unchanged.
func TestNameTooLarge(t *testing.T) {
	supplier := uuid.New()
	down, other := errors.New("partner down"), errors.New("something else")
	named := partners{names: map[uuid.UUID]partner.Named{supplier: {Number: "BP1", Name: "Tyrell"}}}
	for name, c := range map[string]struct {
		partners partners
		err      error
		want     string
	}{
		"named": {named, supplierTooLarge{supplier},
			"payments.amount_too_large (422): The open items of supplier BP1 Tyrell (" + supplier.String() + ") add up to more than an amount can hold."},
		"wrapped": {named, fmt.Errorf("propose: %w", supplierTooLarge{supplier}),
			"payments.amount_too_large (422): The open items of supplier BP1 Tyrell (" + supplier.String() + ") add up to more than an amount can hold."},
		"the partner context failing": {partners{err: down}, supplierTooLarge{supplier}, "payments: name supplier " + supplier.String() + ": partner down"},
		"a supplier unknown": {partners{names: map[uuid.UUID]partner.Named{}}, supplierTooLarge{supplier},
			"payments.amount_too_large (422): The open items of supplier " + supplier.String() + " add up to more than an amount can hold."},
		"another error": {partners{err: down}, other, "something else"},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Endpoints{Payees: c.partners}
			if got := e.nameTooLarge(context.Background(), c.err).Error(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if got, want := (supplierTooLarge{supplier}).Error(), "payments: the open items of supplier "+supplier.String()+" add up to more than an amount can hold"; got != want {
		t.Errorf("unnamed: got %q, want %q", got, want)
	}
}
