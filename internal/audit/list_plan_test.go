package audit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestObjectHistoryReadsOnlyItsEvents: the audit trail of one object reads
// that object's events, not the tenant's whole trail. The list's optional
// filters would get a generic plan after five runs of a cached statement,
// so it runs six times before it is measured.
func TestObjectHistoryReadsOnlyItsEvents(t *testing.T) {
	c := setup(t)
	object := uuid.New()
	// The seeded events are not a hash chain: the chain verifier would find
	// this tenant's trail broken. No test here verifies this tenant's chain,
	// and the scheduled verifier's test only asserts that some chain is good.
	pgtest.Exec(t, `INSERT INTO audit.audit_event (tenant_id, seq, actor_id, actor_kind, action, object_type, object_id, outcome, hash)
		SELECT $1::uuid, 1000 + g, $2::uuid, 'human', 'test.thing.do', 'test.thing', CASE WHEN g = 1 THEN $3::uuid ELSE gen_random_uuid() END,
			'succeeded', sha256(g::text::bytea)
		FROM generate_series(1, 2000) g`, c.Tenant.ID, c.Tenant.Human.ID, object)
	pgtest.Exec(t, `ANALYZE audit.audit_event`)
	h := endpoints()
	params := gen.ListAuditEventsParams{ObjectType: gen.NewOptString("test.thing"), ObjectID: gen.NewOptUUID(object)}
	if err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
		for range 6 {
			if _, err := h.ListAuditEvents(ctx, params); err != nil {
				return err
			}
		}
		tx, _ := reqctx.TxFrom(ctx) // InRequest always puts its transaction in the context
		before := pgtest.RowsRead(t, ctx, tx, "audit", `audit\_event\_%`)
		res, err := h.ListAuditEvents(ctx, params)
		if err != nil {
			return err
		}
		if got := pgtest.RowsRead(t, ctx, tx, "audit", `audit\_event\_%`) - before; got != 1 {
			t.Errorf("events read for an object with one event: got %d, want 1", got)
		}
		page, ok := res.(*gen.AuditEventPage)
		if !ok {
			t.Fatalf("list answer: got %T, want *gen.AuditEventPage", res)
		}
		if got := len(page.Items); got != 1 {
			t.Errorf("events of the object: got %d, want 1", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
}
