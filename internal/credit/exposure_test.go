package credit_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// story posts what the tests' exposures read: Northwind owes 100.00 and
// 50.00 EUR and 200.00 USD, booked at 160.00 EUR, and has a credit of
// 20.00 EUR; Eastwind has a credit of 30.00 EUR alone; Southwind, which
// has no customer data, owes 40.00 EUR by a manual entry.
func (e env) story(t *testing.T) {
	t.Helper()
	e.post(t,
		item{customer: e.north, amount: 10000, currency: "EUR", ref: "CI-A"},
		item{customer: e.north, amount: 5000, currency: "EUR", ref: "CI-B"},
		item{customer: e.north, amount: 20000, currency: "USD", ref: "CI-USD"},
		item{customer: e.north, amount: -2000, currency: "EUR", ref: "CREDIT"},
		item{customer: e.east, amount: -3000, currency: "EUR", ref: "ON-ACCOUNT"},
		item{customer: e.south, amount: 4000, currency: "EUR", ref: "MANUAL"})
}

// exposure reads the customer's exposure in the company, with the query
// added ("&amount=…").
func (e env) exposure(t *testing.T, company, customer uuid.UUID, query string) map[string]any {
	t.Helper()
	got, _ := e.In(t).Must(200, "GET", "/v1/credit-exposure?legal_entity_id="+company.String()+"&customer_id="+customer.String()+query, "", "")
	return got
}

// checked is an exposure as "items exposure | has limit, limit,
// available | amount checked | within", with "-" for what is absent.
func checked(t *testing.T, x map[string]any) string {
	t.Helper()
	opt := func(f string) string {
		if _, ok := x[f]; !ok {
			return "-"
		}
		return amount(t, x, f)
	}
	return fmt.Sprintf("%v %s | %v %s %s | %s | %v", x["open_item_count"], amount(t, x, "exposure"), x["has_limit"], opt("limit"), opt("available"),
		opt("amount"), x["is_within_limit"])
}

// booked is what the ledger's open items of the customer in company 2000
// add up to in the company's currency, and the items themselves.
func (e env) booked(t *testing.T, customer uuid.UUID) (money.Minor, string) {
	t.Helper()
	var items []ledger.ReceivableItem
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Receivable(ctx, e.DE, []uuid.UUID{customer})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var sum money.Minor
	for _, it := range items {
		sum += it.AmountLC
	}
	return sum, mustJSON(t, items)
}

