package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The group company a business partner is (T68c, ADR-0059 §4): set when
// the partner is created and changed or cleared by an update change, any
// legal entity of the tenant, shown by code and name, published and
// audited, and read by the ledger through partner.Postings.GroupCompany.

// withGroupCompany is the body of supplier name linked to the group company.
func (e env) withGroupCompany(name string, company uuid.UUID) string {
	body := e.supplier(name, 0)
	return strings.Replace(body, `"reason": "New supplier"`, fmt.Sprintf(`"group_company_id": %q, "reason": "New supplier"`, company), 1)
}

// groupCompanyOf is a partner representation's group company as "id code
// name", "-" for none, with group_company_id checked against it.
func groupCompanyOf(t *testing.T, p map[string]any) string {
	t.Helper()
	gc, ok := p["group_company"].(map[string]any)
	if !ok {
		if p["group_company_id"] != nil {
			t.Errorf("group_company_id: got %v without group_company, want neither", p["group_company_id"])
		}
		return "-"
	}
	if p["group_company_id"] != gc["id"] {
		t.Errorf("group_company_id: got %v, want %v, group_company's", p["group_company_id"], gc["id"])
	}
	return fmt.Sprintf("%v %v %v", gc["id"], gc["code"], gc["name"])
}

func TestGroupCompanyOnCreate(t *testing.T) {
	e := setup(t)
	acme := e.le.String() + " 1000 Acme"
	p, c := e.create(t, e.withGroupCompany("Acme Corp US", e.le))
	t.Run("the created partner names its group company", func(t *testing.T) {
		if got := groupCompanyOf(t, p); got != acme {
			t.Errorf("created: got %q, want %q", got, acme)
		}
	})
	t.Run("the create change records it", func(t *testing.T) {
		if got := c["payload"].(map[string]any)["group_company_id"]; got != e.le.String() {
			t.Errorf("change payload: got %v, want %s", got, e.le)
		}
	})
	t.Run("BusinessPartnerCreated carries it", func(t *testing.T) {
		got := ""
		for _, ev := range e.events(t, "business_partner_created") {
			if ev["partner_id"] == id(p) {
				got = fmt.Sprint(ev["group_company_id"])
			}
		}
		if got != e.le.String() {
			t.Errorf("event: got %q, want %s", got, e.le)
		}
	})
	t.Run("the audit record names it", func(t *testing.T) {
		var got string
		pgtest.Scan(t, `SELECT detail->>'group_company_id' FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'partner.business_partner.create'`,
			[]any{e.ten.ID, id(p)}, &got)
		if got != e.le.String() {
			t.Errorf("audit detail: got %q, want %s", got, e.le)
		}
	})
	e.mustApply(t, id(c))
	t.Run("read and listed with it", func(t *testing.T) {
		got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
		if g := groupCompanyOf(t, got); g != acme {
			t.Errorf("read: got %q, want %q", g, acme)
		}
		plain, _ := e.create(t, e.supplier("Other", 0))
		page, _ := e.Must(200, "GET", "/v1/business-partners?order_by=name", "", "")
		var listed []string
		for _, it := range page["items"].([]any) {
			listed = append(listed, groupCompanyOf(t, it.(map[string]any)))
		}
		if got, want := strings.Join(listed, " | "), acme+" | -"; got != want {
			t.Errorf("listed: got %q, want %q (%s without one)", got, want, id(plain))
		}
	})
	t.Run("a company of no tenant's is refused", func(t *testing.T) {
		p := e.Problem(422, "partner.group_company_unknown", "POST", "/v1/business-partners", e.withGroupCompany("Nobody", uuid.New()), "")
		if got := fmt.Sprint(p["errors"]); !strings.Contains(got, "group_company_id") {
			t.Errorf("field errors: got %s, want group_company_id", got)
		}
	})
}

