package payments_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// noEntity is a tenancy context that no longer finds the run's legal
// entity: payment_run.legal_entity_id has no foreign key into tenancy.
type noEntity struct{}

func (noEntity) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, nil
}

// missingOnce is a tenancy context that does not find legal entity id the
// first time it is asked, and finds every entity as real does after.
type missingOnce struct {
	real    payments.LegalEntities
	id      uuid.UUID
	missing *atomic.Bool
}

func (m missingOnce) Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	if id == m.id && m.missing.CompareAndSwap(true, false) {
		return tenancy.Entity{}, false, nil
	}
	return m.real.Entity(ctx, id)
}

// TestProposeWithoutTheLegalEntity: a proposal whose legal entity the
// tenancy context does not find fails, with a reason naming the entity,
// never built with end-to-end ids that begin with an empty entity code;
// and it fails for good, so the tenant's next proposal is still built
// rather than queued behind it.
func TestProposeWithoutTheLegalEntity(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	first := e.propose(t, "2026-04-15", "2026-04-15")
	second := e.propose(t, "2026-04-16", "2026-04-16")
	h := endpoints(nil)
	missing := &atomic.Bool{}
	missing.Store(true)
	h.LegalEntities = missingOnce{real: h.LegalEntities, id: e.DE, missing: missing}
	w := proposer(t)
	w.Endpoints = h
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("proposer: %v", err)
	}
	op, _ := e.Must(200, "GET", "/v1/operations/"+id(first), "", "")
	if op["status"] != "failed" {
		t.Fatalf("first proposal, status: got %v, want failed: %v", op["status"], op)
	}
	why := op["error"].(map[string]any)
	if why["code"] != "request.reference_not_found" {
		t.Errorf("first proposal, error: got %v, want request.reference_not_found", why["code"])
	}
	// The entity went missing after the request was accepted: the reason
	// names it, on no field of the request.
	if detail := fmt.Sprint(why["detail"]); !strings.Contains(detail, e.DE.String()) {
		t.Errorf("first proposal, detail: got %q, want one naming legal entity %s", detail, e.DE)
	}
	if errs, _ := why["errors"].([]any); len(errs) != 0 {
		t.Errorf("first proposal, field errors: got %v, want none", errs)
	}
	if r, _ := e.Must(200, "GET", second["resource"].(string), "", ""); r["status"] != "proposed" {
		t.Errorf("second proposal, run status: got %v, want proposed", r["status"])
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
// name "-<run>-<id>.xml". The run exists, so its missing entity is a
// server-side fault (500, as the file's download answers), not a 422 on a
// field the client never sent; the error names the entity.
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
	if p := problem.From(err); p.Status != 500 {
		t.Fatalf("transmit: got %d %s (%v), want 500", p.Status, p.Code, err)
	}
	if want := "no legal entity " + e.DE.String(); !strings.Contains(err.Error(), want) {
		t.Errorf("transmit: got %v, want an error naming %q", err, want)
	}
	if len(e.connector.files) != 0 {
		t.Errorf("files transmitted: got %v, want none", e.connector.files)
	}
}
