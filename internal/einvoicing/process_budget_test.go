package einvoicing_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestProcessEInvoiceBudget measures a fetch's processing of one received
// document (ADR-0061 §14; T65c): reading its bytes, validating it as PINT
// AE, checking its buyer against the connector and the company's FTA
// registration, and recording the outcome with its audit record, in a
// connector that holds 10,000 documents processed before, done with (a UAE company's
// e-invoices of some years; the pilot's own volumes are not known yet,
// ADR-0052 facts). p95 250 ms, set from the measured median and p95 with
// room for a loaded machine, as earlier budgets were.
func TestProcessEInvoiceBudget(t *testing.T) {
	nfr.Measure(t)
	s := receiving(t)
	pgtest.Exec(t, `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
			created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, 'OLD-' || n, 'received', sha256(n::text::bytea), 1, 'k', gen_random_uuid(), $3, $3
		FROM generate_series(1, 10000) n`, s.f.Tenant.ID, s.connector, s.f.Tenant.Human.ID)
	// Done with: refused here, as nothing of them is read (since T65e a fetch
	// lists every received document, so a processed one still received
	// would be entered again).
	pgtest.Exec(t, `UPDATE einvoicing.einvoice SET status = 'refused', problem_code = 'einvoicing.unreadable', problem_detail = 'Not XML.',
		processed_at = now(), processed_fetch_operation_id = fetch_operation_id, validated_against = 'PINT AE Billing 1.0.4', version = version + 1
		WHERE tenant_id = $1 AND message_id LIKE 'OLD-%'`, s.f.Tenant.ID)
	ctx := context.Background()
	var times []time.Duration
	for i := range 20 {
		number := fmt.Sprintf("GOI-%d", i)
		s.srv.Deliver(number, uaeParticipant, einvoicing.RenderPINTAE(supplierInvoice(number)))
		op := uuid.Must(uuid.NewV7())
		s.recordAll(t, op)
		start := time.Now()
		if got, err := s.e.ProcessEInvoices(ctx, s.connector, op, s.within()); err != nil || got.Refused != 0 {
			t.Fatalf("process %s: got %d refused (%v), want it valid", number, got.Refused, err)
		}
		times = append(times, time.Since(start))
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("processing one e-invoice beside 10,000 processed: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(250 * time.Millisecond); p95 > budget {
		t.Fatalf("p95 %v exceeds the %v budget (250 ms scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
