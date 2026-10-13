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

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func provisioner(t *testing.T) *provisioning.Provisioner {
	t.Helper()
	keys := apitest.FieldKeys()
	return &provisioning.Provisioner{Pool: pgtest.Pool(t), AppRole: "erp_app", Keys: keys, Configuration: httpapi.Configuration(httpapi.Config{FieldKeys: keys})}
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
	if res.ConfigurationVersion != 1 || len(res.Roles) == 0 || res.AdministratorRoleID == uuid.Nil || len(res.LegalEntities) != 2 {
		t.Fatalf("%+v", res)
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
			t.Errorf("role %s missing: %v", want, codes)
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
	if keyed != 1 || format != 2 {
		t.Fatalf("tenant key: %d keys, house banks sealed with format %d", keyed, format)
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
		r := request(t)
		mutate(&r)
		_, err := p.Provision(ctx, r)
		if pr := problem.From(err); err == nil || pr.Status < 400 || pr.Status >= 500 {
			t.Errorf("%s: %v", name, err)
		}
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
	before := count(t, `SELECT count(*) FROM tenancy.tenant`)
	p := provisioner(t)
	p.Now = time.Now
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
	if _, err := p.Provision(ctx, request(t)); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("failed commit: %v", err)
	}
	if count(t, `SELECT count(*) FROM tenancy.tenant`) != before+1 {
		t.Fatal("a failed commit left a tenant behind")
	}
}

// uncommittable is a transaction whose commit fails.
type uncommittable struct{ pgx.Tx }

func (uncommittable) Commit(context.Context) error { return errors.New("connection lost") }
