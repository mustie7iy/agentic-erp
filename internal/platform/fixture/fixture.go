// Package fixture builds a configured tenant for tests of the business
// contexts: two legal entities, a bootstrapped configuration with number
// ranges for every document type, open 2026 periods, approvers holding every
// role the configuration names, and two active suppliers, so application
// tests run on real PostgreSQL (06-engineering-process §6.6). It is test
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

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ApprovalPermissions are added to every fixture tenant's human role.
var ApprovalPermissions = []string{"workflow.request.read", "workflow.request.decide", "workflow.request.cancel",
	"procurement.requisition.approve", "partner.change.approve", "payables.invoice.approve", "config.change.approve",
	"ledger.period.unlock.approve"}

// Years are the fiscal years the fixture sets up: 2026, which tests date
// documents in, and the current year, which numbers documents dated today.
func Years() []int {
	if y := time.Now().Year(); y != 2026 {
		return []int{2026, y}
	}
	return []int{2026}
}

// DocumentTypes get a number range per company and year.
var DocumentTypes = map[string]string{"journal_sa": "", "journal_ab": "", "journal_kr": "", "journal_we": "", "requisition": "PR", "purchase_order": "PO",
	"contract": "CT", "goods_receipt": "GR", "supplier_invoice": "SI", "partner_change": "", "payment_run": "PR", "journal_zp": "",
	"journal_re": "", "journal_kz": "", "journal_ob": ""}

// Document is the prototype's configuration (companies 1000 in USD and
// 2000 in EUR) with number ranges for every document type in Years.
func Document(tb testing.TB) *config.Config {
	tb.Helper()
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../config/testdata/valid.yaml"))
	if err != nil {
		tb.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		tb.Fatal(err)
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

// rates converts document amounts into their company's currency, as the
// API wires it.
var rates = ledger.Rates{LegalEntities: tenancy.Directory{}}

// Approvals is the workflow port the API wires.
func Approvals() approvals.Approvals {
	return approvals.Approvals{Settings: configuration.Settings{}, Roles: identity.Directory{}, Rates: rates}
}

// Deps is what the API wires into the configuration context.
func Deps() configuration.Dependencies {
	return configuration.Dependencies{LegalEntities: tenancy.Directory{}, Approvals: Approvals(),
		Sections: []configuration.Section{ledger.Setup{LegalEntities: tenancy.Directory{}}, partner.PaymentTerms{},
			payments.HouseBanks{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Keys: apitest.FieldKeys()}},
		Simulator: payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates},
		Regressor: payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates}, Actors: identity.Actors{}}
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
func New(tb testing.TB, permissions ...string) Env {
	tb.Helper()
	ten := pgtest.NewTenant(tb, append(permissions, ApprovalPermissions...)...)
	ten.AllCompanies(tb, ten.Human)
	e := Env{Client: apitest.Client{T: tb, API: apitest.New(tb), Tenant: ten, As: ten.Human}, Tenant: ten,
		US: uuid.Must(uuid.NewV7()), DE: uuid.Must(uuid.NewV7())}
	pgtest.Exec(tb, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '1000', 'Acme Corp US', 'US', 'USD', gen_random_uuid(), 'active', $4, $4),
		       ($1, $3, '2000', 'Acme GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $4, $4)`, ten.ID, e.US, e.DE, ten.Human.ID)
	doc := Document(tb)
	for _, r := range doc.RoleNames() {
		pgtest.Exec(tb, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $2, 'active', $3, $3)`,
			ten.ID, r, ten.Human.ID)
	}
	e.Approver, e.Approver2 = e.person(tb, "approver1"), e.person(tb, "approver2")
	b, _ := json.Marshal(doc)
	if err := apitest.Committed(tb, ten, ten.Human, func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, Deps(), b)
		return err
	}); err != nil {
		tb.Fatal(err)
	}
	pgtest.Exec(tb, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND period <= 12`, ten.ID)
	e.Supplier, e.Vendor = e.NewSupplier(tb, "BP90000001"), e.NewSupplier(tb, "BP90000002")
	return e
}

// Person creates a human in every legal entity holding the tenant's human
// role and every role other than "all" and "machine": an approver, or one
// of the many users of a load test.
func (e Env) Person(tb testing.TB, name string) pgtest.Actor {
	tb.Helper()
	return e.person(tb, name)
}

// person creates a human in every legal entity holding the tenant's human
// role and every role other than "all" and "machine".
func (e Env) person(tb testing.TB, name string) pgtest.Actor {
	tb.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(tb, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, true, 'active', $2, $2)`, e.Tenant.ID, a.ID, pgtest.Issuer(tb), a.Subject, name)
	pgtest.Exec(tb, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
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
func (e Env) NewSupplier(tb testing.TB, number string) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $3, 'US', 'active', $4, $4)`, e.Tenant.ID, id, number, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'supplier', $3)`, e.Tenant.ID, id, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		SELECT $1, $2, le.id, a.id, 'NT30', $3, $3 FROM tenancy.legal_entity le JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
		JOIN ledger.gl_account a ON a.chart_id = c.id AND a.number = '211000' WHERE le.tenant_id = $1`, e.Tenant.ID, id, e.Tenant.Human.ID)
	return id
}

// Account is the id of an account of a company's chart.
func (e Env) Account(tb testing.TB, company, number string) uuid.UUID {
	tb.Helper()
	var id uuid.UUID
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT a.id FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.id = a.chart_id
		WHERE c.tenant_id = $1 AND c.code = $2 AND a.number = $3`, e.Tenant.ID, company, number).Scan(&id); err != nil {
		tb.Fatalf("account %s/%s: %v", company, number, err)
	}
	return id
}

// Pending is the pending approval request of an object, read as an approver.
func (e Env) Pending(tb testing.TB, objectID string) map[string]any {
	tb.Helper()
	page, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests?status=pending&object_id="+objectID, "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		tb.Fatalf("pending approval requests for %s: %v", objectID, items)
	}
	return items[0].(map[string]any)
}

// Approve has the approvers approve the object's pending request until it
// completes.
func (e Env) Approve(tb testing.TB, objectID string) {
	tb.Helper()
	for _, a := range []pgtest.Actor{e.Approver, e.Approver2} {
		r := e.Pending(tb, objectID)
		got, _ := e.As(a).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, `"`+jsonNumber(r["version"])+`"`)
		if got["status"] != approvals.Pending {
			return
		}
	}
	tb.Fatalf("approval request of %s still pending after two approvals", objectID)
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// GRIR gives company 2000 a GR/IR account (219000), a price difference
// account (590000) and FX accounts (480000 gains, 690000 losses) in the
// ledger, as an activated configuration with them would: orders released
// from now on are valued through GR/IR (T33).
func (e Env) GRIR(tb testing.TB) {
	tb.Helper()
	doc := Document(tb)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "219000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense},
		config.Account{Number: "480000", Name: "FX gains", Type: config.Revenue}, config.Account{Number: "690000", Name: "FX losses", Type: config.Expense})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "219000", "590000"
	de.AccountDetermination.FXGain, de.AccountDetermination.FXLoss = "480000", "690000"
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
}
