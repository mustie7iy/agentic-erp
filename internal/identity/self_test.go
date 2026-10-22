package identity_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCurrentPrincipal: GET /v1/me tells a person, an agent and a service
// who they are, their companies and what they may do now, whatever their
// roles; a segregation-of-duties rule's pair shows as barred (ADR-0052 G2).
func TestCurrentPrincipal(t *testing.T) {
	ten := pgtest.NewTenant(t, "payables.invoice.read", "partner.bank.change", "payments.run.propose")
	api := apitest.New(t)
	// Two companies, granted in the order opposite to their ids': the
	// answer lists them sorted.
	entity, second := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, 'DE01', 'Muster GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $4, $4),
		       ($1, $3, 'DE02', 'Muster Zwei GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $4, $4)`, ten.ID, entity, second, ten.Human.ID)
	for _, le := range []uuid.UUID{second, entity} {
		pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, ten.ID, ten.Human.ID, le)
	}
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'partner.bank.change', 'payments.run.propose', 'd', current_date, $2)`, ten.ID, ten.Human.ID)
	pgtest.Exec(t, `UPDATE identity.principal SET email = 'person@example.com', version = version + 1 WHERE tenant_id = $1 AND id = $2`, ten.ID, ten.Human.ID)
	ten.AllCompanies(t, ten.Service)
	for _, c := range []struct {
		name         string
		as           pgtest.Actor
		companies    string
		all          bool
		permissions  string
		barred       string
		email, level any
	}{
		{"person", ten.Human, "[" + entity.String() + " " + second.String() + "]", false,
			"[identity.self.read identity.session.end payables.invoice.read]", "[partner.bank.change payments.run.propose]", "person@example.com", nil},
		// Agents and services hold no approval permission; NewTenant's
		// machine role gives them the rest, so the rule bars them too.
		{"agent", ten.Agent, "[]", false, "[identity.self.read payables.invoice.read]", "[partner.bank.change payments.run.propose]", nil, float64(2)},
		{"service", ten.Service, "[]", true, "[identity.self.read payables.invoice.read]", "[partner.bank.change payments.run.propose]", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			me, _ := apitest.Client{T: t, API: api, Tenant: ten, As: c.as}.Must(200, "GET", "/v1/me", "", "")
			for field, want := range map[string]any{"id": c.as.ID.String(), "tenant_id": ten.ID.String(), "kind": c.as.Kind, "display_name": c.as.Kind,
				"all_companies": c.all, "legal_entity_ids": c.companies, "permissions": c.permissions, "barred_permissions": c.barred,
				"email": c.email, "autonomy_level": c.level} {
				got := me[field]
				if list, ok := got.([]any); ok {
					got = fmt.Sprint(list)
				}
				if got != want {
					t.Errorf("%s: got %v, want %v", field, got, want)
				}
			}
		})
	}
	// An agent without a profile is at level 0, as authz reads it.
	pgtest.Exec(t, `DELETE FROM identity.agent_profile WHERE tenant_id = $1 AND principal_id = $2`, ten.ID, ten.Agent.ID)
	me, _ := apitest.Client{T: t, API: api, Tenant: ten, As: ten.Agent}.Must(200, "GET", "/v1/me", "", "")
	if me["autonomy_level"] != float64(0) {
		t.Errorf("agent without a profile: got level %v, want 0", me["autonomy_level"])
	}
}

// TestPrincipalNames: names and kinds of the tenant's principals, in the
// order asked and each once; an id of another tenant's principal is left
// out like one of none (ADR-0052 G3). Every principal may ask.
func TestPrincipalNames(t *testing.T) {
	ten, other := pgtest.NewTenant(t), pgtest.NewTenant(t)
	api := apitest.New(t)
	pgtest.Exec(t, `UPDATE identity.principal SET display_name = 'Avery Chen', email = 'avery@example.com', version = version + 1 WHERE tenant_id = $1 AND id = $2`,
		ten.ID, ten.Human.ID)
	asked := []uuid.UUID{ten.Agent.ID, other.Human.ID, ten.Human.ID, uuid.New(), ten.Agent.ID}
	path := "/v1/principal-names?ids=" + join(asked)
	for _, as := range []pgtest.Actor{ten.Human, ten.Agent, ten.Service} {
		t.Run(as.Kind, func(t *testing.T) {
			page, _ := apitest.Client{T: t, API: api, Tenant: ten, As: as}.Must(200, "GET", path, "", "")
			items := apitest.List(t, page, "items")
			want := []map[string]any{{"id": ten.Agent.ID.String(), "display_name": "agent", "kind": "agent"},
				{"id": ten.Human.ID.String(), "display_name": "Avery Chen", "kind": "human"}}
			if len(items) != len(want) {
				t.Fatalf("items: got %v, want %v", items, want)
			}
			for i, it := range items {
				if got := fmt.Sprint(it); got != fmt.Sprint(want[i]) {
					t.Errorf("item %d: got %s, want %v (id, name and kind alone)", i, got, want[i])
				}
			}
		})
	}
	c := apitest.Client{T: t, API: api, Tenant: ten, As: ten.Human}
	many := make([]uuid.UUID, 101)
	for i := range many {
		many[i] = uuid.New()
	}
	for name, query := range map[string]string{"no ids": "", "an empty id": "?ids=", "not a uuid": "?ids=x", "101 ids": "?ids=" + join(many)} {
		t.Run(name, func(t *testing.T) {
			if status, body, _ := c.In(t).Do("GET", "/v1/principal-names"+query, "", ""); status != 400 {
				t.Errorf("got %d %v, want 400", status, body)
			}
		})
	}
	page, _ := c.Must(200, "GET", "/v1/principal-names?ids="+join(many[:100]), "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("100 unknown ids: got %v, want none", items)
	}
}

func join(ids []uuid.UUID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return strings.Join(s, ",")
}
