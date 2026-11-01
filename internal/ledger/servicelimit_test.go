package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// settingsOf serves a configuration document, or fails to.
type settingsOf struct {
	doc *config.Config
	err error
}

func (s settingsOf) Document(context.Context) (*config.Config, int32, error) { return s.doc, 1, s.err }

// serviceEntry is a two-line entry of legal entity 2000 of amount EUR, for
// a service principal to post (T61n).
func (e env) serviceEntry(t *testing.T, amount string) *gen.JournalEntryCreate {
	t.Helper()
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	return &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"), Currency: "EUR",
		Lines: []gen.JournalLineInput{{AccountID: uuid.MustParse(bank), Amount: gen.Money{Amount: amount, Currency: "EUR"}},
			{AccountID: uuid.MustParse(revenue), Amount: gen.Money{Amount: "-" + amount, Currency: "EUR"}}}}
}

// postAsService posts body as the tenant's service principal through h.
func (e env) postAsService(t *testing.T, h *ledger.Endpoints, body *gen.JournalEntryCreate) (gen.PostJournalEntryRes, error) {
	t.Helper()
	e.ten.AllCompanies(t, e.ten.Service)
	var res gen.PostJournalEntryRes
	err := apitest.InRequest(t, e.ten, e.ten.Service, func(ctx context.Context) error {
		var err error
		res, err = h.PostJournalEntry(ctx, body, gen.PostJournalEntryParams{})
		return err
	})
	return res, err
}

// TestServiceLimitOfACompanyNotConfigured: a company the configuration
// lacks has a service posting limit of 0, so the entry is parked, never
// posted unchecked.
func TestServiceLimitOfACompanyNotConfigured(t *testing.T) {
	e := setup(t)
	h := endpoints()
	h.Settings = settingsOf{doc: &config.Config{}}
	res, err := e.postAsService(t, h, e.serviceEntry(t, "0.01"))
	if err != nil {
		t.Fatal(err)
	}
	parked, ok := res.(*gen.ParkedJournalEntryHeaders)
	if !ok {
		t.Fatalf("answer: got %T, want the parked entry", res)
	}
	l, _ := parked.Response.LimitExceeded.Get()
	if l.Limit != (gen.Money{Amount: "0.00", Currency: "EUR"}) {
		t.Errorf("limit: got %v, want 0.00 EUR", l.Limit)
	}
}

// TestServiceLimitUnreadable: a configuration that cannot be read fails
// the posting, naming the company.
func TestServiceLimitUnreadable(t *testing.T) {
	e := setup(t)
	h := endpoints()
	failed := errors.New("no document")
	h.Settings = settingsOf{err: failed}
	_, err := e.postAsService(t, h, e.serviceEntry(t, "1.00"))
	if !errors.Is(err, failed) || !strings.Contains(err.Error(), "company 2000") {
		t.Errorf("got %v, want the document's failure for company 2000", err)
	}
}

// TestServiceLimitNotAnAmount: a limit that is not an amount in the
// company's digits (a document validation would refuse) fails the posting.
func TestServiceLimitNotAnAmount(t *testing.T) {
	e := setup(t)
	h := endpoints()
	h.Settings = settingsOf{doc: &config.Config{Companies: []config.Company{{Code: "2000", ServicePostingLimit: "1.001"}}}}
	if _, err := e.postAsService(t, h, e.serviceEntry(t, "1.00")); err == nil || !strings.Contains(err.Error(), "service posting limit of company 2000") {
		t.Errorf("got %v, want the limit refused", err)
	}
}

// TestServiceLimitWithoutARate: an entry in a currency with no rate into
// its company's on the posting date is refused, as a person's is.
func TestServiceLimitWithoutARate(t *testing.T) {
	e := setup(t)
	body := e.serviceEntry(t, "1.00")
	body.Currency = "JPY"
	for i := range body.Lines {
		body.Lines[i].Amount.Currency, body.Lines[i].Amount.Amount = "JPY", strings.TrimSuffix(body.Lines[i].Amount.Amount, ".00")
	}
	if _, err := e.postAsService(t, endpoints(), body); problem.From(err).Code != "ledger.no_exchange_rate" {
		t.Errorf("got %v, want ledger.no_exchange_rate", err)
	}
}

// TestServiceLimitDatabaseFailures fails each statement of a service
// principal's posting within its limit and of one parked above it.
func TestServiceLimitDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.ten.AllCompanies(t, e.ten.Service)
	h := endpoints()
	h.Settings = settingsOf{doc: &config.Config{Companies: []config.Company{{Code: "2000", ServicePostingLimit: "100.00"}}}}
	for name, amount := range map[string]string{"posted": "100.00", "parked": "100.01"} {
		body := e.serviceEntry(t, amount)
		apitest.EveryFailure(t, e.ten, e.ten.Service, name, func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, body, gen.PostJournalEntryParams{})
			return err
		})
	}
}

// TestParkLimitDetail: the reason names each figure with its currency, and
// the conversion of an entry in another currency than its company's.
func TestParkLimitDetail(t *testing.T) {
	for name, c := range map[string]struct {
		limit ledger.ParkLimit
		want  string
	}{
		"same currency": {ledger.ParkLimit{Debits: 105000, DebitsCurrency: "CAD", DebitsExp: 2, Limit: 100000, LimitCurrency: "CAD", LimitExp: 2},
			"its debits of 1050.00 CAD exceed the company's service posting limit of 1000.00 CAD"},
		"another currency": {ledger.ParkLimit{Debits: money.Minor(80001), DebitsCurrency: "USD", DebitsExp: 2, Limit: 1000, LimitCurrency: "JPY", LimitExp: 0},
			"its debits of 800.01 USD, converted into JPY at the average rate of its posting date, exceed the company's service posting limit of 1000 JPY"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.limit.Detail(); !strings.Contains(got, c.want) {
				t.Errorf("Detail: got %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// TestServiceEntryOutOfRange: debits that overflow are refused before the
// entry is measured, as a person's are.
func TestServiceEntryOutOfRange(t *testing.T) {
	e := setup(t)
	body := e.serviceEntry(t, "92233720368547758.07") // 2^63-1 cents
	body.Lines = append(body.Lines, body.Lines...)
	if _, err := e.postAsService(t, endpoints(), body); problem.From(err).Code != "ledger.amount_too_large" {
		t.Errorf("got %v, want ledger.amount_too_large", err)
	}
}

// TestServiceEntryOutsideARequest: posting needs the request's transaction.
func TestServiceEntryOutsideARequest(t *testing.T) {
	e := setup(t)
	if _, err := endpoints().PostJournalEntry(context.Background(), e.serviceEntry(t, "1.00"), gen.PostJournalEntryParams{}); err == nil {
		t.Error("got no error, want the missing request refused")
	}
}
