package db

import (
	"context"
	"testing"
)

// TestMarginReadPermission (T92b, migration 000296; ADR-0075 §5): the
// margin report's read permission is seeded, is no approval permission
// and is held by no role until it is granted.
func TestMarginReadPermission(t *testing.T) {
	need(t)
	var approval bool
	var roles int
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval, (SELECT count(*) FROM identity.role_permission WHERE permission_code = p.code)
		FROM ref.permission p WHERE code = 'controlling.margin.read'`).Scan(&approval, &roles); err != nil {
		t.Fatalf("controlling.margin.read: %v", err)
	}
	if approval {
		t.Errorf("controlling.margin.read is an approval permission, want a read permission")
	}
	if roles != 0 {
		t.Errorf("roles holding controlling.margin.read after the migration: got %d, want 0", roles)
	}
}
