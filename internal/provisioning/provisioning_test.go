package provisioning_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func provisioner(t *testing.T) *provisioning.Provisioner {
	t.Helper()
	keys := apitest.FieldKeys()
	return &provisioning.Provisioner{Pool: pgtest.Pool(t), AppRole: "erp_app", Keys: keys, Issuers: []string{authntest.URL(t)}, Configuration: app.Configuration(app.Config{FieldKeys: keys})}
}

func request(t *testing.T) provisioning.Request {
	t.Helper()
	email := "ada@example.com"
	return provisioning.Request{Name: "Acme", Region: "eu-west", GroupCurrency: "eur",
		Administrator: provisioning.Administrator{Subject: "idp|ada", DisplayName: "Ada", Email: &email}, Document: fixture.Document(t)}
}

// TestProvisionedTenantIsManagedThroughTheAPI provisions a tenant and has
// its administrator set it up through the API.
func TestProvisionedTenantIsManagedThroughTheAPI(t *testing.T) {
	ctx := context.Background()
	res, err := provisioner(t).Provision(ctx, request(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.ConfigurationVersion != 1 {
		t.Errorf("ConfigurationVersion = %d, want 1", res.ConfigurationVersion)
	}
	if len(res.Roles) == 0 {
		t.Error("Roles: got none, want the configured roles")
	}
	if res.AdministratorRoleID == uuid.Nil {
		t.Error("AdministratorRoleID: got nil, want an id")
	}
	if len(res.LegalEntities) != 2 {
		t.Errorf("LegalEntities: got %d, want 2", len(res.LegalEntities))
	}
	ten := pgtest.Tenant{ID: res.TenantID}
	admin := pgtest.Actor{ID: res.AdministratorID, Subject: "idp|ada", Kind: "human"}
	c := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: admin}
	// The administrator may do anything: read the configuration, create a legal entity, provision a colleague.
	versions, _ := c.Must(200, "GET", "/v1/config-versions?is_active=true", "", "")
	if len(versions["items"].([]any)) != 1 {
		t.Fatalf("versions: %v", versions)
	}
	le, _ := c.Must(201, "POST", "/v1/legal-entities", `{"code": "3000", "name": "Acme Ltd", "country_code": "GB", "local_currency": "GBP", "fiscal_calendar_id": "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01"}`, "")
	if le["status"] != "inactive" {
		t.Fatalf("legal entity: %v", le)
	}
	// The configuration's companies are legal entities ready to post once the administrator opens a period.
	de := res.LegalEntities["2000"].String()
	periods, _ := c.Must(200, "GET", "/v1/fiscal-periods?legal_entity_id="+de+"&fiscal_year=2026&page_size=20", "", "")
	for _, it := range periods["items"].([]any) {
		period := it.(map[string]any)
		if period["period"] == float64(6) {
			version, _ := json.Marshal(period["version"])
			c.Must(200, "POST", "/v1/fiscal-periods/"+period["id"].(string)+":open", "", `"`+string(version)+`"`)
		}
	}
	expense, bank := account(t, res.TenantID, "2000", "520000"), account(t, res.TenantID, "2000", "100000")
	c.Must(201, "POST", "/v1/journal-entries", `{"legal_entity_id": "`+de+`", "document_date": "2026-06-15", "posting_date": "2026-06-15",
		"currency": "EUR", "lines": [{"account_id": "`+expense+`", "amount": {"amount": "1.00", "currency": "EUR"}},
		{"account_id": "`+bank+`", "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, "")
	c.Must(201, "POST", "/v1/principals", `{"kind": "human", "external_subject": "idp|bob", "display_name": "Bob"}`, "")
	roles, _ := c.Must(200, "GET", "/v1/roles?page_size=50", "", "")
	codes := map[string]bool{}
	for _, r := range roles["items"].([]any) {
		codes[r.(map[string]any)["code"].(string)] = true
	}
	for _, want := range append([]string{provisioning.AdministratorRole}, res.Roles...) {
		if !codes[want] {
			t.Errorf("role %s: got roles %v, want it among them", want, codes)
		}
	}
	// The tenant has its own field-encryption key, and its house banks are sealed with it.
	var keyed, format int
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT count(*) FROM tenancy.tenant_key WHERE tenant_id = $1 AND status = 'active'`, res.TenantID).Scan(&keyed); err != nil {
		t.Fatalf("count active tenant keys: %v", err)
	}
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT get_byte(iban_cipher, 0) FROM payments.house_bank WHERE tenant_id = $1 LIMIT 1`, res.TenantID).Scan(&format); err != nil {
		t.Fatalf("read the house bank seal format: %v", err)
	}
	if keyed != 1 {
		t.Errorf("active tenant keys: got %d, want 1", keyed)
	}
	if format != 2 {
		t.Errorf("house bank seal format: got %d, want 2", format)
	}
	// The tenant announced itself, and the audit trail starts with the administrator.
	if ev := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "tenant_status_changed"}); len(ev) != 1 || ev[0]["status"] != "active" {
		t.Fatalf("events: %v", ev)
	}
	trail, _ := c.Must(200, "GET", "/v1/audit-events?order_by=occurred_at&page_size=1", "", "")
	first := trail["items"].([]any)[0].(map[string]any)
	if first["actor_id"] != res.AdministratorID.String() {
		t.Fatalf("first audit event: %v", first)
	}
	// Anyone else is unknown to the tenant.
	stranger := apitest.Client{T: t, API: c.API, Tenant: ten, As: pgtest.Actor{ID: uuid.New(), Subject: "idp|eve", Kind: "human"}}
	stranger.Problem(403, "identity.principal_unknown", "GET", "/v1/roles", "", "")
	// A second tenant is independent, even with the same administrator subject.
	if again, err := provisioner(t).Provision(ctx, request(t)); err != nil || again.TenantID == res.TenantID {
		t.Fatalf("second tenant: %+v %v", again, err)
	}
}

func account(t *testing.T, tenant uuid.UUID, company, number string) string {
	t.Helper()
	var id string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT a.id FROM ledger.gl_account a JOIN ledger.chart_of_accounts c ON c.id = a.chart_id
		WHERE c.tenant_id = $1 AND c.code = $2 AND a.number = $3`, tenant, company, number).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestProvisioningRefusals(t *testing.T) {
	ctx := context.Background()
	p := provisioner(t)
	for name, mutate := range map[string]func(r *provisioning.Request){
		"name":             func(r *provisioning.Request) { r.Name = " " },
		"region":           func(r *provisioning.Request) { r.Region = "" },
		"currency":         func(r *provisioning.Request) { r.GroupCurrency = "EURO" },
		"subject":          func(r *provisioning.Request) { r.Administrator.Subject = "" },
		"admin":            func(r *provisioning.Request) { r.Administrator.DisplayName = "" },
		"document":         func(r *provisioning.Request) { r.Document = nil },
		"unknown currency": func(r *provisioning.Request) { r.GroupCurrency = "XXX" },
		"bad region":       func(r *provisioning.Request) { r.Region = "EU West" },
		"invalid document": func(r *provisioning.Request) { r.Document.Companies[0].InvoiceTolerance.PricePercent = 500 },
	} {
		t.Run(name, func(t *testing.T) {
			r := request(t)
			mutate(&r)
			_, err := p.Provision(ctx, r)
			if err == nil {
				t.Fatal("Provision: got nil error, want a 4xx problem")
			}
			if pr := problem.From(err); pr.Status < 400 || pr.Status >= 500 {
				t.Errorf("status: got %d (%v), want 4xx", pr.Status, err)
			}
		})
	}
	// A cell that trusts several tenant issuers cannot tell which one the
	// administrator's subject belongs to (ADR-0010), so it refuses.
	several := provisioner(t)
	several.Issuers = append(several.Issuers, "https://other.example")
	_, err := several.Provision(ctx, request(t))
	if pr := problem.From(err); pr.Code != "provisioning.issuer_ambiguous" {
		t.Errorf("several issuers: got %v, want provisioning.issuer_ambiguous", err)
	}
	// A provisioner given no issuer is a wiring mistake, not an ambiguous
	// cell: it fails closed with a server error that says so.
	unwired := provisioner(t)
	unwired.Issuers = nil
	_, err = unwired.Provision(ctx, request(t))
	if pr := problem.From(err); pr.Status != 500 {
		t.Errorf("no issuer: got status %d (%v), want 500", pr.Status, err)
	}
	if err == nil {
		t.Fatal("no issuer: got provisioned, want refused")
	}
	if !strings.Contains(err.Error(), "no tenant issuer") {
		t.Errorf("no issuer: got %v, want an error naming the missing tenant issuer", err)
	}
}

// TestProvisioningIsAllOrNothing fails every statement in turn: the error
// surfaces and nothing of the tenant stays.
func TestProvisioningIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	admin := pgtest.Admin(t)
	count := func(t *testing.T, sql string) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	p := provisioner(t)
	p.Now = time.Now
	// With an identity provider too: its registration's statements fail
	// in turn as well.
	withProvider := request(t)
	withProvider.IdentityProvider = &identity.NewIdentityProvider{Issuer: "https://all-or-nothing-" + uuid.NewString() + ".example.com", Audience: "erp-api"}
	if !everyStatementFails(t, p, pool, withProvider, count) {
		return
	}
	before := count(t, `SELECT count(*) FROM tenancy.tenant`)
	for n := 1; ; n++ {
		var failing *pgtest.FailingTx
		p.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			failing = &pgtest.FailingTx{Tx: tx, FailAt: n}
			return failing, err
		}
		_, err := p.Provision(ctx, request(t))
		if err == nil {
			if failing.Calls() >= n {
				t.Fatalf("statement %d of %d failed, but provisioning succeeded: a database error was ignored", n, failing.Calls())
			}
			if n < 20 {
				t.Fatalf("succeeded after %d statements", n)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) && !strings.Contains(err.Error(), pgtest.ErrInjected.Error()) {
			t.Fatalf("statement %d: %v", n, err)
		}
		if count(t, `SELECT count(*) FROM tenancy.tenant`) != before {
			t.Fatalf("statement %d left a tenant behind", n)
		}
	}
	if count(t, `SELECT count(*) FROM tenancy.tenant`) != before+1 {
		t.Fatal("the last attempt did not commit")
	}
	p.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("no database") }
	if _, err := p.Provision(ctx, request(t)); err == nil {
		t.Fatal("began without a database")
	}
	// A commit that fails (a connection lost at the very end) leaves nothing either.
	p.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return uncommittable{tx}, err
	}
	_, err := p.Provision(ctx, request(t))
	pgtest.WantErr(t, err, "commit")
	if count(t, `SELECT count(*) FROM tenancy.tenant`) != before+1 {
		t.Fatal("a failed commit left a tenant behind")
	}
}

// uncommittable is a transaction whose commit fails.
type uncommittable struct{ pgx.Tx }

func (uncommittable) Commit(context.Context) error { return errors.New("connection lost") }

// everyStatementFails provisions req with its first, second, ... statement
// failing until it succeeds, checking that every failure is reported and
// leaves no tenant.
func everyStatementFails(t *testing.T, p *provisioning.Provisioner, pool *database.Pool, req provisioning.Request, count func(*testing.T, string) int) bool {
	t.Helper()
	ctx := context.Background()
	before := count(t, `SELECT count(*) FROM tenancy.tenant`)
	defer func() { p.Begin = nil }()
	for n := 1; n < 2000; n++ {
		p.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: n}, err
		}
		if _, err := p.Provision(ctx, req); err == nil {
			return count(t, `SELECT count(*) FROM tenancy.tenant`) == before+1
		} else if !strings.Contains(err.Error(), pgtest.ErrInjected.Error()) {
			t.Errorf("statement %d: %v", n, err)
			return false
		}
		if count(t, `SELECT count(*) FROM tenancy.tenant`) != before {
			t.Errorf("statement %d left a tenant behind", n)
			return false
		}
	}
	t.Error("still failing after 2000 statements")
	return false
}
