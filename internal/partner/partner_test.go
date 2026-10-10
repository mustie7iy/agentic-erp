package partner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"partner.supplier.read", "partner.supplier.create", "partner.supplier.change", "partner.bank.change"}

// env is a tenant with legal entity 1000, its supplier reconciliation
// account 211000, an expense account and payment terms NT30 and NT60. The
// human may act in every legal entity; the service in none.
type env struct {
	apitest.Client
	ten            pgtest.Tenant
	le             uuid.UUID
	recon, expense uuid.UUID
	day            time.Time
}

const iban = "DE89370400440532013000"

func setup(t *testing.T) env {
	t.Helper()
	ten := pgtest.NewTenant(t, perms...)
	ten.AllCompanies(t, ten.Human)
	e := env{Client: apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}, ten: ten,
		le: uuid.Must(uuid.NewV7()), recon: uuid.Must(uuid.NewV7()), expense: uuid.Must(uuid.NewV7())}
	chart := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '1000', 'Acme', 'US', 'USD', gen_random_uuid(), 'active', $3, $3)`, ten.ID, e.le, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.chart_of_accounts (tenant_id, id, code, name, created_by) VALUES ($1, $2, '1000', 'Acme', $3)`, ten.ID, chart, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
		VALUES ($1, $2, $4, '211000', 'AP', 'liability', 'supplier', 'active', $5, $5), ($1, $3, $4, '520000', 'Office', 'expense', 'none', 'active', $5, $5)`,
		ten.ID, e.recon, e.expense, chart, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		VALUES ($1, $2, $3, $5, $5), ($1, $2, $4, $5, $5)`, ten.ID, e.le, e.recon, e.expense, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, created_by, updated_by)
		VALUES ($1, 'NT30', 'Net 30', 30, $2, $2), ($1, 'NT60', 'Net 60', 60, $2, $2)`, ten.ID, ten.Human.ID)
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT current_date`).Scan(&e.day); err != nil {
		t.Fatal(err)
	}
	// Partner changes are approved by the configuration's vendor master approver.
	doc, _ := json.Marshal(fixture.Document(t))
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, activated_by)
		VALUES ($1, gen_random_uuid(), 1, $2::jsonb, sha256(convert_to($2::text, 'UTF8')), 'bootstrap', $3)`, ten.ID, string(doc), ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO config.active_version (tenant_id, number) VALUES ($1, 1)`, ten.ID)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'vendor_master_approver', 'Approver', 'active', $2, $2)`, ten.ID, ten.Human.ID)
	return e
}

func (e env) date(days int) string { return e.day.AddDate(0, 0, days).Format(time.DateOnly) }

// supplier is the body of a supplier with company data in 1000, an address,
// a VAT number and a bank account valid from today+bankFrom.
func (e env) supplier(name string, bankFrom int) string {
	return fmt.Sprintf(`{"kind": "organization", "name": %q, "country_code": "DE", "roles": ["supplier"],
		"supplier_companies": [{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": "NT30"}],
		"addresses": [{"purpose": "legal", "line1": "Hauptstrasse 1", "city": "Berlin", "country_code": "DE", "valid_from": %q}],
		"tax_identifiers": [{"scheme": "EU_VAT", "value": "DE123456789"}],
		"bank_accounts": [{"account_number": "de89 3704 0044 0532 0130 00", "account_scheme": "iban", "bic": "COBADEFFXXX",
			"bank_country_code": "DE", "holder_name": %q, "valid_from": %q}],
		"extensions": {"cost_object_hint": "MKT"}, "reason": "New supplier"}`, name, e.le, e.recon, e.date(-30), name, e.date(bankFrom))
}

func id(m map[string]any) string { return m["id"].(string) }

// create posts a partner and returns it with its create change.
func (e env) create(t *testing.T, body string) (map[string]any, map[string]any) {
	t.Helper()
	p, _ := e.Must(201, "POST", "/v1/business-partners", body, "")
	changes, _ := e.Must(200, "GET", "/v1/partner-changes?partner_id="+id(p)+"&kind=create", "", "")
	return p, changes["items"].([]any)[0].(map[string]any)
}

// apply applies a change as the workflow context will, committed, and
// returns the failure as the problem a client would see.
func (e env) apply(t *testing.T, change string) error {
	t.Helper()
	err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := partner.ApplyChange(ctx, uuid.MustParse(change))
		return err
	})
	if err != nil {
		return problem.From(err)
	}
	return nil
}

func (e env) mustApply(t *testing.T, change string) {
	t.Helper()
	if err := e.apply(t, change); err != nil {
		t.Fatalf("apply %s: %v", change, err)
	}
}

// active creates a supplier and applies its create change.
func (e env) active(t *testing.T, name string, bankFrom int) map[string]any {
	t.Helper()
	p, c := e.create(t, e.supplier(name, bankFrom))
	e.mustApply(t, id(c))
	got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
	return got
}

func (e env) events(t *testing.T, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, e.ten, outbox.Event{Context: "partner", Name: name})
}

func TestCreateAndRead(t *testing.T) {
	e := setup(t)
	p, c := e.create(t, e.supplier("Muster Industrie GmbH", 0))
	if p["status"] != "pending" || p["number"] != "BP00000001" || p["kind"] != "organization" || len(p["supplier_companies"].([]any)) != 1 ||
		p["extensions"].(map[string]any)["cost_object_hint"] != "MKT" {
		t.Fatalf("partner: %v", p)
	}
	tax := p["tax_identifiers"].([]any)[0].(map[string]any)
	if tax["scheme"] != "EU_VAT" || tax["value_last4"] != "6789" || len(tax) != 2 {
		t.Fatalf("tax identifier not masked: %v", tax)
	}
	addr := p["addresses"].([]any)[0].(map[string]any)
	if addr["purpose"] != "legal" || addr["valid_from"] != e.date(-30) || addr["id"] == nil {
		t.Fatalf("address: %v", addr)
	}
	if c["number"] != "PC00000001" || c["status"] != "pending" || c["reason"] != "New supplier" || len(c["warnings"].([]any)) != 0 {
		t.Fatalf("change: %v", c)
	}
	payload := c["payload"].(map[string]any)
	bank := payload["bank_accounts"].([]any)[0].(map[string]any)
	if bank["account_last4"] != "3000" || bank["account_cipher"] != nil || bank["account_fingerprint"] != nil {
		t.Fatalf("bank account in payload not masked: %v", bank)
	}
	if strings.Contains(fmt.Sprint(payload), "DE123456789") || strings.Contains(fmt.Sprint(payload), iban) {
		t.Fatalf("payload leaks a sensitive value: %v", payload)
	}
	created := e.events(t, "business_partner_created")
	if len(created) != 1 || created[0]["partner_change_id"] != id(c) || created[0]["status"] != "pending" {
		t.Fatalf("events: %v", created)
	}
	if got, tag := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", ""); got["name"] != "Muster Industrie GmbH" || tag != `"1"` {
		t.Fatalf("get: %v %s", got, tag)
	}
	if got, tag := e.Must(200, "GET", "/v1/partner-changes/"+id(c), "", ""); got["kind"] != "create" || tag != `"2"` || got["workflow_request_id"] == nil {
		t.Fatalf("get change: %v %s", got, tag)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/business-partners/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/partner-changes/"+uuid.NewString(), "", "")

	// A person who is a customer, and the same identifiers again: warnings
	// once the first partner's bank account exists.
	e.mustApply(t, id(c))
	_, c2 := e.create(t, `{"kind": "person", "name": "Erika Mustermann", "legal_name": "Erika Mustermann", "language": "de", "country_code": "DE",
		"roles": ["customer"], "tax_identifiers": [{"scheme": "EU_VAT", "value": "de 123456789"}],
		"bank_accounts": [{"purpose": "collections", "account_number": "`+iban+`", "account_scheme": "iban", "bank_country_code": "DE",
			"holder_name": "E", "valid_from": "`+e.date(1)+`"}], "reason": "Customer"}`)
	codes := []string{}
	for _, w := range c2["warnings"].([]any) {
		codes = append(codes, w.(map[string]any)["code"].(string))
	}
	if strings.Join(codes, ",") != "partner.duplicate_tax_identifier,partner.duplicate_bank_account" {
		t.Fatalf("warnings: %v", c2["warnings"])
	}

	list := func(q string) []string {
		page, _ := e.Must(200, "GET", "/v1/business-partners"+q, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["number"].(string))
		}
		if next, ok := page["next_page_token"].(string); ok {
			out = append(out, "next:"+next)
		}
		return out
	}
	for q, want := range map[string]string{
		"":                                 "BP00000001,BP00000002",
		"?number=BP00000002":               "BP00000002",
		"?name_prefix=ERIKA":               "BP00000002",
		"?name_prefix=mu":                  "BP00000001",
		"?name_prefix=a_":                  "",
		"?role=customer":                   "BP00000002",
		"?status=pending":                  "BP00000002",
		"?order_by=name%20desc":            "BP00000001,BP00000002",
		"?order_by=name&page_size=1":       "BP00000002",
		"?status=active&role=supplier":     "BP00000001",
		"?order_by=number%20desc":          "BP00000002,BP00000001",
		"?role=customer&status=active":     "",
		"?name_prefix=muster%20%20indus":   "BP00000001",
		"?number=BP00000001&role=supplier": "BP00000001",
	} {
		got := list(q)
		if len(got) > 0 && strings.HasPrefix(got[len(got)-1], "next:") {
			got = got[:len(got)-1]
		}
		if strings.Join(got, ",") != want {
			t.Errorf("list %s: %v, want %s", q, got, want)
		}
	}
	first := list("?order_by=name&page_size=1")
	if len(first) != 2 {
		t.Fatalf("page 1: %v", first)
	}
	if rest := list("?order_by=name&page_size=1&page_token=" + strings.TrimPrefix(first[1], "next:")); strings.Join(rest, ",") != "BP00000001" {
		t.Fatalf("page 2: %v", rest)
	}
	if byNumber := list("?page_size=1"); len(byNumber) != 2 || list("?page_size=1&page_token=" + strings.TrimPrefix(byNumber[1], "next:"))[0] != "BP00000002" {
		t.Fatalf("by number: %v", byNumber)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/business-partners?order_by=status", "", "")

	// The service has no company access: it sees the partner, not its company data.
	svc := e.Client
	svc.As = e.ten.Service
	if got, _ := svc.Must(200, "GET", "/v1/business-partners/"+id(p), "", ""); len(got["supplier_companies"].([]any)) != 0 {
		t.Fatalf("company data outside access: %v", got)
	}
	if page, _ := svc.Must(200, "GET", "/v1/business-partners?number=BP00000001", "", ""); len(page["items"].([]any)[0].(map[string]any)["supplier_companies"].([]any)) != 0 {
		t.Fatalf("listed company data outside access: %v", page)
	}

	changes, _ := e.Must(200, "GET", "/v1/partner-changes?order_by=created_at%20desc&page_size=1", "", "")
	if changes["items"].([]any)[0].(map[string]any)["number"] != "PC00000002" || changes["next_page_token"] == nil {
		t.Fatalf("changes: %v", changes)
	}
	more, _ := e.Must(200, "GET", "/v1/partner-changes?order_by=created_at%20desc&page_size=1&page_token="+changes["next_page_token"].(string), "", "")
	if more["items"].([]any)[0].(map[string]any)["number"] != "PC00000001" {
		t.Fatalf("changes page 2: %v", more)
	}
	if applied, _ := e.Must(200, "GET", "/v1/partner-changes?status=applied", "", ""); len(applied["items"].([]any)) != 1 {
		t.Fatalf("applied changes: %v", applied)
	}
}

func TestCreateRefusals(t *testing.T) {
	e := setup(t)
	base := func(extra string) string {
		return `{"kind": "organization", "name": "X", "country_code": "DE", "roles": ["supplier"], "reason": "r"` + extra + `}`
	}
	company := func(le, account, term string) string {
		return fmt.Sprintf(`, "supplier_companies": [{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": %q}]`, le, account, term)
	}
	address := func(purpose, country, from, to string) string {
		s := fmt.Sprintf(`{"purpose": %q, "line1": "L", "city": "C", "country_code": %q, "valid_from": %q`, purpose, country, from)
		if to != "" {
			s += fmt.Sprintf(`, "valid_to": %q`, to)
		}
		return s + "}"
	}
	bank := func(purpose, number, scheme, from string) string {
		return fmt.Sprintf(`{"purpose": %q, "account_number": %q, "account_scheme": %q, "bank_country_code": "DE", "holder_name": "H", "valid_from": %q}`,
			purpose, number, scheme, from)
	}
	le, recon := e.le.String(), e.recon.String()
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"kind", strings.Replace(base(""), "organization", "robot", 1), 422, "partner.unknown_kind"},
		{"role", strings.Replace(base(""), `["supplier"]`, `["vendor"]`, 1), 422, "partner.unknown_role"},
		{"extension", base(`, "extensions": {"Bad Key": 1}`), 422, "request.invalid_extension"},
		{"country", strings.Replace(base(""), `"DE"`, `"QQ"`, 1), 422, "partner.unknown_country"},
		{"address country", base(`, "addresses": [` + address("legal", "QQ", e.date(0), "") + `]`), 422, "partner.unknown_country"},
		{"bank country", base(`, "bank_accounts": [` + strings.Replace(bank("payments", iban, "iban", e.date(0)), `"DE"`, `"QQ"`, 1) + `]`), 422, "partner.unknown_country"},
		{"no supplier role", strings.Replace(base(company(le, recon, "NT30")), `["supplier"]`, `["customer"]`, 1), 422, "partner.supplier_role_required"},
		{"duplicate company", base(`, "supplier_companies": [{"legal_entity_id": "` + le + `", "reconciliation_account_id": "` + recon + `", "payment_term_code": "NT30"},
			{"legal_entity_id": "` + le + `", "reconciliation_account_id": "` + recon + `", "payment_term_code": "NT30"}]`), 422, "partner.duplicate_company"},
		{"unknown legal entity", base(company(uuid.NewString(), recon, "NT30")), 422, "request.reference_not_found"},
		{"not a reconciliation account", base(company(le, e.expense.String(), "NT30")), 422, "partner.invalid_reconciliation_account"},
		{"payment term", base(company(le, recon, "NT99")), 422, "partner.unknown_payment_term"},
		{"address purpose", base(`, "addresses": [` + address("home", "DE", e.date(0), "") + `]`), 422, "partner.unknown_address_purpose"},
		{"address validity", base(`, "addresses": [` + address("legal", "DE", e.date(0), e.date(0)) + `]`), 422, "partner.invalid_validity"},
		{"address overlap", base(`, "addresses": [` + address("legal", "DE", e.date(-5), "") + `, ` + address("legal", "DE", e.date(-1), e.date(3)) + `]`), 409, "partner.address_overlap"},
		{"tax scheme twice", base(`, "tax_identifiers": [{"scheme": "EU_VAT", "value": "1"}, {"scheme": "EU_VAT", "value": "2"}]`), 422, "partner.duplicate_tax_scheme"},
		{"bank purpose", base(`, "bank_accounts": [` + bank("savings", iban, "iban", e.date(0)) + `]`), 422, "partner.unknown_bank_purpose"},
		{"bank purpose twice", base(`, "bank_accounts": [` + bank("payments", iban, "iban", e.date(0)) + `, ` + bank("payments", "12345678", "domestic", e.date(0)) + `]`), 422, "partner.duplicate_bank_purpose"},
		{"scheme", base(`, "bank_accounts": [` + bank("payments", iban, "swift", e.date(0)) + `]`), 422, "partner.unknown_account_scheme"},
		{"account number", base(`, "bank_accounts": [` + bank("payments", "1234-5678", "domestic", e.date(0)) + `]`), 422, "partner.invalid_account_number"},
		{"iban", base(`, "bank_accounts": [` + bank("payments", "DE00370400440532013000", "iban", e.date(0)) + `]`), 422, "partner.invalid_iban"},
		{"backdated bank account", base(`, "bank_accounts": [` + bank("payments", iban, "iban", e.date(-1)) + `]`), 422, "partner.backdated_bank_account"},
	}
	for _, c := range cases {
		if got, m, _ := e.Do("POST", "/v1/business-partners", c.body, ""); got != c.status || m["code"] != c.code {
			t.Errorf("%s: %d %v, want %d %s", c.name, got, m, c.status, c.code)
		}
	}
	// Supplier data only in legal entities the caller may act in.
	svc := e.Client
	svc.As = e.ten.Service
	svc.Problem(422, "request.reference_not_found", "POST", "/v1/business-partners", base(company(le, recon, "NT30")), "")
	if page, _ := e.Must(200, "GET", "/v1/business-partners", "", ""); len(page["items"].([]any)) != 0 {
		t.Fatalf("a refused request left a partner: %v", page)
	}
}

