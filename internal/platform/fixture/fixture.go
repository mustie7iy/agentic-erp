// Package fixture builds a configured tenant for tests of the business
// contexts: two legal entities, a bootstrapped configuration with number
// ranges for every document type, open 2026 periods, approvers holding
// every role the configuration names, and two active suppliers. It is test
// support and is excluded from the coverage figure.
package fixture

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ApprovalPermissions are added to every fixture tenant's human role.
var ApprovalPermissions = []string{"workflow.request.read", "workflow.request.decide", "workflow.request.cancel",
	"procurement.requisition.approve", "partner.change.approve", "payables.invoice.approve", "config.change.approve"}

// Years are the fiscal years the fixture sets up: 2026, which tests date
// documents in, and the current year, which numbers documents dated today.
func Years() []int {
	if y := time.Now().Year(); y != 2026 {
		return []int{2026, y}
	}
	return []int{2026}
}

// DocumentTypes get a number range per company and year.
var DocumentTypes = map[string]string{"journal_sa": "", "journal_ab": "", "journal_kr": "", "requisition": "PR", "purchase_order": "PO",
	"contract": "CT", "goods_receipt": "GR", "supplier_invoice": "SI", "partner_change": ""}

// Document is the prototype's configuration (companies 1000 in USD and
// 2000 in EUR) with number ranges for every document type in Years.
func Document(t testing.TB) *config.Config {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../config/testdata/valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, y := range Years() {
		for _, co := range []string{"1000", "2000"} {
			for dt, prefix := range DocumentTypes {
				c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: co, DocumentType: dt, FiscalYear: y, Prefix: prefix,
					First: 100000, Last: 999999})
			}
		}
	}
	return c
}

// Approvals is the workflow port the API wires.
func Approvals() approvals.Approvals {
	return approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}}
}

// Deps is what the API wires into the configuration context.
func Deps() configuration.Dependencies {
	return configuration.Dependencies{LegalEntities: tenancy.Directory{}, Approvals: Approvals(),
		Sections: []configuration.Section{ledger.Setup{LegalEntities: tenancy.Directory{}}, partner.PaymentTerms{}}}
}

// Env is a configured tenant.
type Env struct {
	apitest.Client      // as Tenant.Human, the requester
	Tenant              pgtest.Tenant
	Approver, Approver2 pgtest.Actor // people holding every role the configuration names
	US, DE              uuid.UUID    // legal entities 1000 (USD) and 2000 (EUR)
	Supplier, Vendor    uuid.UUID    // active suppliers set up in both legal entities
}

// New creates a configured tenant whose people hold the permissions plus
// ApprovalPermissions. Tenant.Human requests; the approvers decide.
func New(t testing.TB, permissions ...string) Env {
	t.Helper()
	ten := pgtest.NewTenant(t, append(permissions, ApprovalPermissions...)...)
	ten.AllCompanies(t, ten.Human)
	e := Env{Client: apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}, Tenant: ten,
		US: uuid.Must(uuid.NewV7()), DE: uuid.Must(uuid.NewV7())}
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '1000', 'Acme Corp US', 'US', 'USD', gen_random_uuid(), 'active', $4, $4),
		       ($1, $3, '2000', 'Acme GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $4, $4)`, ten.ID, e.US, e.DE, ten.Human.ID)
	doc := Document(t)
	for _, r := range doc.Roles {
		pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $3, 'active', $4, $4)`,
			ten.ID, r.Name, r.Description, ten.Human.ID)
	}
	e.Approver, e.Approver2 = e.person(t, "approver1"), e.person(t, "approver2")
	b, _ := json.Marshal(doc)
	if err := apitest.Committed(t, ten, ten.Human, func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, Deps(), b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND period <= 12`, ten.ID)
	e.Supplier, e.Vendor = e.NewSupplier(t, "BP90000001"), e.NewSupplier(t, "BP90000002")
	return e
}

// person creates a human in every legal entity holding the tenant's human
// role and every role other than "all" and "machine".
func (e Env) person(t testing.TB, name string) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, true, 'active', $2, $2)`, e.Tenant.ID, a.ID, a.Subject, name)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code <> 'machine'`, e.Tenant.ID, a.ID)
	return a
}

// As is a client acting as another principal of the tenant.
func (e Env) As(a pgtest.Actor) apitest.Client {
	c := e.Client
	c.As = a
	return c
}

// NewSupplier inserts an active supplier set up in both legal entities on
// their payables reconciliation account 211000 with payment terms NT30.
func (e Env) NewSupplier(t testing.TB, number string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $3, 'US', 'active', $4, $4)`, e.Tenant.ID, id, number, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'supplier', $3)`, e.Tenant.ID, id, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		SELECT $1, $2, le.id, a.id, 'NT30', $3, $3 FROM tenancy.legal_entity le JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
		JOIN ledger.gl_account a ON a.chart_id = c.id AND a.number = '211000' WHERE le.tenant_id = $1`, e.Tenant.ID, id, e.Tenant.Human.ID)
	return id
}

// Account is the id of an account of a company's chart.
func (e Env) Account(t testing.TB, company, number string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT a.id FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.id = a.chart_id
		WHERE c.tenant_id = $1 AND c.code = $2 AND a.number = $3`, e.Tenant.ID, company, number).Scan(&id); err != nil {
		t.Fatalf("account %s/%s: %v", company, number, err)
	}
	return id
}

// Pending is the pending approval request of an object, read as an approver.
func (e Env) Pending(t testing.TB, objectID string) map[string]any {
	t.Helper()
	page, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests?status=pending&object_id="+objectID, "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("pending approval requests for %s: %v", objectID, items)
	}
	return items[0].(map[string]any)
}

// Approve has the approvers approve the object's pending request until it
// completes.
func (e Env) Approve(t testing.TB, objectID string) {
	t.Helper()
	for _, a := range []pgtest.Actor{e.Approver, e.Approver2} {
		r := e.Pending(t, objectID)
		got, _ := e.As(a).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, `"`+jsonNumber(r["version"])+`"`)
		if got["status"] != approvals.Pending {
			return
		}
	}
	t.Fatalf("approval request of %s still pending after two approvals", objectID)
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
