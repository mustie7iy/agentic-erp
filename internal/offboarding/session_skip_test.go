package offboarding_test

import (
	"context"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestSessionsAreNotMoved: a tenant's sessions are ephemeral (ADR-0021 as
// amended by ADR-0031): they are left out of its fingerprint, as its
// outboxes are, and out of its export, so a move's verification does not
// depend on when its people last made a request.
func TestSessionsAreNotMoved(t *testing.T) {
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `INSERT INTO identity.session (tenant_id, id, principal_id, session_key, started_at, last_seen_at)
		VALUES ($1, gen_random_uuid(), $2, sha256('s'::bytea), now(), now())`, ten.ID, ten.Human.ID)
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	fp, err := offboarding.TakeFingerprint(ctx, tx, ten.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fp.Tables {
		if tc.Name == "identity.session" {
			t.Errorf("fingerprint: got identity.session (%d rows), want it left out", tc.Rows)
		}
	}
	if !offboarding.Ephemeral("identity.session") || offboarding.Ephemeral("identity.principal") {
		t.Error("ephemeral tables: want identity.session only")
	}
}
