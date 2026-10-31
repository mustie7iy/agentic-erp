// Package fixture builds a configured tenant for tests of the business
// contexts: two legal entities (more with NewWith), a bootstrapped
// configuration with number ranges for every document type, open 2026
// periods, approvers holding every role the configuration names, and two
// active suppliers, so application tests run on real PostgreSQL
// (06-engineering-process §6.6). It is test support and is excluded from
// the coverage figure.
package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ApprovalPermissions are added to every fixture tenant's human role.
var ApprovalPermissions = []string{"workflow.request.read", "workflow.request.decide", "workflow.request.cancel",
	"procurement.requisition.approve", "partner.change.approve", "payables.invoice.approve", "config.change.approve",
	"ledger.period.unlock.approve", "ledger.entry.approve", "procurement.order.approve", "payments.statement.clearing.approve",
	"ledger.cost_center.approve", "procurement.gr_ir.clear.approve"}

// Years are the fiscal years the fixture sets up: 2026, which tests date
// documents in, and the current year, which numbers documents dated today.
func Years() []int {
	if y := time.Now().Year(); y != 2026 {
		return []int{2026, y}
	}
	return []int{2026}
}

// DocumentTypes get a number range per company and year.
var DocumentTypes = map[string]string{"journal_sa": "", "journal_ab": "", "journal_kr": "", "journal_we": "", "journal_kg": "", "requisition": "PR", "purchase_order": "PO",
	"contract": "CT", "goods_receipt": "GR", "supplier_invoice": "SI", "partner_change": "", "payment_run": "PR", "journal_zp": "",
	"journal_re": "", "journal_kz": "", "journal_ob": "", "down_payment_request": "DP"}

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
	c.Extensions = Schemas()
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

// Schemas are the custom field schemas of the prototype's configuration:
// optional strings on each first entity (ADR-0046), version 1. The last
// key of each is personal or sensitive, so events mask it.
func Schemas() map[string]extensions.Schema {
	schema := func(private string, pii extensions.PII, keys ...string) extensions.Schema {
		s := extensions.Schema{Version: 1, Type: "object", AdditionalProperties: new(bool), Properties: map[string]extensions.Property{}}
		for _, k := range keys {
			s.Properties[k] = extensions.Property{Type: "string", PII: extensions.PIINone}
		}
		s.Properties[private] = extensions.Property{Type: "string", PII: pii}
		return s
	}
	return map[string]extensions.Schema{"business_partner": schema("routing", extensions.PIISensitive, "cost_object_hint", "segment"),
		"purchase_order":   schema("project", extensions.PIIPersonal, "region"),
		"supplier_invoice": schema("channel", extensions.PIIPersonal, "region")}
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
			partner.PaymentMethodCodes{LegalEntities: tenancy.Directory{}},
			payments.HouseBanks{LegalEntities: tenancy.Directory{}, Accounts: ledger.Accounts{}, Channels: integration.BankChannels{}, Keys: apitest.FieldKeys()},
			payments.PaymentMethods{LegalEntities: tenancy.Directory{}}, cash.Setup{}, partner.PlanningGroupCodes{LegalEntities: tenancy.Directory{}}},
		Simulator: configuration.Simulators{payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates},
			ledger.AssignmentSimulation{LegalEntities: tenancy.Directory{}}},
		Regressor: payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates}, Actors: identity.Actors{},
		RuleSamplers: map[string]rules.Sampler{"supplier_invoice": payables.RuleSamples{}, "purchase_order": procurement.RuleSamples{},
			"business_partner": partner.RuleSamples{}},
		Disclosures: []configuration.Disclosure{payables.Disclosure{}, ledger.AssignmentDisclosure{}}}
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
	return NewWith(tb, nil, nil, permissions...)
}

// Company is a further legal entity of a tenant NewWith creates.
type Company struct{ Code, Name, Country, Currency string }

