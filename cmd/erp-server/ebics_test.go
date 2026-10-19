package main

import (
	"context"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestProductionImportRefusesEBICS: a production import refuses a tenant
// without an active identity provider or with an active bank_ebics
// connector, as the cell would refuse its next start (ADR-0038, T44d).
func TestProductionImportRefusesEBICS(t *testing.T) {
	ten := pgtest.NewTenant(t)
	pool := pgtest.Pool(t)
	check := func() error {
		ctx := context.Background()
		tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return productionImport(ctx, tx, ten.ID)
	}
	if got := problem.From(check()).Code; got != "identity.provider_required" {
		t.Errorf("without a provider: got %s, want identity.provider_required", got)
	}
	pgtest.Exec(t, `INSERT INTO identity.identity_provider (tenant_id, id, issuer, audience, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, 'erp', $3, $3)`, ten.ID, "https://import-"+ten.ID.String()+".example.com", ten.Human.ID)
	if err := check(); err != nil {
		t.Errorf("with a provider: got %v, want imported", err)
	}
	connector := uuid.New()
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, $2, 'bank_ebics', 'EBICS', '{}', 'active', $3, $3)`, ten.ID, connector, ten.Human.ID)
	if got := problem.From(check()).Code; got != "integration.ebics_not_certified" {
		t.Errorf("with an active bank_ebics connector: got %s, want integration.ebics_not_certified", got)
	}
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, connector)
	if err := check(); err != nil {
		t.Errorf("with the connector disabled: got %v, want imported", err)
	}
}

// TestDisableConnectorCommand: erp-server disable-connector refuses
// incomplete arguments, a connector the tenant does not have (row-level
// security hides another tenant's) and an operator named by an email
// address (ADR-0026), and finds one already disabled done.
func TestDisableConnectorCommand(t *testing.T) {
	env := environment(t, authntest.New(t), "")
	getenv := func(k string) string { return env[k] }
	ten, other := pgtest.NewTenant(t), pgtest.NewTenant(t)
	connector, active := uuid.New(), uuid.New()
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, $2, 'bank_ebics', 'EBICS', '{}', 'disabled', $3, $3), ($1, $4, 'bank_ebics', 'EBICS 2', '{}', 'active', $3, $3)`,
		ten.ID, connector, ten.Human.ID, active)
	full := func(tenant, connector string) []string {
		return []string{"disable-connector", "-tenant", tenant, "-connector", connector, "-by", "ops", "-reason", "not certified"}
	}
	for name, c := range map[string]struct {
		args []string
		code int
	}{
		"help":             {[]string{"disable-connector", "-h"}, 0},
		"bad flag":         {[]string{"disable-connector", "-colour", "red"}, 2},
		"no connector":     {[]string{"disable-connector", "-tenant", ten.ID.String(), "-by", "ops", "-reason", "r"}, 2},
		"no tenant":        {[]string{"disable-connector", "-connector", connector.String(), "-by", "ops", "-reason", "r"}, 2},
		"no operator":      {[]string{"disable-connector", "-tenant", ten.ID.String(), "-connector", connector.String(), "-reason", "r"}, 2},
		"no reason":        {[]string{"disable-connector", "-tenant", ten.ID.String(), "-connector", connector.String(), "-by", "ops"}, 2},
		"another tenant's": {full(other.ID.String(), connector.String()), 1},
		"already disabled": {full(ten.ID.String(), connector.String()), 0},
		"an email address": {[]string{"disable-connector", "-tenant", ten.ID.String(), "-connector", active.String(), "-by", "ops@example.com", "-reason", "r"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if code := cli(context.Background(), c.args, getenv, io.Discard, io.Discard, nil); code != c.code {
				t.Errorf("exit: got %d, want %d", code, c.code)
			}
		})
	}
	t.Run("no database", func(t *testing.T) {
		cfg := load(t, env)
		cfg.DatabaseURL = "postgres://nobody@127.0.0.1:1/none?connect_timeout=1"
		if err := disableConnector(context.Background(), cfg, ten.ID, connector, "ops", "r"); err == nil {
			t.Error("got disabled, want the connection failure")
		}
	})
}
