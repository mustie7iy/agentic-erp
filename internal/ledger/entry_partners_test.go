package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// grantPartners lets the tenant's human read business partners, which an
// expansion of an entry's partners requires; the agent does not.
func grantPartners(tb testing.TB, ten pgtest.Tenant) {
	tb.Helper()
	pgtest.Exec(tb, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT $1, gen_random_uuid(), id, 'partner.supplier.read', current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`,
		ten.ID, ten.Human.ID)
}

// partnersOf is an entry's expanded partners as "number name" in order,
// and whether it had an expansion at all.
func partnersOf(doc map[string]any) ([]string, bool) {
	exp, ok := doc["expanded"].(map[string]any)
	if !ok {
		return nil, false
	}
	var out []string
	for _, p := range exp["partners"].([]any) {
		m := p.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s %s", m["id"], m["number"], m["name"]))
	}
	return out, true
}

// TestEntryPartners reads posted and parked entries with expand=partners
// (T73h3): each partner the lines name once, in the order of the lines,
// by number and name; nothing embedded when not asked, for a caller who
// may not read partners, one a segregation-of-duties rule bars from
// reading them, or for lines naming none; the posted entry's ETag the
// same either way.
func TestEntryPartners(t *testing.T) {
	e := setup(t)
	grantPartners(t, e.ten)
	e.ten.AllCompanies(t, e.ten.Agent)
	bank, ap, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "211000"), e.account(t, "2000", "400000")
	sup, ven := e.supplier.String(), e.vendor.String()
	want := []string{ven + " BP00000002 BP00000002", sup + " BP00000001 BP00000001"}
	body := entry(e.de, "2026-03-15", "EUR", line{bank, "3.00", ""}, line{ap, "-1.00", ven}, line{ap, "-1.00", sup}, line{ap, "-1.00", ven})
	plain := entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""})
	je, _ := e.Must(201, "POST", "/v1/journal-entries", body, "")
	bare, _ := e.Must(201, "POST", "/v1/journal-entries", plain, "")
	pe, _ := e.Must(201, "POST", "/v1/parked-journal-entries", body, "")
	pbare, _ := e.Must(201, "POST", "/v1/parked-journal-entries", plain, "")
	for _, c := range []struct {
		name, path string
		as         pgtest.Actor
		want       []string
	}{
		{"posted", "/v1/journal-entries/" + je["id"].(string) + "?expand=partners", e.ten.Human, want},
		{"parked", "/v1/parked-journal-entries/" + pe["id"].(string) + "?expand=partners", e.ten.Human, want},
		{"posted, not asked", "/v1/journal-entries/" + je["id"].(string), e.ten.Human, nil},
		{"parked, asked for another", "/v1/parked-journal-entries/" + pe["id"].(string) + "?expand=supplier", e.ten.Human, nil},
		{"posted, by an agent who may not read partners", "/v1/journal-entries/" + je["id"].(string) + "?expand=partners", e.ten.Agent, nil},
		{"parked, by an agent who may not read partners", "/v1/parked-journal-entries/" + pe["id"].(string) + "?expand=partners", e.ten.Agent, nil},
		{"posted, no partners", "/v1/journal-entries/" + bare["id"].(string) + "?expand=partners", e.ten.Human, nil},
		{"parked, no partners", "/v1/parked-journal-entries/" + pbare["id"].(string) + "?expand=partners", e.ten.Human, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := e.In(t)
			cl.As = c.as
			doc, _ := cl.Must(200, "GET", c.path, "", "")
			got, expanded := partnersOf(doc)
			if expanded != (c.want != nil) {
				t.Fatalf("expanded: got %v, want %v", expanded, c.want != nil)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("partners: got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("the posted entry's ETag is the entry's", func(t *testing.T) {
		_, plainTag := e.Must(200, "GET", "/v1/journal-entries/"+je["id"].(string), "", "")
		_, expandedTag := e.Must(200, "GET", "/v1/journal-entries/"+je["id"].(string)+"?expand=partners", "", "")
		if plainTag != expandedTag {
			t.Errorf("ETag: got %s expanded, want %s as without", expandedTag, plainTag)
		}
	})
	t.Run("a person a segregation-of-duties rule bars from reading partners", func(t *testing.T) {
		// Authorize would refuse them listBusinessPartners, so the entry
		// carries no copy of a partner either (04 §4.4).
		pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), 'ledger.entry.post', 'partner.supplier.read', 'Read partners or post', '2000-01-01', $2)`, e.ten.ID, e.ten.Human.ID)
		defer pgtest.Exec(t, `DELETE FROM identity.sod_rule WHERE tenant_id = $1 AND description = 'Read partners or post'`, e.ten.ID)
		for _, path := range []string{"/v1/journal-entries/" + je["id"].(string), "/v1/parked-journal-entries/" + pe["id"].(string)} {
			doc, _ := e.Must(200, "GET", path+"?expand=partners", "", "")
			if got, expanded := partnersOf(doc); expanded {
				t.Errorf("%s: got partners %v, want no expanded", path, got)
			}
		}
	})
	t.Run("a partner the tenant does not have is left out", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.parked_entry_line SET partner_id = $3 WHERE tenant_id = $1 AND parked_entry_id = $2 AND partner_id = $4`,
			e.ten.ID, pe["id"], uuid.Must(uuid.NewV7()), e.vendor)
		doc, _ := e.Must(200, "GET", "/v1/parked-journal-entries/"+pe["id"].(string)+"?expand=partners", "", "")
		if got, _ := partnersOf(doc); fmt.Sprint(got) != fmt.Sprint(want[1:]) {
			t.Errorf("partners: got %v, want %v", got, want[1:])
		}
	})
}

// TestEntryPartnersFailures fails each statement of the reads with their
// partners expanded.
func TestEntryPartnersFailures(t *testing.T) {
	e := setup(t)
	grantPartners(t, e.ten)
	bank, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "211000")
	body := entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{ap, "-1.00", e.supplier.String()})
	je, _ := e.Must(201, "POST", "/v1/journal-entries", body, "")
	pe, _ := e.Must(201, "POST", "/v1/parked-journal-entries", body, "")
	h, partners := endpoints(), []string{"partners"}
	for name, call := range map[string]func(ctx context.Context) error{
		"posted": func(ctx context.Context) error {
			_, err := h.GetJournalEntry(ctx, gen.GetJournalEntryParams{ID: uuid.MustParse(je["id"].(string)), Expand: partners})
			return err
		},
		"parked": func(ctx context.Context) error {
			_, err := h.GetParkedJournalEntry(ctx, gen.GetParkedJournalEntryParams{ID: uuid.MustParse(pe["id"].(string)), Expand: partners})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.ten, e.ten.Human, name, call) })
	}
}