func TestApplyRejectCancel(t *testing.T) {
	e := setup(t)
	p, c := e.create(t, e.supplier("A", 5))
	e.mustApply(t, id(c))
	got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
	if got["status"] != "active" || got["version"] != float64(2) {
		t.Fatalf("applied create: %v", got)
	}
	applied, _ := e.Must(200, "GET", "/v1/partner-changes/"+id(c), "", "")
	if applied["status"] != "applied" || applied["applied_at"] == nil {
		t.Fatalf("change: %v", applied)
	}
	banks, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p)+"/bank-accounts", "", "")
	b := banks["items"].([]any)[0].(map[string]any)
	if b["valid_from"] != e.date(5) || b["account_last4"] != "3000" || b["partner_change_id"] != id(c) || b["bic"] != "COBADEFFXXX" {
		t.Fatalf("bank account: %v", b)
	}
	approved := e.events(t, "supplier_approved")
	if len(approved) != 1 || approved[0]["legal_entity_ids"].([]any)[0] != e.le.String() || approved[0]["status"] != "active" {
		t.Fatalf("supplier approved: %v", approved)
	}
	changed := e.events(t, "bank_account_changed")
	if len(changed) != 1 || changed[0]["ended_bank_account_id"] != nil || changed[0]["valid_from"] != e.date(5) {
		t.Fatalf("bank account changed: %v", changed)
	}
	for name, fn := range map[string]func(context.Context, uuid.UUID) (partner.Change, error){
		"apply": partner.ApplyChange, "reject": partner.RejectChange, "cancel": partner.CancelChange,
	} {
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := fn(ctx, uuid.MustParse(id(c)))
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "partner.invalid_status") {
			t.Errorf("%s an applied change: %v", name, err)
		}
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := partner.ApplyChange(ctx, uuid.New())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "request.not_found") {
		t.Fatalf("unknown change: %v", err)
	}

	// A customer without bank accounts, rejected; another cancelled.
	customer := `{"kind": "person", "name": "C", "country_code": "DE", "roles": ["customer"], "reason": "r"}`
	cp, cc := e.create(t, customer)
	if got, _ := e.Must(200, "GET", "/v1/partner-changes/"+id(cc), "", ""); got["workflow_request_id"] == nil {
		t.Fatalf("workflow request: %v", got)
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		ch, err := partner.RejectChange(ctx, uuid.MustParse(id(cc)))
		if err == nil && (ch.Status != "rejected" || ch.Version != 3) {
			t.Errorf("rejected: %+v", ch)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(cp), "", ""); got["status"] != "pending" {
		t.Fatalf("a rejected create changed the partner: %v", got)
	}
	_, cc2 := e.create(t, customer)
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := partner.CancelChange(ctx, uuid.MustParse(id(cc2)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := e.events(t, "partner_change_rejected"); len(r) != 1 || r[0]["previous_status"] != "pending" || r[0]["kind"] != "create" {
		t.Fatalf("rejected events: %v", r)
	}
	if c := e.events(t, "partner_change_cancelled"); len(c) != 1 || c[0]["status"] != "cancelled" {
		t.Fatalf("cancelled events: %v", c)
	}
	// Applying a customer's create publishes no supplier approval.
	_, cc3 := e.create(t, customer)
	e.mustApply(t, id(cc3))
	if len(e.events(t, "supplier_approved")) != 1 {
		t.Fatal("supplier approval for a customer")
	}
}

func TestBlockAndUnblock(t *testing.T) {
	e := setup(t)
	pending, _ := e.create(t, e.supplier("P", 0))
	e.Problem(409, "partner.invalid_status", "POST", "/v1/business-partners/"+id(pending)+":block", `{"reason": "r"}`, `"1"`)
	e.Problem(409, "partner.invalid_status", "POST", "/v1/business-partners/"+id(pending)+":unblock", `{"reason": "r"}`, `"1"`)
	e.Problem(404, "request.not_found", "POST", "/v1/business-partners/"+uuid.NewString()+":block", `{"reason": "r"}`, `"1"`)
	e.Problem(404, "request.not_found", "POST", "/v1/business-partners/"+uuid.NewString()+":unblock", `{"reason": "r"}`, `"1"`)

	p := e.active(t, "A", 0)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/business-partners/"+id(p)+":block", `{"reason": "r"}`, `"1"`)
	block, tag := e.Must(200, "POST", "/v1/business-partners/"+id(p)+":block", `{"reason": "Fraud suspected"}`, `"2"`)
	if block["kind"] != "block" || block["status"] != "applied" || block["applied_at"] == nil || tag != `"1"` ||
		block["payload"].(map[string]any)["scope"] != "all" {
		t.Fatalf("block: %v %s", block, tag)
	}
	if got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", ""); got["status"] != "blocked" {
		t.Fatalf("not blocked: %v", got)
	}
	if b := e.events(t, "partner_blocked"); len(b) != 1 || b[0]["reason"] != "Fraud suspected" || b[0]["legal_entity_id"] != nil {
		t.Fatalf("blocked events: %v", b)
	}
	e.Problem(409, "partner.invalid_status", "POST", "/v1/business-partners/"+id(p)+":block", `{"reason": "r"}`, `"3"`)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/business-partners/"+id(p)+":unblock", `{"reason": "r"}`, `"2"`)
	unblock, _ := e.Must(200, "POST", "/v1/business-partners/"+id(p)+":unblock", `{"reason": "Cleared"}`, `"3"`)
	if unblock["kind"] != "unblock" || unblock["status"] != "pending" {
		t.Fatalf("unblock: %v", unblock)
	}
	e.Problem(409, "partner.change_pending", "POST", "/v1/business-partners/"+id(p)+":unblock", `{"reason": "again"}`, `"3"`)
	e.mustApply(t, id(unblock))
	if got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", ""); got["status"] != "active" {
		t.Fatalf("not unblocked: %v", got)
	}
	if u := e.events(t, "partner_unblocked"); len(u) != 1 || u[0]["reason"] != "Cleared" || u[0]["scope"] != "all" {
		t.Fatalf("unblocked events: %v", u)
	}
	// An unblock whose partner is no longer blocked cannot be applied.
	e.Must(200, "POST", "/v1/business-partners/"+id(p)+":block", `{"reason": "r"}`, `"4"`)
	again, _ := e.Must(200, "POST", "/v1/business-partners/"+id(p)+":unblock", `{"reason": "r"}`, `"5"`)
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'active', version = version + 1 WHERE id = $1`, uuid.MustParse(id(p)))
	if err := e.apply(t, id(again)); err == nil || !strings.Contains(err.Error(), "partner.invalid_status") {
		t.Fatalf("unblock of an active partner: %v", err)
	}
}
