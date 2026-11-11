package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// receivables adds legal entity 1000's customer reconciliation account
// 120000 and returns its id.
func (e env) receivables(t *testing.T) uuid.UUID {
	t.Helper()
	account := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
		SELECT $1, $2, chart_id, '120000', 'AR', 'asset', 'customer', 'active', $4, $4 FROM ledger.gl_account WHERE id = $3`,
		e.ten.ID, account, e.recon, e.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`,
		e.ten.ID, e.le, account, e.ten.Human.ID)
	return account
}

// customerData is one entry of customer_companies in legal entity 1000;
// extra adds fields, each led by a comma.
func (e env) customerData(account uuid.UUID, term, extra string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": %q%s}`, e.le, account, term, extra)
}

// customer is the body of a customer with the company data given.
func customer(name, companies string) string {
	return fmt.Sprintf(`{"kind": "organization", "name": %q, "country_code": "CA", "roles": ["customer"], "customer_companies": [%s], "reason": "New customer"}`,
		name, companies)
}

// companyOf is the partner's one entry of the list (supplier_companies or
// customer_companies) as "account term blocked".
func companyOf(t *testing.T, p map[string]any, list string) string {
	t.Helper()
	items := apitest.List(t, p, list)
	if len(items) != 1 {
		t.Fatalf("%s: got %d entries, want 1 (%v)", list, len(items), items)
	}
	c, _ := items[0].(map[string]any) // a nil map prints as missing fields below
	return fmt.Sprint(c["legal_entity_id"], " ", c["reconciliation_account_id"], " ", c["payment_term_code"], " ", c["is_posting_blocked"])
}

// asCustomer reads the partner as receivables and credit do, and what the
// ledger answers about posting to it as a customer.
func (e env) asCustomer(t *testing.T, partnerID string) (c partner.Customer, found bool, block string) {
	t.Helper()
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		if c, found, err = (partner.Customers{}).Customer(ctx, uuid.MustParse(partnerID), e.le); err != nil {
			return err
		}
		block, err = partner.Postings{}.PostingBlock(ctx, uuid.MustParse(partnerID), e.le, "customer")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c, found, block
}

