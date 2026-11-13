package db

import (
	"context"
	"testing"
)

// TestRecordsPermissions (T99a, migration 000309; ADR-0079 §4): the
// permission to read the retention status is seeded and is no approval
// permission.
func TestRecordsPermissions(t *testing.T) {
	need(t)
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'records.retention.read'`).Scan(&approval); err != nil || approval {
		t.Errorf("records.retention.read: got approval %v and %v, want seeded, no approval permission", approval, err)
	}
}
