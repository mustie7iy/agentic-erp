package db

import (
	"context"
	"testing"
)

// TestMarginReadPermission (T92b, migration 000308; T92a, migration
// 000331; ADR-0075 §5): the margin report's and the profitability
// report's read permissions are seeded, are no approval permissions and
// are held by no role until they are granted.
func TestMarginReadPermission(t *testing.T) {
	need(t)
	for _, code := range []string{"controlling.margin.read", "controlling.profitability.read"} {
		t.Run(code, func(t *testing.T) {
			var approval bool
			var roles int
			if err := testDB.QueryRow(context.Background(), `SELECT is_approval, (SELECT count(*) FROM identity.role_permission WHERE permission_code = p.code)
				FROM ref.permission p WHERE code = $1`, code).Scan(&approval, &roles); err != nil {
				t.Fatalf("%s: %v", code, err)
			}
			if approval {
				t.Errorf("%s is an approval permission, want a read permission", code)
			}
			if roles != 0 {
				t.Errorf("roles holding %s after the migration: got %d, want 0", code, roles)
			}
		})
	}
}
