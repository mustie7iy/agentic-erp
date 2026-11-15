package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// TestOpenCompanies (T100b, ADR-0079 §11): the companies in which a
// partner has an open item, what blocking its personal data waits for:
// an open customer or supplier item counts; a partner whose items are all
// cleared or reversed has none, as has one the journal never names.
func TestOpenCompanies(t *testing.T) {
	e := setup(t)
	if err := apply(t, e, withReceivables(t)); err != nil {
		t.Fatal(err)
	}
	open, settled := e.newCustomer(t, "BP00000031", true), e.newCustomer(t, "BP00000032", true)
	var paid, credit ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		var reversed ledger.Entry
		var err error
		post := func(to *ledger.Entry, in ledger.Posting) {
			if err == nil {
				*to, err = postings().Post(ctx, in)
			}
		}
		post(new(ledger.Entry), customerPosting(t, e, open, 10000, "2026-03-01", nil))
		post(&paid, customerPosting(t, e, settled, 500, "2026-03-04", nil))
		post(&credit, customerPosting(t, e, settled, -500, "2026-03-04", nil))
		post(&reversed, customerPosting(t, e, settled, 900, "2026-03-03", nil))
		post(new(ledger.Entry), termsPosting(t, e, nil, &e.supplier))
		if err == nil {
			_, err = postings().Reverse(ctx, reversed.ID, nil, nil)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{map[string]any{"fiscal_year": 2026, "id": paid.ID.String()}, 1},
		ref{map[string]any{"fiscal_year": 2026, "id": credit.ID.String()}, 1}), "")
	for _, c := range []struct {
		name    string
		partner uuid.UUID
		want    int
	}{{"an open customer item", open, 1}, {"items cleared and reversed", settled, 0}, {"an open supplier item", e.supplier, 1}, {"no item", uuid.New(), 0}} {
		t.Run(c.name, func(t *testing.T) {
			var got []uuid.UUID
			if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
				got, err = ledger.OpenItems{}.OpenCompanies(ctx, c.partner)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != c.want || (c.want == 1 && got[0] != e.de) {
				t.Errorf("got %v, want %d company (2000)", got, c.want)
			}
		})
	}
	if _, err := (ledger.OpenItems{}).OpenCompanies(context.Background(), open); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "open companies", func(ctx context.Context) error {
		_, err := ledger.OpenItems{}.OpenCompanies(ctx, open)
		return err
	})
}
