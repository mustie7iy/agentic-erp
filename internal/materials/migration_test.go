package materials_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
)

// TestMigrationOutsideAUnitOfWork: the migration entry point refuses a
// call without a request's unit of work.
func TestMigrationOutsideAUnitOfWork(t *testing.T) {
	m := materials.Migration{Endpoints: &materials.Endpoints{}}
	calls := map[string]func(context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := m.CreateMaterial(ctx, materials.MigratedMaterial{})
			return err
		},
		"lock":   func(ctx context.Context) error { return m.LockMaterials(ctx, nil) },
		"extend": func(ctx context.Context) error { _, err := m.ExtendMaterial(ctx, uuid.New(), nil, ""); return err },
		"number": func(ctx context.Context) error { _, err := m.Number(ctx, uuid.New()); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Errorf("got nil error, want the call refused outside a unit of work")
			}
		})
	}
}
