package main

import (
	"context"
	"testing"

	"github.com/google/uuid"

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