// TestCustomerCompanyData is T74a's main path: a customer is created with
// its data in a company, approved and read back; an approved update
// replaces the data, the posting block with it; the change is audited and
// published, and receivables' read and the ledger's posting check see what
// was approved.
func TestCustomerCompanyData(t *testing.T) {
	e := setup(t)
	ar := e.receivables(t)
	p, c := e.create(t, customer("Northwind Traders", e.customerData(ar, "NT30", "")))
	pid := id(p)
	want := fmt.Sprint(e.le, " ", ar, " NT30 false")

	t.Run("created pending with its company data", func(t *testing.T) {
		if p["status"] != "pending" {
			t.Errorf("status: got %v, want pending", p["status"])
		}
		if got := companyOf(t, p, "customer_companies"); got != want {
			t.Errorf("customer company: got %s, want %s", got, want)
		}
		if n := len(apitest.List(t, p, "supplier_companies")); n != 0 {
			t.Errorf("supplier companies of a customer: got %d, want none", n)
		}
		payload, _ := c["payload"].(map[string]any)
		if got := companyOf(t, payload, "customer_companies"); got != fmt.Sprint(e.le, " ", ar, " NT30 <nil>") {
			t.Errorf("the create change's customer company: got %s, want the data requested, no block", got)
		}
		if cust, _, block := e.asCustomer(t, pid); cust.Usable() != "the business partner is pending" || block != "the business partner is pending" {
			t.Errorf("a pending customer: got usable %q and posting block %q, want both to say it is pending", cust.Usable(), block)
		}
	})

	t.Run("approved, audited and published", func(t *testing.T) {
		e.in(t).mustApply(t, id(c))
		got, _ := e.in(t).Must(200, "GET", "/v1/business-partners/"+pid, "", "")
		if got["status"] != "active" {
			t.Errorf("status after approval: got %v, want active", got["status"])
		}
		if company := companyOf(t, got, "customer_companies"); company != want {
			t.Errorf("customer company after approval: got %s, want %s", company, want)
		}
		approved := e.events(t, "customer_approved")
		if len(approved) != 1 {
			t.Fatalf("CustomerApproved: got %d events, want 1", len(approved))
		}
		if ev := fmt.Sprint(approved[0]["partner_id"], " ", approved[0]["number"], " ", approved[0]["status"], " ", approved[0]["legal_entity_ids"]); ev !=
			fmt.Sprint(pid, " ", p["number"], " active [", e.le, "]") {
			t.Errorf("CustomerApproved: got %s, want the partner, its number, active and legal entity %s", ev, e.le)
		}
		if n := len(e.events(t, "supplier_approved")); n != 0 {
			t.Errorf("SupplierApproved for a customer: got %d events, want none", n)
		}
		if created := e.events(t, "business_partner_created"); len(created) != 1 || fmt.Sprint(created[0]["roles"]) != "[customer]" {
			t.Errorf("BusinessPartnerCreated: got %v, want one event with roles [customer]", created)
		}
		var actions string
		pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND object_id IN ($2, $3)`,
			[]any{e.ten.ID, pid, id(c)}, &actions)
		if actions != "partner.business_partner.create partner.change.apply" {
			t.Errorf("audit: got %q, want the creation and the change's approval", actions)
		}
	})

	t.Run("read by receivables", func(t *testing.T) {
		cust, found, block := e.asCustomer(t, pid)
		wantCust := partner.Customer{ID: uuid.MustParse(pid), Number: p["number"].(string), Name: "Northwind Traders", Status: partner.Active,
			IsCustomer: true, SetUp: true, ReconciliationID: ar, PaymentTermCode: "NT30"}
		if !found || cust != wantCust {
			t.Errorf("customer: got %+v (found %v), want %+v", cust, found, wantCust)
		}
		if cust.Usable() != "" || block != "" {
			t.Errorf("an active customer set up in the company: got usable %q and posting block %q, want neither", cust.Usable(), block)
		}
	})

	update := func(t *testing.T, data string) map[string]any {
		t.Helper()
		ch, _ := e.in(t).Must(201, "POST", "/v1/partner-changes", e.update(pid, `{"customer_companies": [`+data+`]}`), "")
		return ch
	}
	t.Run("an approved update blocks postings", func(t *testing.T) {
		ch := update(t, e.customerData(ar, "NT60", `, "is_posting_blocked": true`))
		if _, _, block := e.asCustomer(t, pid); block != "" {
			t.Errorf("posting block while the change is pending: got %q, want none", block)
		}
		e.in(t).mustApply(t, id(ch))
		got, _ := e.in(t).Must(200, "GET", "/v1/business-partners/"+pid, "", "")
		if company := companyOf(t, got, "customer_companies"); company != fmt.Sprint(e.le, " ", ar, " NT60 true") {
			t.Errorf("customer company after the update: got %s, want term NT60, blocked", company)
		}
		const blocked = "the customer is blocked for posting in this legal entity"
		if cust, _, block := e.asCustomer(t, pid); cust.Usable() != blocked || block != blocked || cust.PaymentTermCode != "NT60" {
			t.Errorf("a blocked customer: got usable %q, posting block %q and term %s, want %q twice and NT60", cust.Usable(), block, cust.PaymentTermCode, blocked)
		}
		updated := e.events(t, "business_partner_updated")
		if len(updated) != 1 || fmt.Sprint(updated[0]["changed_fields"]) != "[customer_company."+e.le.String()+"]" {
			t.Errorf("BusinessPartnerUpdated: got %v, want one event naming customer_company.%s", updated, e.le)
		}
	})

	t.Run("data given without the block lifts it", func(t *testing.T) {
		e.in(t).mustApply(t, id(update(t, e.customerData(ar, "NT60", ""))))
		if cust, _, block := e.asCustomer(t, pid); cust.IsPostingBlocked || block != "" {
			t.Errorf("after the data was replaced without a block: got blocked %v and posting block %q, want neither", cust.IsPostingBlocked, block)
		}
	})

	t.Run("the service sees no company data", func(t *testing.T) {
		svc := e.in(t).Client
		svc.As = e.ten.Service
		if got, _ := svc.Must(200, "GET", "/v1/business-partners/"+pid, "", ""); len(apitest.List(t, got, "customer_companies")) != 0 {
			t.Errorf("customer company data outside the caller's companies: got %v, want none", got["customer_companies"])
		}
		page, _ := svc.Must(200, "GET", "/v1/business-partners?ids="+pid, "", "")
		if listed, _ := apitest.List(t, page, "items")[0].(map[string]any); len(apitest.List(t, listed, "customer_companies")) != 0 {
			t.Errorf("listed customer company data outside the caller's companies: got %v, want none", listed["customer_companies"])
		}
	})
}

// TestCustomersRead: what receivables and credit learn of a partner that
// is no usable customer of the company, and that a customer without
// company data stays postable by journal entries (plan-breadth-week1,
// default 2).
func TestCustomersRead(t *testing.T) {
	e := setup(t)
	ar := e.receivables(t)
	supplier := e.active(t, "Supplier", 0)
	bare, bc := e.create(t, `{"kind": "person", "name": "Bare", "country_code": "DE", "roles": ["customer"], "reason": "r"}`)
	e.mustApply(t, id(bc))
	both, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(id(supplier), `{"roles": ["supplier", "customer"], "customer_companies": [`+
		e.customerData(ar, "NT30", "")+`]}`), "")
	cases := []struct {
		name, partner string
		usable, block string
		setUp         bool
	}{
		{"a supplier", id(supplier), "the business partner is not a customer", "the business partner does not have the customer role", false},
		{"a customer without company data", id(bare), "the customer is not set up in this legal entity", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cust, found, block := e.asCustomer(t, tc.partner)
			if !found || cust.Usable() != tc.usable || block != tc.block || cust.SetUp != tc.setUp {
				t.Errorf("got found %v, usable %q, posting block %q, set up %v; want true, %q, %q, %v", found, cust.Usable(), block, cust.SetUp,
					tc.usable, tc.block, tc.setUp)
			}
		})
	}
	t.Run("no such partner", func(t *testing.T) {
		if cust, found, _ := e.asCustomer(t, uuid.NewString()); found || cust != (partner.Customer{}) {
			t.Errorf("got %+v (found %v), want no customer", cust, found)
		}
	})
	t.Run("a supplier that becomes a customer too", func(t *testing.T) {
		e.in(t).mustApply(t, id(both))
		cust, _, block := e.asCustomer(t, id(supplier))
		if cust.Usable() != "" || block != "" || cust.ReconciliationID != ar {
			t.Errorf("got usable %q, posting block %q and account %s; want a usable customer on %s", cust.Usable(), block, cust.ReconciliationID, ar)
		}
		got, _ := e.in(t).Must(200, "GET", "/v1/business-partners/"+id(supplier), "", "")
		if s, c := companyOf(t, got, "supplier_companies"), companyOf(t, got, "customer_companies"); s != fmt.Sprint(e.le, " ", e.recon, " NT30 false") ||
			c != fmt.Sprint(e.le, " ", ar, " NT30 false") {
			t.Errorf("company data of both roles: got supplier %s and customer %s, want each on its own reconciliation account", s, c)
		}
	})
	if _, _, err := (partner.Customers{}).Customer(context.Background(), uuid.New(), e.le); err == nil {
		t.Error("Customer outside a request: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "customer", func(ctx context.Context) error {
		_, _, err := partner.Customers{}.Customer(ctx, uuid.MustParse(id(supplier)), e.le)
		return err
	})
}

// TestCustomerCompanyRefusals: each rule of a customer's company data is
// refused with its code, naming the request's own field, on creation and
// in an update change.
func TestCustomerCompanyRefusals(t *testing.T) {
	e := setup(t)
	ar := e.receivables(t)
	active := e.active(t, "Supplier only", 0)
	good := e.customerData(ar, "NT30", "")
	unknown := strings.Replace(good, e.le.String(), uuid.NewString(), 1)
	cases := []struct {
		name, companies string
		code, field     string
	}{
		{"a supplier reconciliation account", e.customerData(e.recon, "NT30", ""), "partner.invalid_reconciliation_account", "customer_companies[0].reconciliation_account_id"},
		{"an account that reconciles nothing", e.customerData(e.expense, "NT30", ""), "partner.invalid_reconciliation_account", "customer_companies[0].reconciliation_account_id"},
		{"an unknown payment term", e.customerData(ar, "NT99", ""), "partner.unknown_payment_term", "customer_companies[0].payment_term_code"},
		{"a legal entity twice", good + ", " + good, "partner.duplicate_company", "customer_companies[1].legal_entity_id"},
		{"an unknown legal entity", unknown, "request.reference_not_found", "customer_companies[0].legal_entity_id"},
	}
	refused := func(t *testing.T, path, body, code, field string) {
		t.Helper()
		m := e.in(t).Problem(422, code, "POST", path, body, "")
		errs, _ := m["errors"].([]any)
		if len(errs) != 1 {
			t.Fatalf("errors: got %v, want one naming %s", m["errors"], field)
		}
		if got, _ := errs[0].(map[string]any); got["field"] != field {
			t.Errorf("field: got %v, want %s", got["field"], field)
		}
	}
	for _, tc := range cases {
		t.Run("create: "+tc.name, func(t *testing.T) {
			refused(t, "/v1/business-partners", customer("X", tc.companies), tc.code, tc.field)
		})
		t.Run("update: "+tc.name, func(t *testing.T) {
			refused(t, "/v1/partner-changes", e.update(id(active), `{"roles": ["supplier", "customer"], "customer_companies": [`+tc.companies+`]}`),
				tc.code, "payload."+tc.field)
		})
	}
	t.Run("create: without the customer role", func(t *testing.T) {
		refused(t, "/v1/business-partners", strings.Replace(customer("X", good), `["customer"]`, `["supplier"]`, 1), "partner.customer_role_required", "roles")
	})
	t.Run("update: without the customer role", func(t *testing.T) {
		refused(t, "/v1/partner-changes", e.update(id(active), `{"customer_companies": [`+good+`]}`), "partner.customer_role_required", "payload.roles")
	})
	t.Run("create: a legal entity outside the caller's companies", func(t *testing.T) {
		svc := e.in(t).Client
		svc.As = e.ten.Service
		svc.Problem(422, "request.reference_not_found", "POST", "/v1/business-partners", customer("X", good), "")
	})
	if page, _ := e.Must(200, "GET", "/v1/business-partners?role=customer", "", ""); len(apitest.List(t, page, "items")) != 0 {
		t.Errorf("a refused request left a customer: %v", page["items"])
	}
	if page, _ := e.Must(200, "GET", "/v1/partner-changes?kind=update", "", ""); len(apitest.List(t, page, "items")) != 0 {
		t.Errorf("a refused request left an update change: %v", page["items"])
	}
}
