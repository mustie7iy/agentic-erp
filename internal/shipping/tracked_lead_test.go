package shipping_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestTrackedOnTheLeadTime (batch 96S integration: S2j2 with S2f2 and
// S2b3): the sealant, managed in batches with 15 EA in stock (L1 10, L2
// 5) and a replenishment lead time of 10 days, ordered 20 EA for a day
// after the lead time ends, is confirmed in full, 5 EA of it on the lead
// time. Once batch L3 brings 10 EA, the delivery of the 20 EA is issued
// from L1, L2 and L3; its reversal holds the availability check's lock
// of the sealant at the plant until it commits, as an untracked
// material's does, and gives each batch back what it issued.
func TestTrackedOnTheLeadTime(t *testing.T) {
	e := setupTracked(t)
	e.leadTime(t, e.sealant, 10)
	order := e.askedFor(t, "2099-01-20", e.ofMaterial(e.sealant, "20 EA"))
	o := e.confirmedOn(t, order)
	if got, want := confirmedOf(t, o)+": "+onLeadOf(t, o), "20 EA: 5 EA until 2099-01-11"; got != want {
		t.Fatalf("confirmed: got %s, want %s", got, want)
	}
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{
		e.stockItem(e.sealant, "10", map[string]any{"batch_id": e.newBatch(t, e.sealant, "L3")})}}), "")
	id, tag := e.picked(t, order.String(), "1 20 EA")
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 10 EA", "L1"), part("1 5 EA", "L2"), part("1 5 EA", "L3")), tag)
	if got, want := e.stockByBatch(t, e.sealant), "L3 5"; got != want {
		t.Errorf("the batches after the goods issue: got %q, want %q", got, want)
	}
	h := endpoints(t)
	err := e.inRequest(t, func(ctx context.Context) error {
		if _, err := h.ReverseOutboundDeliveryGoodsIssue(ctx, gen.ReverseOutboundDeliveryGoodsIssueParams{ID: uuid.MustParse(id), IfMatch: tag}); err != nil {
			return err
		}
		var free bool // read on another connection: the reversal's transaction holds the lock
		pgtest.Scan(t, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
			[]any{fmt.Sprintf("sales.availability:%s:%s:%s", e.Tenant.ID, e.p1, e.sealant)}, &free)
		if free {
			t.Errorf("the sealant's availability lock during the reversal: got free, want held by the reversal")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.reverse(t, id, tag) // the request above was rolled back
	if got, want := e.stockByBatch(t, e.sealant), "L1 10, L2 5, L3 10"; got != want {
		t.Errorf("the batches after the reversal: got %q, want %q", got, want)
	}
}
