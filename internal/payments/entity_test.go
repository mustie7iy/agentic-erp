package payments_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// noEntity is a tenancy context that no longer finds the run's legal
// entity: payment_run.legal_entity_id has no foreign key into tenancy.
type noEntity struct{}

func (noEntity) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, nil
}

// TestProposeWithoutTheLegalEntity: a proposal whose legal entity the
// tenancy context no longer finds is refused, never built with end-to-end
// ids that begin with an empty entity code.
func TestProposeWithoutTheLegalEntity(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.propose(t, "2026-04-15", "2026-04-15")
	h := endpoints(nil)
	h.LegalEntities = noEntity{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		j, _, err := h.NextJob(ctx, tx)
		if err != nil {
			return err
		}
		return h.Process(ctx, j)
	})
	if want := "no legal entity " + e.DE.String(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("process: got %v, want an error naming %q", err, want)
	}
}

// TestReleaseWithoutTheLegalEntity: a release whose legal entity the
// tenancy context no longer finds writes no bank file, never one whose
// message id begins with an empty entity code.
func TestReleaseWithoutTheLegalEntity(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.decide(t, 202, run, "release", "")
	pay := uuidOf(id(e.payments(t, run)[0].(map[string]any)))
	h := endpoints(nil)
	h.LegalEntities = noEntity{}
	err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, pay); err != nil {
			return err
		}
		_, err = h.FinishRelease(ctx, r)
		return err
	})
	if want := "no legal entity " + e.DE.String(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("finish the release: got %v, want an error naming %q", err, want)
	}
}

// TestTransmitWithoutTheLegalEntity: a bank file whose run's legal entity
// the tenancy context no longer finds is not transmitted, never under the
// name "-<run>-<id>.xml"; the refusal names the entity.
func TestTransmitWithoutTheLegalEntity(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := uuidOf(id(files["items"].([]any)[0].(map[string]any)))
	h := endpoints(e.connector)
	h.LegalEntities = noEntity{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.TransmitFile(ctx, file, yes)
		return err
	})
	p := problem.From(err)
	if p.Status != 422 || p.Code != "request.reference_not_found" {
		t.Fatalf("transmit: got %d %s (%v), want 422 request.reference_not_found", p.Status, p.Code, err)
	}
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0].Message, e.DE.String()) {
		t.Errorf("transmit, field errors: got %+v, want one naming legal entity %s", p.Errors, e.DE)
	}
	if len(e.connector.files) != 0 {
		t.Errorf("files transmitted: got %v, want none", e.connector.files)
	}
}
