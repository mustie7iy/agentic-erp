package tenancy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// entityIn records a legal entity of the tenant in country.
func entityIn(t *testing.T, ten pgtest.Tenant, code, country string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $3, $4, 'EUR', $2, 'active', $5, $5)`, ten.ID, uuid.New(), code, country, ten.Human.ID)
}

// TestCountries: the countries of the tenant's own legal entities, each
// once, sorted; another tenant's stay out; a failed read fails.
func TestCountries(t *testing.T) {
	ten, other := pgtest.NewTenant(t), pgtest.NewTenant(t)
	entityIn(t, ten, "1000", "DE")
	entityIn(t, ten, "2000", "US")
	entityIn(t, ten, "3000", "DE")
	entityIn(t, other, "1000", "FR")
	read := func(wrap func(pgx.Tx) pgx.Tx) ([]string, error) {
		var out []string
		err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) (err error) {
			out, err = tenancy.Countries(ctx, wrap(tx))
			return err
		})
		return out, err
	}
	got, err := read(func(tx pgx.Tx) pgx.Tx { return tx })
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[DE US]" {
		t.Errorf("countries: got %v, want [DE US]", got)
	}
	if _, err := read(func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: 1} }); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("a failed read: got %v, want the injected error", err)
	}
}
