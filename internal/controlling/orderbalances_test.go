package controlling_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Costs posted to internal orders and the orders' actuals (T60b, ADR-0064
// §2).

// releasedOrder creates an order of company 2000 under cost center 1000,
// releases it and returns its id.
func (p plan) releasedOrder(t *testing.T, code string) string {
	t.Helper()
	id := p.order(t, code)
	p.Must(200, "POST", "/v1/internal-orders/"+id+":release", "", `"1"`)
	return id
}

// charge posts the amount in EUR to the order on the account against the
// bank on the day and returns the entry with its ETag.
func (p plan) charge(t *testing.T, order, day string, account uuid.UUID, amount string) (map[string]any, string) {
	t.Helper()
	return p.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR",
		"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}, "internal_order_id": %q},
			{"account_id": %q, "amount": {"amount": "-%s", "currency": "EUR"}}]}`, p.DE, day, day, account, amount, order, p.bank, amount), "")
}

// balances reads a page of an order's 2026 actuals as "number name/period
// debit credit balance currency" rows, then the totals, then "next" when
// there is a further page, and returns that page's token.
func balances(t *testing.T, c apitest.Client, order, query string) ([]string, string) {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/internal-order-balances?fiscal_year=2026&internal_order_id="+order+query, "", "")
	figures := func(v any) string {
		m := v.(map[string]any)
		amount := func(k string) any { return m[k].(map[string]any)["amount"] }
		return fmt.Sprint(amount("debit"), " ", amount("credit"), " ", amount("balance"), " ", m["balance"].(map[string]any)["currency"])
	}
	var out []string
	for _, it := range page["items"].([]any) {
		r := it.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v/%v %s", r["account_number"], r["account_name"], r["fiscal_period"], figures(r)))
	}
	out = append(out, "totals "+figures(page["totals"]))
	token, more := page["next_page_token"].(string)
	if more {
		out = append(out, "next")
	}
	return out, token
}

// accountName is the name of an account of the tenant.
func (p plan) accountName(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var name string
	pgtest.Scan(t, `SELECT name FROM ledger.gl_account WHERE tenant_id = $1 AND id = $2`, []any{p.Tenant.ID, id}, &name)
	return name
}

func TestInternalOrderBalances(t *testing.T) {
	p := setup(t)
	fair := p.releasedOrder(t, "FAIR-26")
	other := p.releasedOrder(t, "OTHER")
	it := p.Account(t, "2000", "530000") // a primary cost account without a default cost center
	office, itName := "520000 "+p.accountName(t, p.office), "530000 "+p.accountName(t, it)
	p.charge(t, fair, "2026-05-10", p.office, "100.00")
	p.charge(t, fair, "2026-05-20", p.office, "20.00")
	_, tag := p.charge(t, fair, "2026-06-15", p.office, "7.00")
	p.charge(t, fair, "2026-06-16", it, "50.00")
	p.charge(t, other, "2026-05-10", p.office, "999.00")
	// A cost center's own line is no part of any order's actuals.
	p.post(t, "2026-05-10", on(p.office, 4200, &p.c.Admin), on(p.bank, -4200, nil))

	t.Run("the actuals equal the lines posted", func(t *testing.T) {
		got, _ := balances(t, p.Client, fair, "")
		wantList(t, "balances", got, []string{office + "/5 120.00 0.00 120.00 EUR", office + "/6 7.00 0.00 7.00 EUR", itName + "/6 50.00 0.00 50.00 EUR",
			"totals 177.00 0.00 177.00 EUR"})
		var posted int64
		pgtest.Scan(t, `SELECT sum(l.amount_lc) FROM ledger.journal_line l JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
			WHERE l.tenant_id = $1 AND l.internal_order_id = $2`, []any{p.Tenant.ID, fair}, &posted)
		want(t, "the lines posted to the order, in minor units", posted, 17700)
	})
	t.Run("the page names the order and the selection", func(t *testing.T) {
		page, _ := p.in(t).Must(200, "GET", "/v1/internal-order-balances?fiscal_year=2026&fiscal_period_from=5&fiscal_period_to=5&internal_order_id="+fair, "", "")
		want(t, "header", fmt.Sprint(page["internal_order_id"], " ", page["internal_order_code"], " ", page["internal_order_name"], " ", page["legal_entity_id"],
			" ", page["fiscal_year"], " ", page["fiscal_period_from"], " ", page["fiscal_period_to"]),
			fmt.Sprint(fair, " FAIR-26 Order FAIR-26 ", p.DE, " 2026 5 5"))
	})
	t.Run("a reversal's lines count as credits", func(t *testing.T) {
		p := p.in(t)
		entries, _ := p.Must(200, "GET", "/v1/journal-entries?legal_entity_id="+p.DE.String()+"&fiscal_year=2026&page_size=100", "", "")
		var seven string
		for _, e := range entries["items"].([]any) {
			m := e.(map[string]any)
			if m["lines"].([]any)[0].(map[string]any)["amount"].(map[string]any)["amount"] == "7.00" {
				seven = m["id"].(string)
			}
		}
		p.Must(200, "POST", "/v1/journal-entries/"+seven+":reverse", "", tag)
		got, _ := balances(t, p.Client, fair, "&fiscal_period_from=6")
		wantList(t, "balances from June", got, []string{office + "/6 7.00 7.00 0.00 EUR", itName + "/6 50.00 0.00 50.00 EUR", "totals 57.00 7.00 50.00 EUR"})
	})
	t.Run("pages carry the whole selection's totals", func(t *testing.T) {
		first, token := balances(t, p.Client, fair, "&page_size=2")
		wantList(t, "first page", first, []string{office + "/5 120.00 0.00 120.00 EUR", office + "/6 7.00 7.00 0.00 EUR", "totals 177.00 7.00 170.00 EUR", "next"})
		second, _ := balances(t, p.Client, fair, "&page_size=2&page_token="+token)
		wantList(t, "second page", second, []string{itName + "/6 50.00 0.00 50.00 EUR", "totals 177.00 7.00 170.00 EUR"})
		none, _ := balances(t, p.Client, fair, "&fiscal_period_from=7")
		wantList(t, "periods without postings", none, []string{"totals 0.00 0.00 0.00 EUR"})
	})
	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		path := "/v1/internal-order-balances?fiscal_year=2026&internal_order_id="
		p.Problem(400, "request.invalid_parameter", "GET", path+fair+"&fiscal_period_from=6&fiscal_period_to=5", "", "")
		// A page size the contract refuses first, sent by a direct call.
		err := apitest.InRequest(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
			_, err := endpoints(t).ListInternalOrderBalances(ctx, gen.ListInternalOrderBalancesParams{InternalOrderID: uuid.MustParse(fair), FiscalYear: 2026,
				PageSize: gen.NewOptInt32(0)})
			return err
		})
		want(t, "page size 0", problem.From(err).Code, "request.invalid_page_size")
		p.Problem(400, "request.invalid_page_token", "GET", path+fair+"&page_token=nonsense", "", "")
		p.Problem(422, "request.reference_not_found", "GET", path+uuid.NewString(), "", "")
		p.as(p.Reader(t, "Reader of 1000", p.US)).Problem(422, "request.reference_not_found", "GET", path+fair, "", "")
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'ledger.entry.read'`, p.Tenant.ID)
		p.Problem(403, "identity.permission_missing", "GET", path+fair, "", "")
	})
}

// TestPostingHoldsItsOrders: a posting holds the orders its lines name
// until its transaction ends, so a status change, which takes the order
// FOR UPDATE, waits for it.
func TestPostingHoldsItsOrders(t *testing.T) {
	p := setup(t)
	id := uuid.MustParse(p.releasedOrder(t, "FAIR-26"))
	held, changed := apitest.Serialized(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		orders, err := controlling.CostObjects{}.Orders(ctx, []uuid.UUID{id, uuid.New()}, true)
		if err != nil {
			return err
		}
		o := orders[id]
		want(t, "orders read, the unknown id left out", len(orders), 1)
		want(t, "the order takes postings", o.Postable, true)
		want(t, "the order's code", o.Code, "FAIR-26")
		want(t, "the order's responsible cost center", o.CostCenterID, p.c.Admin)
		want(t, "the order's company", o.LegalEntityID, p.DE)
		return nil
	}, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT 1 FROM controlling.internal_order WHERE id = $1 FOR UPDATE`, id)
		return err
	})
	if err := errors.Join(held, changed); err != nil {
		t.Fatal(err)
	}
}
