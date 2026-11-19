package configuration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestNumberRangeLock: Lock holds a range's row until the transaction
// ends, as drawing a number does, and draws none; a range the
// configuration lacks is Next's 422 config.no_number_range, and every
// database failure is reported.
func TestNumberRangeLock(t *testing.T) {
	f := fixture.New(t, perms...)
	t.Run("holds the range, draws no number", func(t *testing.T) {
		var before, after int64
		pgtest.Scan(t, `SELECT next_value FROM config.number_range WHERE tenant_id = $1 AND legal_entity_id = $2 AND document_type = 'material_document'
			AND fiscal_year = 2026`, []any{f.Tenant.ID, f.DE}, &before)
		err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			if err := (configuration.Numbers{}).Lock(ctx, f.DE, "material_document", 2026); err != nil {
				return err
			}
			tx, err := pgtest.Admin(t).Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }() // the probe changes nothing: a failed rollback leaves nothing behind
			_, err = tx.Exec(ctx, `SELECT FROM config.number_range WHERE tenant_id = $1 AND legal_entity_id = $2 AND document_type = 'material_document'
				AND fiscal_year = 2026 FOR UPDATE NOWAIT`, f.Tenant.ID, f.DE)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "55P03" {
				t.Errorf("another transaction locking the range: got %v, want lock_not_available (55P03)", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		pgtest.Scan(t, `SELECT next_value FROM config.number_range WHERE tenant_id = $1 AND legal_entity_id = $2 AND document_type = 'material_document'
			AND fiscal_year = 2026`, []any{f.Tenant.ID, f.DE}, &after)
		if after != before {
			t.Errorf("next number after the lock: got %d, want %d", after, before)
		}
	})
	t.Run("no range", func(t *testing.T) {
		err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			return (configuration.Numbers{}).Lock(ctx, f.DE, "material_document", 2999)
		})
		if pr := (*problem.Problem)(nil); !errors.As(err, &pr) || pr.Code != "config.no_number_range" {
			t.Errorf("a range the configuration lacks: got %v, want config.no_number_range", err)
		}
	})
	t.Run("database failures", func(t *testing.T) {
		apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "lock a number range", func(ctx context.Context) error {
			return (configuration.Numbers{}).Lock(ctx, f.DE, "material_document", 2026)
		})
	})
	t.Run("outside a unit of work", func(t *testing.T) {
		if err := (configuration.Numbers{}).Lock(context.Background(), uuid.New(), "material_document", 2026); !errors.Is(err, reqctx.ErrOutsideRequest) {
			t.Errorf("lock outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
		}
	})
}
