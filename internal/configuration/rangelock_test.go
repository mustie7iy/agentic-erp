package configuration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestApplyRangesLocksOnlyChanged (finfix, ADR-0048 As built): while a
// posting has drawn a number from a range and not committed, an
// activation whose document leaves that range as it is applies its
// ranges without waiting for it, so it never waits on a posting that in
// turn waits for the activation's account determination lock; one
// raising the range's last number waits for the posting, as it must.
func TestApplyRangesLocksOnlyChanged(t *testing.T) {
	f := fixture.New(t, perms...)
	var doc *config.Config
	if err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		d, _, err := configuration.Settings{}.Document(ctx)
		doc = d
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raised := *doc
	raised.NumberRanges = append([]config.NumberRange(nil), doc.NumberRanges...)
	found := false
	for i, n := range raised.NumberRanges {
		if n.Company == "2000" && n.DocumentType == "material_document" && n.FiscalYear == 2026 {
			raised.NumberRanges[i].Last, found = n.Last+1, true
		}
	}
	if !found {
		t.Fatal("the fixture's document has no material_document range of company 2000 for 2026")
	}
	apply := func(d *config.Config) error {
		return apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			tx, p, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
				return err
			}
			return configuration.ApplyRanges(ctx, tx, tenancy.Directory{}, p, d)
		})
	}
	var unchanged, raising error
	if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		if _, err := (configuration.Numbers{}).Next(ctx, f.DE, "material_document", 2026); err != nil {
			return err
		}
		unchanged, raising = apply(doc), apply(&raised)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if unchanged != nil {
		t.Errorf("ranges left as they are, beside a posting holding one: got %v, want applied without waiting", unchanged)
	}
	var pg *pgconn.PgError
	if !errors.As(raising, &pg) || pg.Code != "55P03" {
		t.Errorf("the range raised, beside a posting holding it: got %v, want lock_not_available (55P03): it waits for the posting", raising)
	}
}
