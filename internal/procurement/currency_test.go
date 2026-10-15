package procurement_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestThresholdInTheCompanysCurrency: the requisition approval thresholds
// are in the company's currency (F12). A USD requisition of company 2000
// (EUR) is converted at the ledger's average rate, 0.9, before it is
// compared with the 10,000.00 tier: 11,111.11 USD is 10,000.00 EUR, one
// step; 11,111.12 USD is 10,000.01 EUR, two. Before, 11,111.11 was compared
// with 10,000.00 as it stood and took two steps. The converted amount is
// compared exactly: 3,333,334 JPY at 0.003 is 10,000.002 EUR, over the
// tier, although it rounds to 10,000.00 EUR.
func TestThresholdInTheCompanysCurrency(t *testing.T) {
	f := fixture.New(t, append(perms, "ledger.rate.maintain")...)
	e := env{Env: f, expense: f.Account(t, "2000", "510000").String(), office: f.Account(t, "2000", "520000").String()}
	e.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR", "valid_from": "2020-01-01",
		"rate": "0.9", "source": "ECB"}`, "")
	e.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "JPY", "to_currency": "EUR", "valid_from": "2020-01-01",
		"rate": "0.003", "source": "ECB"}`, "")
	for _, c := range []struct {
		currency, price string
		steps           int
	}{{"USD", "11111.11", 1}, {"USD", "11111.12", 2}, {"JPY", "3333333", 1}, {"JPY", "3333334", 2}} {
		t.Run(c.currency+" "+c.price, func(t *testing.T) {
			e := e.in(t)
			r, _ := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": %q, "items": %s}`,
				e.DE, c.currency, items(item(e.office, "1", "EA", c.price, c.currency, ""))), "")
			e.act(t, "/v1/requisitions/"+id(r), "submit", "")
			if got := len(e.Pending(t, id(r))["steps"].([]any)); got != c.steps {
				t.Errorf("approval steps: got %d, want %d", got, c.steps)
			}
		})
	}
	// Without a rate the submission is refused, not compared unconverted.
	r, tag := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "GBP", "items": %s}`,
		e.DE, items(item(e.office, "1", "EA", "1.00", "GBP", ""))), "")
	e.Problem(422, "ledger.no_exchange_rate", "POST", "/v1/requisitions/"+id(r)+":submit", "", tag)
}
