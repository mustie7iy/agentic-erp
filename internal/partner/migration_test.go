package partner_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// migrated is a supplier as a migration run creates one: no company data,
// its payments account valid from today+bankFrom, partner's UTC day.
func (e env) migrated(name string, bankFrom int) partner.NewPartner {
	return partner.NewPartner{Kind: "organization", Name: name, CountryCode: "DE", Roles: []string{"supplier"}, Reason: "Migration run",
		BankAccounts: []partner.BankInput{{Purpose: "payments", AccountNumber: iban, AccountScheme: "iban", BankCountryCode: "DE",
			HolderName: name, ValidFrom: e.day.AddDate(0, 0, bankFrom)}}}
}

// migrate creates the partner as partner.Migration does, committed, and
// returns its id or the problem a client would see.
func (e env) migrate(t *testing.T, in partner.NewPartner) (uuid.UUID, error) {
	t.Helper()
	var id uuid.UUID
	err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		id, err = partner.Migration{Ports: e.ports()}.Create(ctx, in)
		return err
	})
	if err != nil {
		return uuid.Nil, problem.From(err)
	}
	return id, nil
}

func (e env) ports() partner.Ports {
	return partner.Ports{Keys: apitest.FieldKeys(), Schemas: configuration.Settings{}, Rules: configuration.Settings{}}
}

// TestMigrationBackdating: the one exemption from the bank accounts'
// backdating control (product owner 2026-11-02; ADR-0051 As built). A
// partner partner.Migration creates may have its bank account start on the
// day before partner's UTC day, so a payment run dated on a Toronto
// company's day finds it on the cutover evening; two days before is
// refused even there, and every other path (the API's create and bank
// change, CreatePartner called directly, a bank change of the migrated
// partner) still refuses yesterday.
func TestMigrationBackdating(t *testing.T) {
	e := setup(t)
	yesterday := e.date(-1)
	var pid uuid.UUID
	t.Run("the migration's new partner starts its account yesterday", func(t *testing.T) {
		var err error
		if pid, err = e.migrate(t, e.migrated("Cutover GmbH", -1)); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		accounts, _ := e.Must(200, "GET", "/v1/business-partners/"+pid.String()+"/bank-accounts", "", "")
		items := accounts["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("bank accounts: got %d, want 1", len(items))
		}
		if got := items[0].(map[string]any)["valid_from"]; got != yesterday {
			t.Errorf("bank account valid_from: got %v, want %s", got, yesterday)
		}
		changes, _ := e.Must(200, "GET", "/v1/partner-changes?partner_id="+pid.String()+"&kind=create", "", "")
		c := changes["items"].([]any)[0].(map[string]any)
		if c["status"] != "applied" {
			t.Errorf("create change status: got %v, want applied", c["status"])
		}
		banks := c["payload"].(map[string]any)["bank_accounts"].([]any)
		if got := banks[0].(map[string]any)["valid_from"]; got != yesterday {
			t.Errorf("create change's bank account valid_from: got %v, want %s", got, yesterday)
		}
		var changed map[string]any
		for _, ev := range e.events(t, "bank_account_changed") {
			if ev["partner_id"] == pid.String() {
				changed = ev
			}
		}
		if changed == nil || changed["valid_from"] != yesterday {
			t.Errorf("bank_account_changed: got %v, want valid_from %s", changed, yesterday)
		}
		var actions string
		pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND object_id IN ($2, $3)`,
			[]any{e.ten.ID, pid, c["id"]}, &actions)
		if want := "partner.business_partner.create partner.change.apply"; actions != want {
			t.Errorf("audit of the migrated partner: got %q, want %q", actions, want)
		}
	})

	// code is the problem code an error carries, "" for none.
	code := func(err error) string {
		if err == nil {
			return ""
		}
		return problem.From(err).Code
	}
	refusals := []struct {
		name string
		call func(t *testing.T) string
	}{
		{"the migration, two days before", func(t *testing.T) string {
			t.Helper()
			_, err := e.migrate(t, e.migrated("Too Early GmbH", -2))
			return code(err)
		}},
		{"CreatePartner called directly, yesterday", func(t *testing.T) string {
			t.Helper()
			return code(apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, _, err := partner.CreatePartner(ctx, e.ports(), e.migrated("Direct GmbH", -1))
				return err
			}))
		}},
		{"the API's create, yesterday", func(t *testing.T) string {
			t.Helper()
			return status422(t, e.in(t), "/v1/business-partners", e.supplier("Api GmbH", -1))
		}},
		{"the API's bank change of the migrated partner, yesterday", func(t *testing.T) string {
			t.Helper()
			return status422(t, e.in(t), "/v1/business-partners/"+pid.String()+"/bank-accounts",
				fmt.Sprintf(`{"account_number": "12345678", "account_scheme": "domestic", "bank_country_code": "DE", "holder_name": "Cutover GmbH",
					"valid_from": %q, "reason": "Confirmed by call-back"}`, yesterday))
		}},
	}
	for _, r := range refusals {
		t.Run(r.name+" is refused", func(t *testing.T) {
			if got := r.call(t); got != "partner.backdated_bank_account" {
				t.Errorf("problem: got %q, want partner.backdated_bank_account", got)
			}
		})
	}
	// The approval path keeps the apply clamp: a create change requested
	// through the API for an account starting on its day and approved
	// after that day has passed (simulated by moving the requested day
	// back) starts the account on the day of approval, not before.
	t.Run("an approved create change starts its account no earlier than today", func(t *testing.T) {
		p, c := e.create(t, e.supplier("Approved GmbH", 0))
		pgtest.Unguarded(t, `UPDATE partner.partner_change SET payload = jsonb_set(payload, '{bank_accounts,0,valid_from}', to_jsonb($2::text)) WHERE id = $1`,
			id(c), yesterday)
		e.mustApply(t, id(c))
		accounts, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p)+"/bank-accounts", "", "")
		if got := accounts["items"].([]any)[0].(map[string]any)["valid_from"]; got != e.date(0) {
			t.Errorf("approved account valid_from: got %v, want %s", got, e.date(0))
		}
	})

	var early int
	pgtest.Scan(t, `SELECT count(*) FROM partner.bank_account WHERE tenant_id = $1 AND valid_from < $2`, []any{e.ten.ID, e.day}, &early)
	if early != 1 {
		t.Errorf("bank accounts starting before today: got %d, want 1, the migrated partner's", early)
	}

	apitest.EveryFailure(t, e.ten, e.ten.Human, "migrate a partner", func(ctx context.Context) error {
		_, err := partner.Migration{Ports: e.ports()}.Create(ctx, e.migrated("Failing GmbH", -1))
		return err
	})
}

// status422 posts body to path and returns the problem code, failing t
// unless the answer is 422.
func status422(t *testing.T, c env, path, body string) string {
	t.Helper()
	status, m, _ := c.Do("POST", path, body, "")
	if status != 422 {
		t.Errorf("POST %s: got status %d, want 422", path, status)
	}
	return fmt.Sprint(m["code"])
}