func TestGroupCompanyChanges(t *testing.T) {
	e := setup(t)
	other := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Maple', 'CA', 'CAD', gen_random_uuid(), 'active', $3, $3)`, e.ten.ID, other, e.ten.Human.ID)
	p := e.active(t, "Maple Industries Canada", 0)
	pid := id(p)
	updates := func(t *testing.T) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, ev := range e.events(t, "business_partner_updated") {
			if ev["partner_id"] == pid {
				out = append(out, ev)
			}
		}
		return out
	}
	change := func(t *testing.T, value string) {
		t.Helper()
		c, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, `{"group_company_id": `+value+`}`), "")
		e.mustApply(t, id(c))
	}
	t.Run("linked by an update change alone", func(t *testing.T) {
		change(t, fmt.Sprintf("%q", other))
		got, _ := e.Must(200, "GET", "/v1/business-partners/"+pid, "", "")
		if g, want := groupCompanyOf(t, got), other.String()+" 3000 Maple"; g != want {
			t.Errorf("partner: got %q, want %q", g, want)
		}
		evs := updates(t)
		if got := fmt.Sprint(evs[len(evs)-1]["changed_fields"], " ", evs[len(evs)-1]["group_company_id"]); got != "[group_company_id] "+other.String() {
			t.Errorf("BusinessPartnerUpdated: got %s, want [group_company_id] %s", got, other)
		}
	})
	t.Run("the ledger reads it", func(t *testing.T) {
		if got := groupCompany(t, e, uuid.MustParse(pid)); got != other.String() {
			t.Errorf("GroupCompany: got %s, want %s", got, other)
		}
	})
	t.Run("a company of no tenant's is refused", func(t *testing.T) {
		p := e.Problem(422, "partner.group_company_unknown", "POST", "/v1/partner-changes", e.update(pid, fmt.Sprintf(`{"group_company_id": %q}`, uuid.New())), "")
		if got := fmt.Sprint(p["errors"]); !strings.Contains(got, "payload.group_company_id") {
			t.Errorf("field errors: got %s, want payload.group_company_id", got)
		}
	})
	t.Run("a change that keeps it leaves it", func(t *testing.T) {
		change(t, `null`)
		c, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, fmt.Sprintf(`{"group_company_id": %q}`, other)), "")
		e.mustApply(t, id(c))
		c, _ = e.Must(201, "POST", "/v1/partner-changes", e.update(pid, `{"name": "Maple Industries Canada Ltd."}`), "")
		e.mustApply(t, id(c))
		if got := groupCompany(t, e, uuid.MustParse(pid)); got != other.String() {
			t.Errorf("GroupCompany after a name change: got %s, want %s", got, other)
		}
	})
	t.Run("null unlinks it", func(t *testing.T) {
		change(t, `null`)
		got, _ := e.Must(200, "GET", "/v1/business-partners/"+pid, "", "")
		if g := groupCompanyOf(t, got); g != "-" {
			t.Errorf("partner: got %q, want none", g)
		}
		evs := updates(t)
		if ev, ok := evs[len(evs)-1]["group_company_id"]; !ok || ev != nil {
			t.Errorf("BusinessPartnerUpdated's group_company_id: got %v (present %v), want null", ev, ok)
		}
		if got := groupCompany(t, e, uuid.MustParse(pid)); got != "-" {
			t.Errorf("GroupCompany: got %s, want none", got)
		}
	})
	t.Run("a group company tenancy does not know is an error, not a name", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE partner.business_partner SET group_company_id = $2, version = version + 1 WHERE id = $1`, pid, uuid.New())
		err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := endpoints().GetBusinessPartner(ctx, gen.GetBusinessPartnerParams{ID: uuid.MustParse(pid)})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "no legal entity of the tenant has the id") {
			t.Errorf("got %v, want the unknown company reported", err)
		}
		pgtest.Exec(t, `UPDATE partner.business_partner SET group_company_id = NULL, version = version + 1 WHERE id = $1`, pid)
	})
	t.Run("a partner that does not exist is linked to none", func(t *testing.T) {
		if got := groupCompany(t, e, uuid.New()); got != "-" {
			t.Errorf("GroupCompany: got %s, want none", got)
		}
	})
}

// groupCompany is what the ledger's port reads for the partner, "-" for
// none.
func groupCompany(t *testing.T, e env, partnerID uuid.UUID) string {
	t.Helper()
	var got *uuid.UUID
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		got, err = partner.Postings{}.GroupCompany(ctx, partnerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		return "-"
	}
	return got.String()
}