// stored counts what the credit context holds: its limits, its events,
// and its audit records.
func (e env) stored(t *testing.T) string {
	t.Helper()
	var limits, events, audits int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM credit.credit_limit WHERE tenant_id = $1), (SELECT count(*) FROM credit.outbox WHERE tenant_id = $1),
		(SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'credit.%')`, []any{e.Tenant.ID}, &limits, &events, &audits)
	return fmt.Sprintf("%d limits, %d events, %d audit records", limits, events, audits)
}

// TestExposure: the exposure is the customer's open items in the
// company's currency as booked, credits netted; available is the limit
// less the exposure; an amount is within the limit when it and the
// exposure together are no more than the limit; a customer without a
// limit reads as within it, saying none is set; the check changes
// nothing (T77a acceptance).
func TestExposure(t *testing.T) {
	e := setup(t)
	e.story(t)
	sum, before := e.booked(t, e.north)
	if sum != 29000 {
		t.Fatalf("Northwind's open items: got %v, want 29000 minor: 100.00 + 50.00 + 160.00 - 20.00 EUR", sum)
	}
	t.Run("without a limit", func(t *testing.T) {
		got := e.exposure(t, e.DE, e.north, "")
		for f, want := range map[string]any{"legal_entity_id": e.DE.String(), "company_code": "2000", "company_name": "Acme GmbH", "currency": "EUR",
			"customer_id": e.north.String(), "customer_number": "BP70000002", "customer_name": "Northwind Traders"} {
			if got[f] != want {
				t.Errorf("%s: got %v, want %v", f, got[f], want)
			}
		}
		if c := checked(t, got); c != "4 290.00 EUR | false - - | - | true" {
			t.Errorf("got %s, want 4 290.00 EUR | false - - | - | true", c)
		}
		if _, ok := got["credit_limit_id"]; ok {
			t.Errorf("credit_limit_id: got %v, want it absent", got["credit_limit_id"])
		}
		if c := checked(t, e.exposure(t, e.DE, e.north, "&amount=1000000.00")); c != "4 290.00 EUR | false - - | 1000000.00 EUR | true" {
			t.Errorf("an amount checked: got %s, want 4 290.00 EUR | false - - | 1000000.00 EUR | true", c)
		}
	})
	set, tag := e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	id := apitest.String(t, set, "id")
	held := e.stored(t)
	for _, c := range []struct{ name, query, want string }{
		{"with a limit", "", "4 290.00 EUR | true 500.00 EUR 210.00 EUR | - | true"},
		{"an amount of nothing", "&amount=0.00", "4 290.00 EUR | true 500.00 EUR 210.00 EUR | 0.00 EUR | true"},
		{"an amount within the limit", "&amount=209.99", "4 290.00 EUR | true 500.00 EUR 210.00 EUR | 209.99 EUR | true"},
		{"the amount that reaches the limit", "&amount=210.00", "4 290.00 EUR | true 500.00 EUR 210.00 EUR | 210.00 EUR | true"},
		{"a cent more", "&amount=210.01", "4 290.00 EUR | true 500.00 EUR 210.00 EUR | 210.01 EUR | false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.exposure(t, e.DE, e.north, c.query)
			if s := checked(t, got); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
			if got["credit_limit_id"] != id {
				t.Errorf("credit_limit_id: got %v, want %s", got["credit_limit_id"], id)
			}
		})
	}
	t.Run("the checks change nothing", func(t *testing.T) {
		if _, after := e.booked(t, e.north); after != before {
			t.Errorf("the open items after the checks: got %s, want them unchanged: %s", after, before)
		}
		if now := e.stored(t); now != held {
			t.Errorf("stored after the checks: got %s, want %s", now, held)
		}
		read, readTag := e.In(t).Must(200, "GET", "/v1/credit-limits/"+id, "", "")
		if a, want := amount(t, read, "limit")+" "+readTag, "500.00 EUR "+tag; a != want {
			t.Errorf("the limit after the checks: got %s, want %s", a, want)
		}
	})
	t.Run("a limit below the exposure", func(t *testing.T) {
		e.In(t).Must(200, "PATCH", "/v1/credit-limits/"+id, change("200.00", "EUR"), tag)
		if c := checked(t, e.exposure(t, e.DE, e.north, "")); c != "4 290.00 EUR | true 200.00 EUR -90.00 EUR | - | false" {
			t.Errorf("got %s, want 4 290.00 EUR | true 200.00 EUR -90.00 EUR | - | false", c)
		}
	})
	t.Run("a customer whose credits exceed what it owes", func(t *testing.T) {
		e.In(t).Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.east, "100.00", "EUR"), "")
		if c := checked(t, e.exposure(t, e.DE, e.east, "&amount=130.00")); c != "1 -30.00 EUR | true 100.00 EUR 130.00 EUR | 130.00 EUR | true" {
			t.Errorf("got %s, want 1 -30.00 EUR | true 100.00 EUR 130.00 EUR | 130.00 EUR | true", c)
		}
	})
	t.Run("a business partner without customer data", func(t *testing.T) {
		if c := checked(t, e.exposure(t, e.DE, e.south, "&amount=1.00")); c != "1 40.00 EUR | false - - | 1.00 EUR | true" {
			t.Errorf("got %s, want 1 40.00 EUR | false - - | 1.00 EUR | true: its items posted by a manual entry count", c)
		}
	})
	t.Run("in a company where it owes nothing", func(t *testing.T) {
		if c := checked(t, e.exposure(t, e.US, e.north, "&amount=5.00")); c != "0 0.00 USD | false - - | 5.00 USD | true" {
			t.Errorf("got %s, want 0 0.00 USD | false - - | 5.00 USD | true", c)
		}
	})
}

// TestExposureRefusals: the exposure is read for a company the caller
// may act in and a business partner that exists; an amount checked is
// zero or more, with the currency's digits.
func TestExposureRefusals(t *testing.T) {
	e := setup(t)
	e.story(t)
	path := func(company, customer uuid.UUID, query string) string {
		return "/v1/credit-exposure?legal_entity_id=" + company.String() + "&customer_id=" + customer.String() + query
	}
	other := e.As(e.Reader(t, "A person of company 1000", e.US))
	for _, c := range []struct {
		name   string
		as     apitest.Client
		status int
		code   string
		path   string
		field  any
	}{
		{"an unknown company", e.Client, 404, "request.not_found", path(uuid.New(), e.north, ""), nil},
		{"a company the caller may not act in", other, 404, "request.not_found", path(e.DE, e.north, ""), nil},
		{"an unknown business partner", e.Client, 404, "request.not_found", path(e.DE, uuid.New(), ""), nil},
		{"an amount below zero", e.Client, 422, "credit.invalid_amount", path(e.DE, e.north, "&amount=-1.00"), "amount"},
		{"an amount with the wrong decimals", e.Client, 400, "request.invalid_amount", path(e.DE, e.north, "&amount=1.0"), "amount"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := c.as.In(t).Problem(c.status, c.code, "GET", c.path, "", "")
			if f := field(p); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'credit.limit.read'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "GET", path(e.DE, e.north, ""), "", "")
	})
}