// NewWith is New with further companies: their legal entities, number
// ranges for every document type in Years, and the configuration document
// changed by extend (which adds the companies, or parallel ledgers a later
// change request could not add without its evidence, ADR-0045) before it
// is bootstrapped.
func NewWith(tb testing.TB, companies []Company, extend func(*config.Config), permissions ...string) Env {
	tb.Helper()
	ten := pgtest.NewTenant(tb, append(permissions, ApprovalPermissions...)...)
	ten.AllCompanies(tb, ten.Human)
	e := Env{Client: apitest.Client{T: tb, API: apitest.New(tb), Tenant: ten, As: ten.Human}, Tenant: ten,
		US: uuid.Must(uuid.NewV7()), DE: uuid.Must(uuid.NewV7())}
	pgtest.Exec(tb, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '1000', 'Acme Corp US', 'US', 'USD', gen_random_uuid(), 'active', $4, $4),
		       ($1, $3, '2000', 'Acme GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $4, $4)`, ten.ID, e.US, e.DE, ten.Human.ID)
	doc := Document(tb)
	for _, co := range companies {
		pgtest.Exec(tb, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, gen_random_uuid(), 'active', $6, $6)`, ten.ID, co.Code, co.Name, co.Country, co.Currency, ten.Human.ID)
		for _, y := range Years() {
			for dt, prefix := range DocumentTypes {
				doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: co.Code, DocumentType: dt, FiscalYear: y, Prefix: prefix,
					First: 100000, Last: 999999})
			}
		}
	}
	if extend != nil {
		extend(doc)
	}
	for _, r := range doc.RoleNames() {
		pgtest.Exec(tb, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $2, 'active', $3, $3)`,
			ten.ID, r, ten.Human.ID)
	}
	e.Approver, e.Approver2 = e.person(tb, "approver1"), e.person(tb, "approver2")
	b, _ := json.Marshal(doc)
	if err := apitest.Committed(tb, ten, ten.Human, func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, Deps(), b, "configuration") // as provisioning names it (DocumentField)
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

// Reader is a person holding the tenant's permissions in the legal entities
// alone (none: no company).
func (e Env) Reader(tb testing.TB, name string, legalEntities ...uuid.UUID) pgtest.Actor {
	tb.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(tb, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, 'active', $2, $2)`, e.Tenant.ID, a.ID, pgtest.Issuer(tb), a.Subject, name)
	pgtest.Exec(tb, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, e.Tenant.ID, a.ID)
	for _, le := range legalEntities {
		pgtest.Exec(tb, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, e.Tenant.ID, a.ID, le)
	}
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

// Requisition submits, as the tenant's human, a requisition for ten
// laptops at EUR 1,200.00 in company 2000, above the first tier, and
// returns its pending approval request (two steps). The tenant needs
// procurement.requisition.create and .read.
func (e Env) Requisition(tb testing.TB) uuid.UUID {
	tb.Helper()
	req, _ := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR",
		"items": [{"description": "Laptop", "quantity": {"value": "10", "unit": "EA"}, "unit_price": {"amount": "1200.00", "currency": "EUR"}, "account_id": %q}]}`,
		e.DE, e.Account(tb, "2000", "520000")), "")
	path := "/v1/requisitions/" + req["id"].(string)
	_, tag := e.Must(200, "GET", path, "", "")
	e.Must(200, "POST", path+":submit", "", tag)
	return uuid.MustParse(e.Pending(tb, req["id"].(string))["id"].(string))
}

// PartnerChange requests, as the tenant's human, a new customer named
// name and returns its change's pending approval request. The tenant
// needs partner.supplier.create and .read.
func (e Env) PartnerChange(tb testing.TB, name string) uuid.UUID {
	tb.Helper()
	p, _ := e.Must(201, "POST", "/v1/business-partners", fmt.Sprintf(`{"kind": "organization", "name": %q, "country_code": "DE", "roles": ["customer"], "reason": "New"}`, name), "")
	changes, _ := e.Must(200, "GET", "/v1/partner-changes?partner_id="+p["id"].(string), "", "")
	return uuid.MustParse(e.Pending(tb, apitest.List(tb, changes, "items")[0].(map[string]any)["id"].(string))["id"].(string))
}

// Open opens, as a, an approval request titled title for a requisition
// of amount minor units of EUR in company 2000 that no context holds: a
// request to show, as an agent's proposal would ask for one; approving it
// fails in procurement, which knows no such requisition.
func (e Env) Open(tb testing.TB, a pgtest.Actor, title string, amount money.Minor) uuid.UUID {
	tb.Helper()
	eur := "EUR"
	var id uuid.UUID
	if err := apitest.Committed(tb, e.Tenant, a, func(ctx context.Context) (err error) {
		id, _, err = Approvals().Open(ctx, approvals.NewRequest{ObjectType: approvals.Requisition, ObjectID: uuid.New(), Title: title,
			Amount: &amount, Currency: &eur, LegalEntityID: e.DE})
		return err
	}); err != nil {
		tb.Fatal(err)
	}
	return id
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
	GRIRAccounts(doc)
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
}

// GRIRAccounts adds GRIR's accounts to company 2000 of doc.
func GRIRAccounts(doc *config.Config) {
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "219000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense},
		config.Account{Number: "480000", Name: "FX gains", Type: config.Revenue}, config.Account{Number: "690000", Name: "FX losses", Type: config.Expense})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "219000", "590000"
	de.AccountDetermination.FXGain, de.AccountDetermination.FXLoss = "480000", "690000"
}

