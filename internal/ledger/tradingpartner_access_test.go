package ledger_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// A trading partner is named (company code and name) only to a caller who
// may act in that company; any other caller gets its id alone (product
// owner 2026-11-05, ADR-0059 As built), and the ETag does not depend on
// who reads.

// actingIn is a colleague of e's human, with the same roles, who may act
// in the legal entities only.
func (e env) actingIn(tb testing.TB, legalEntities ...uuid.UUID) env {
	tb.Helper()
	c := e.colleague(tb)
	pgtest.Exec(tb, `UPDATE identity.principal SET all_companies = false, version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.ten.ID, c.As.ID)
	for _, le := range legalEntities {
		pgtest.Exec(tb, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, e.ten.ID, c.As.ID, le)
	}
	return c
}

// tpCall is a request whose answer shows lines with trading partners:
// one representation, or a page of them (items).
type tpCall struct {
	method, path, body, ifMatch string
	status                      int
	items                       bool
}

// reps are the representations a call answers with.
func (c tpCall) reps(t *testing.T, e env) ([]map[string]any, string) {
	t.Helper()
	got, tag := e.in(t).Must(c.status, c.method, c.path, c.body, c.ifMatch)
	if !c.items {
		return []map[string]any{got}, tag
	}
	var out []map[string]any
	for _, it := range apitest.List(t, got, "items") {
		out = append(out, it.(map[string]any))
	}
	return out, tag
}

func TestTradingPartnerNamedOnlyWithAccess(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	us := e.us.String()
	lines := []partnerLine{{account: bank, amount: "5.00", tradingPartner: us}, {account: revenue, amount: "-5.00"}}
	je, tag := e.Must(201, "POST", "/v1/journal-entries", partnerEntry(e.de, "2026-03-15", lines...), "")
	parked, _ := e.Must(201, "POST", "/v1/parked-journal-entries", partnerEntry(e.de, "2026-03-15", lines...), "")
	template, _ := e.Must(201, "POST", "/v1/entry-templates", fmt.Sprintf(`{"legal_entity_id": %q, "name": "Intercompany charge", "currency": "EUR", "lines": %s}`,
		e.de, partnerLines(lines...)), "")
	jid, pid, tid := je["id"].(string), parked["id"].(string), template["id"].(string)
	named, bare := us+" 1000 Acme Corp US | -", us+" <nil> <nil> | -"
	reads := map[string]tpCall{
		"entry":         {method: "GET", path: "/v1/journal-entries/" + jid, status: 200},
		"entry list":    {method: "GET", path: "/v1/journal-entries?legal_entity_id=" + e.de.String() + "&trading_partner_id=" + us, status: 200, items: true},
		"parked":        {method: "GET", path: "/v1/parked-journal-entries/" + pid, status: 200},
		"parked list":   {method: "GET", path: "/v1/parked-journal-entries?legal_entity_id=" + e.de.String(), status: 200, items: true},
		"template":      {method: "GET", path: "/v1/entry-templates/" + tid, status: 200},
		"template list": {method: "GET", path: "/v1/entry-templates?legal_entity_id=" + e.de.String(), status: 200, items: true},
	}
	for who, c := range map[string]struct {
		caller env
		want   string
	}{
		"a caller who may act in the trading partner": {e.actingIn(t, e.de, e.us), named},
		"a caller who may not":                        {e.actingIn(t, e.de), bare},
	} {
		for name, call := range reads {
			t.Run(who+": "+name, func(t *testing.T) {
				reps, _ := call.reps(t, c.caller)
				if len(reps) == 0 {
					t.Fatal("no representation")
				}
				for _, r := range reps {
					if got := tradingPartnersOf(t, r); got != c.want {
						t.Errorf("lines: got %q, want %q", got, c.want)
					}
				}
			})
		}
	}
	reader := e.actingIn(t, e.de)
	t.Run("the ETag is the same whoever reads, and the reader's If-Match holds", func(t *testing.T) {
		_, got := reader.in(t).Must(200, "GET", "/v1/journal-entries/"+jid, "", "")
		if got != tag {
			t.Errorf("ETag: got %s, want %s, the poster's", got, tag)
		}
		rev, _ := reader.in(t).Must(200, "POST", "/v1/journal-entries/"+jid+":reverse", "", got)
		if g := tradingPartnersOf(t, rev); g != bare {
			t.Errorf("the reversal: got %q, want %q", g, bare)
		}
	})
	t.Run("the answers of the reader's writes give the id alone", func(t *testing.T) {
		for name, call := range map[string]tpCall{
			"posted":         {method: "POST", path: "/v1/journal-entries", body: partnerEntry(e.de, "2026-03-16", lines...), status: 201},
			"parked":         {method: "POST", path: "/v1/parked-journal-entries", body: partnerEntry(e.de, "2026-03-16", lines...), status: 201},
			"parked from it": {method: "POST", path: "/v1/entry-templates/" + tid + ":park", body: `{"posting_date": "2026-03-20"}`, ifMatch: "*", status: 201},
		} {
			t.Run(name, func(t *testing.T) {
				reps, _ := call.reps(t, reader)
				if got := tradingPartnersOf(t, reps[0]); got != bare {
					t.Errorf("lines: got %q, want %q", got, bare)
				}
				if raw := fmt.Sprint(reps[0]); strings.Contains(raw, "Acme Corp US") {
					t.Errorf("the answer names the company the caller may not act in: %s", raw)
				}
			})
		}
	})
}
