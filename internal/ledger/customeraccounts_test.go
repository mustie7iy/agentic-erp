package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// withReceivables is the document with company 2000's new account 120000,
// a customer reconciliation account (T74a).
func withReceivables(tb testing.TB) *config.Config {
	tb.Helper()
	doc := document(tb)
	de := &doc.Companies[1]
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
	return doc
}

// newCustomer inserts an active customer; with data, set up in company
// 2000 on its receivables account 120000.
func (e env) newCustomer(t *testing.T, number string, data bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $3, 'DE', 'active', $4, $4)`, e.ten.ID, id, number, e.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, e.ten.ID, id, e.ten.Human.ID)
	if data {
		pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.ten.ID, id, e.de, e.account(t, "2000", "120000"), e.ten.Human.ID)
	}
	return id
}

// TestCustomerReconciliationAccounts (T74a): an account the configuration
// flags customer_reconciliation is the ledger's customer reconciliation
// account, open-item managed as a supplier's is, and its kind cannot change
// once activated; a line on it needs a customer, and a customer blocked
// for posting in the company is refused, while one without company data
// stays postable (plan-breadth-week1, default 2).
func TestCustomerReconciliationAccounts(t *testing.T) {
	e := setup(t)
	if err := apply(t, e, withReceivables(t)); err != nil {
		t.Fatal(err)
	}
	ar := e.account(t, "2000", "120000")
	if got := reconciliationKind(t, e, ar); got != "customer" {
		t.Errorf("account 120000: got reconciliation kind %s, want customer", got)
	}
	if !openItemManaged(t, e, ar) {
		t.Error("customer reconciliation account 120000 open-item managed: got false, want true")
	}

	turnedOn, turnedOff := document(t), withReceivables(t)
	turnedOn.Companies[1].Account("100000").CustomerReconciliation = true
	turnedOff.Companies[1].Account("120000").CustomerReconciliation = false
	for _, c := range []struct {
		name           string
		base, proposed *config.Config
		want           string
	}{
		{"the flag taken off", withReceivables(t), turnedOff, "companies[2000].chart_of_accounts[120000].customer_reconciliation"},
		{"an activated account flagged", document(t), turnedOn, "companies[2000].chart_of_accounts[100000].customer_reconciliation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				fs, err := ledger.Setup{LegalEntities: tenancy.Directory{}}.Check(ctx, c.base, c.proposed)
				for _, f := range fs {
					got = append(got, f.Path+": "+f.Message)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if want := c.want + ": an account's reconciliation kind cannot change once activated"; strings.Join(got, "\n") != want {
				t.Errorf("findings: got %q, want %q", got, want)
			}
		})
	}

	revenue := e.account(t, "2000", "400000")
	open, bare, blocked := e.newCustomer(t, "BP00000011", true), e.newCustomer(t, "BP00000012", false), e.newCustomer(t, "BP00000013", true)
	pgtest.Exec(t, `UPDATE partner.customer_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, blocked)
	sale := func(partner string) string {
		return entry(e.de, "2026-03-15", "EUR", line{ar, "100.00", partner}, line{revenue, "-100.00", ""})
	}
	for _, c := range []struct {
		name, partner string
		status        int
		code, detail  string // of the refusal; "" when posted
	}{
		{"a customer set up in the company", open.String(), 201, "", ""},
		{"a customer without company data", bare.String(), 201, "", ""},
		{"a customer blocked for posting in the company", blocked.String(), 422, "ledger.partner_not_postable",
			"the customer is blocked for posting in this legal entity"},
		{"a supplier", e.supplier.String(), 422, "ledger.partner_not_postable", "does not have the customer role"},
		{"no partner", "", 422, "ledger.partner_required", "needs a partner"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, m, _ := e.in(t).Do("POST", "/v1/journal-entries", sale(c.partner), "")
			if status != c.status {
				t.Fatalf("posting to the receivables account: got %d %v, want %d", status, m, c.status)
			}
			if c.code == "" {
				return
			}
			if m["code"] != c.code {
				t.Errorf("the refusal's code: got %v, want %s", m["code"], c.code)
			}
			if detail, _ := m["detail"].(string); !strings.Contains(detail, c.detail) {
				t.Errorf("the refusal's detail: got %q, want it to say %q", detail, c.detail)
			}
		})
	}
}
