package payments_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// runOf is the status of the run and of the operation, "run/operation".
func runOf(t *testing.T, run, op string) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT r.status || '/' || o.status FROM payments.payment_run r, platform.operation o WHERE r.id = $1 AND o.id = $2`,
		[]any{run, op}, &got)
	return got
}

// listed reports whether list, as the worker role across tenants, names
// the operation.
func listed(t *testing.T, list func(context.Context, pgx.Tx) ([]operations.Waiting, error), op string) bool {
	t.Helper()
	found := false
	err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_worker", ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		waiting, err := list(ctx, tx)
		for _, w := range waiting {
			found = found || w.ID == uuid.MustParse(op)
		}
		return err
	})
	if err != nil {
		t.Fatalf("list the waiting operations: %v", err)
	}
	return found
}

// TestProposerLeavesInactiveTenants: the proposal worker leaves a
// suspended tenant's queued proposal as it is, not listed for a workflow
// either, while an active tenant's is built, and builds it once the tenant
// is active again (ADR-0015).
func TestProposerLeavesInactiveTenants(t *testing.T) {
	active, suspended := setup(t), setup(t)
	opA := active.propose(t, "2026-04-15", "2026-04-15")
	opS := suspended.propose(t, "2026-04-15", "2026-04-15")
	runA, runS := uuidOf(opA["resource"].(string)[len("/v1/payment-runs/"):]), uuidOf(opS["resource"].(string)[len("/v1/payment-runs/"):])
	suspended.Tenant.SetStatus(t, "suspended")
	if listed(t, payments.Queued, id(opS)) {
		t.Error("suspended: got the proposal listed for a workflow, want it left")
	}
	work(t)
	if got := runOf(t, runA.String(), id(opA)); got != "proposed/succeeded" {
		t.Errorf("the active tenant's run: got %s, want proposed/succeeded", got)
	}
	if got := runOf(t, runS.String(), id(opS)); got != "proposing/queued" {
		t.Errorf("the suspended tenant's run: got %s, want proposing/queued", got)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, suspended.Tenant.ID)
	if !listed(t, payments.Queued, id(opS)) {
		t.Error("resumed: got the proposal unlisted, want it listed for a workflow")
	}
	work(t)
	if got := runOf(t, runS.String(), id(opS)); got != "proposed/succeeded" {
		t.Errorf("the resumed tenant's run: got %s, want proposed/succeeded", got)
	}
}

// TestReleaserLeavesInactiveTenants: the release worker leaves a
// suspended tenant's queued release as it is, not listed for a workflow
// either, while an active tenant's run is released, and releases it once
// the tenant is active again; a second round does nothing more.
func TestReleaserLeavesInactiveTenants(t *testing.T) {
	active, suspended := setup(t), setup(t)
	var runs [2]map[string]any
	var ops [2]map[string]any
	for i, e := range []env{active, suspended} {
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
		runs[i] = e.run(t, "2026-04-15", "2026-04-15")
		ops[i] = e.decide(t, 202, runs[i], "release", "")
	}
	suspended.Tenant.SetStatus(t, "suspended")
	if listed(t, payments.Queued, id(ops[1])) {
		t.Error("suspended: got the release listed for a workflow, want it left")
	}
	if _, err := active.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release round: %v", err)
	}
	if got := runOf(t, id(runs[0]), id(ops[0])); got != "released/succeeded" {
		t.Errorf("the active tenant's run: got %s, want released/succeeded", got)
	}
	if got := runOf(t, id(runs[1]), id(ops[1])); got != "releasing/queued" {
		t.Errorf("the suspended tenant's run: got %s, want releasing/queued", got)
	}
	var files int
	pgtest.Scan(t, `SELECT count(*) FROM payments.bank_file WHERE payment_run_id = $1`, []any{id(runs[1])}, &files)
	if files != 0 {
		t.Errorf("the suspended tenant's bank files: got %d, want none", files)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, suspended.Tenant.ID)
	if !listed(t, payments.Queued, id(ops[1])) {
		t.Error("resumed: got the release unlisted, want it listed for a workflow")
	}
	for range 2 {
		if _, err := suspended.releaser(t).RunOnce(context.Background()); err != nil {
			t.Fatalf("release round: %v", err)
		}
	}
	if got := runOf(t, id(runs[1]), id(ops[1])); got != "released/succeeded" {
		t.Errorf("the resumed tenant's run: got %s, want released/succeeded", got)
	}
	pgtest.Scan(t, `SELECT count(*) FROM payments.bank_file WHERE payment_run_id = $1`, []any{id(runs[1])}, &files)
	if files != 1 {
		t.Errorf("the resumed tenant's bank files after two rounds: got %d, want 1", files)
	}
}

// TestTransmitterLeavesInactiveTenants: the transmitter does not send a
// suspended tenant's file, nor count an attempt, while an active tenant's
// is sent, and sends it once the tenant is active again.
func TestTransmitterLeavesInactiveTenants(t *testing.T) {
	active, suspended := setup(t), setup(t)
	var files [2]string
	for i, e := range []env{active, suspended} {
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		files[i] = asked(t, e, "INV-1")
	}
	suspended.Tenant.SetStatus(t, "suspended")
	for _, e := range []env{active, suspended} {
		if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
			t.Fatalf("transmit round: %v", err)
		}
	}
	if status, _, _ := fileState(t, files[0]); status != "transmitted" {
		t.Errorf("the active tenant's file: got %s, want transmitted", status)
	}
	if status, attempts, _ := fileState(t, files[1]); status != "to_send" || attempts != 0 {
		t.Errorf("the suspended tenant's file: got %s after %d attempts, want to_send after 0", status, attempts)
	}
	if got := len(suspended.connector.files); got != 0 {
		t.Errorf("the suspended tenant's uploads: got %d, want 0", got)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, suspended.Tenant.ID)
	if _, err := suspended.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("transmit round: %v", err)
	}
	if status, attempts, _ := fileState(t, files[1]); status != "transmitted" || attempts != 1 {
		t.Errorf("the resumed tenant's file: got %s after %d attempts, want transmitted after 1", status, attempts)
	}
}
