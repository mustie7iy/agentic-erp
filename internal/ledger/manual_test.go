package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// manualEntries is the approvals handler the API wires.
func manualEntries() ledger.ManualEntries {
	return ledger.ManualEntries{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
}

// submitted parks an entry and posts it as its owner through an approval
// policy that opens request; it returns the entry's id.
func (e env) submitted(t *testing.T, request uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.MustParse(e.parked(t)[len("/v1/parked-journal-entries/"):])
	h := endpoints()
	h.Approvals = opener{request, true}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := h.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: id, IfMatch: "*"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// journalEntriesOf counts the journal entries posted for a parked entry.
func (e env) journalEntriesOf(t *testing.T, id uuid.UUID) int {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/journal-entries?source_context=ledger&source_document_id="+id.String(), "", "")
	return len(page["items"].([]any))
}

// TestManualEntryCallbacks: the handler acts only on the request the
// entry waits for, so a second callback for the same request, or one for
// another request, changes nothing and fails the decision.
func TestManualEntryCallbacks(t *testing.T) {
	e := setup(t)
	m := manualEntries()
	call := func(fn func(ctx context.Context) error) error {
		return apitest.Committed(t, e.ten, e.ten.Human, fn)
	}
	request := uuid.New()
	id := e.submitted(t, request)
	for name, fn := range map[string]func(ctx context.Context) error{
		"approved for another request":  func(ctx context.Context) error { return m.Approved(ctx, id, uuid.New()) },
		"rejected for another request":  func(ctx context.Context) error { return m.Rejected(ctx, id, uuid.New(), "c") },
		"cancelled for another request": func(ctx context.Context) error { return m.Cancelled(ctx, id, uuid.New(), "c") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(fn); err == nil {
				t.Errorf("%s: got no error, want the decision refused", name)
			}
		})
	}
	if err := call(func(ctx context.Context) error { return m.Approved(ctx, id, request) }); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if err := call(func(ctx context.Context) error { return m.Approved(ctx, id, request) }); err == nil {
		t.Error("approved again: got no error, want the decision refused")
	}
	if n := e.journalEntriesOf(t, id); n != 1 {
		t.Errorf("journal entries after two approvals: got %d, want 1", n)
	}
}

// TestPostParkedWithinTheLimit: when the policy opens no request the entry
// is posted at once, as the journal entry posted for it.
func TestPostParkedWithinTheLimit(t *testing.T) {
	e := setup(t)
	id := uuid.MustParse(e.parked(t)[len("/v1/parked-journal-entries/"):])
	h := endpoints()
	h.Approvals = opener{uuid.Nil, false}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := h.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: id, IfMatch: "*"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := e.journalEntriesOf(t, id); n != 1 {
		t.Errorf("journal entries: got %d, want 1", n)
	}
}

// recorder is an approval policy that records what it was asked and
// answers err.
type recorder struct {
	asked *approvals.NewRequest
	err   error
}

func (r recorder) Open(_ context.Context, in approvals.NewRequest) (uuid.UUID, bool, error) {
	*r.asked = in
	return uuid.New(), true, r.err
}

func (r recorder) Required(_ context.Context, in approvals.NewRequest) (bool, error) {
	*r.asked = in
	return true, r.err
}

// TestPostParkedAsksThePolicy: the entry is measured by its debits in its
// currency on its posting date, against the limit of its own legal
// entity's company, and approvers see what it is; a policy that cannot
// answer refuses the posting.
func TestPostParkedAsksThePolicy(t *testing.T) {
	e := setup(t)
	path := e.parked(t)
	e.Must(200, "PATCH", path, `{"header_text": "Accrual"}`, `"1"`)
	id := uuid.MustParse(path[len("/v1/parked-journal-entries/"):])
	var asked approvals.NewRequest
	h := endpoints()
	h.Approvals = recorder{asked: &asked}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := h.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: id, IfMatch: "*"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for field, c := range map[string][2]any{
		"object type":  {asked.ObjectType, approvals.ManualEntry},
		"object":       {asked.ObjectID, id},
		"title":        {asked.Title, "Manual entry SA of 2026-03-15, 10.00 EUR: Accrual"},
		"amount":       {*asked.Amount, money.Minor(1000)},
		"currency":     {*asked.Currency, "EUR"},
		"legal entity": {asked.LegalEntityID, e.de},
		"company":      {asked.Company, "2000"},
		"date":         {asked.Date.Format(time.DateOnly), "2026-03-15"},
	} {
		if c[0] != c[1] {
			t.Errorf("request %s: got %v, want %v", field, c[0], c[1])
		}
	}
	refused := errors.New("no policy")
	h.Approvals = recorder{asked: &asked, err: refused}
	path = e.parked(t)
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := h.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: uuid.MustParse(path[len("/v1/parked-journal-entries/"):]), IfMatch: "*"})
		return err
	}); !errors.Is(err, refused) {
		t.Errorf("posting without a policy: got %v, want %v", err, refused)
	}
}

// TestDirectPostingAsksThePolicy: a direct posting the policy would send
// for approval is refused, measured as :post measures it.
func TestDirectPostingAsksThePolicy(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	var asked approvals.NewRequest
	h := endpoints()
	h.Approvals = recorder{asked: &asked}
	body := gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"), Currency: "EUR",
		Lines: []gen.JournalLineInput{{AccountID: uuid.MustParse(bank), Amount: gen.Money{Amount: "12.50", Currency: "EUR"}},
			{AccountID: uuid.MustParse(revenue), Amount: gen.Money{Amount: "-12.50", Currency: "EUR"}}}}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := h.PostJournalEntry(ctx, &body, gen.PostJournalEntryParams{})
		return err
	})
	if got := problem.From(err).Code; got != "ledger.approval_required" {
		t.Errorf("direct posting above the limit: got %v, want ledger.approval_required", err)
	}
	if *asked.Amount != 1250 {
		t.Errorf("measured debits: got %d, want 1250", *asked.Amount)
	}
	if asked.Company != "2000" {
		t.Errorf("measured against company: got %s, want 2000", asked.Company)
	}
}