// Withholding gives company 2000 a withholding tax payable account
// (236000) and the codes, in the ledger, as an activated configuration
// with them would (T46f).
func (e Env) Withholding(tb testing.TB, codes ...config.WithholdingTaxCode) {
	tb.Helper()
	doc := Document(tb)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "236000", Name: "Withholding tax payable", Type: config.Liability})
	de.WithholdingTaxCodes = codes
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
}

// TaxCodes gives company 2000 the tax codes in the ledger, beside or in
// place of the prototype's codes of the same name, as an activated
// configuration with them would (T61a).
func (e Env) TaxCodes(tb testing.TB, codes ...config.TaxCode) {
	tb.Helper()
	TaxCodes(tb, e.Tenant, Document(tb), codes...)
}

// TaxCodes applies doc to the tenant's ledger with company 2000's tax codes
// beside or in place of doc's codes of the same name.
func TaxCodes(tb testing.TB, ten pgtest.Tenant, doc *config.Config, codes ...config.TaxCode) {
	tb.Helper()
	de := doc.Company("2000")
	for _, c := range codes {
		if i := slices.IndexFunc(de.TaxCodes, func(t config.TaxCode) bool { return t.Code == c.Code }); i >= 0 {
			de.TaxCodes[i] = c
		} else {
			de.TaxCodes = append(de.TaxCodes, c)
		}
	}
	if err := apitest.Committed(tb, ten, ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
}

// SupplierWithholding gives the supplier the withholding tax codes in
// company 2000, as a partner change would.
func (e Env) SupplierWithholding(tb testing.TB, supplier uuid.UUID, codes ...string) {
	tb.Helper()
	for _, c := range codes {
		pgtest.Exec(tb, `INSERT INTO partner.supplier_withholding (tenant_id, partner_id, legal_entity_id, code, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $5)`, e.Tenant.ID, supplier, e.DE, c, e.Tenant.Human.ID)
	}
}

// ActivateRules makes the tenant's active configuration a new version
// whose validation rules of entity are rs (ADR-0046), as an approved
// change would leave it, and returns the version's number.
func ActivateRules(tb testing.TB, tenant uuid.UUID, entity string, rs map[string]rules.Rule) int32 {
	tb.Helper()
	b, err := json.Marshal(rs)
	if err != nil {
		tb.Fatal(err)
	}
	pgtest.Exec(tb, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1,
			v.document || jsonb_build_object('rules', coalesce(v.document->'rules', '{}') || jsonb_build_object($2::text, $3::jsonb)),
			v.hash, 'rollback', v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		tenant, entity, string(b))
	pgtest.Exec(tb, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, tenant)
	var n int32
	pgtest.Scan(tb, `SELECT number FROM config.active_version WHERE tenant_id = $1`, []any{tenant}, &n)
	return n
}

// RuleAudit is an audit record of a write a validation rule refused or
// held.
type RuleAudit struct {
	ObjectType, Outcome, Rule string
	ObjectID                  *uuid.UUID // nil for a create refused
	ConfigVersion             int32
}

// RuleAudits are the tenant's audit records of action with source rule,
// in the order they were written.
func RuleAudits(tb testing.TB, tenant uuid.UUID, action string) []RuleAudit {
	tb.Helper()
	rows, err := pgtest.Admin(tb).Query(context.Background(), `SELECT object_type, outcome, detail->>'rule', object_id, (detail->>'config_version')::int
		FROM audit.audit_event WHERE tenant_id = $1 AND action = $2 AND detail->>'source' = 'rule' ORDER BY seq`, tenant, action)
	if err != nil {
		tb.Fatal(err)
	}
	var out []RuleAudit
	var a RuleAudit
	if _, err := pgx.ForEachRow(rows, []any{&a.ObjectType, &a.Outcome, &a.Rule, &a.ObjectID, &a.ConfigVersion}, func() error {
		out = append(out, a)
		return nil
	}); err != nil {
		tb.Fatal(err)
	}
	return out
}

// WantRuleAudit checks an audit record against the one wanted, one field
// at a time.
func WantRuleAudit(tb testing.TB, got, want RuleAudit) {
	tb.Helper()
	id := func(p *uuid.UUID) string {
		if p == nil {
			return "none"
		}
		return p.String()
	}
	for _, c := range []struct{ field, got, want string }{
		{"object type", got.ObjectType, want.ObjectType}, {"outcome", got.Outcome, want.Outcome}, {"rule", got.Rule, want.Rule},
		{"object id", id(got.ObjectID), id(want.ObjectID)}, {"config version", fmt.Sprint(got.ConfigVersion), fmt.Sprint(want.ConfigVersion)},
	} {
		if c.got != c.want {
			tb.Errorf("audit %s: got %s, want %s", c.field, c.got, c.want)
		}
	}
}
