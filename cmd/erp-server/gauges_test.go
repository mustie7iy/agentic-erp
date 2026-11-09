package main

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// businessNames are the gauges businessGauges registers.
var businessNames = []string{"erp_supplier_invoices_on_hold", "erp_supplier_invoices_parked_stale", "erp_payment_runs_proposed",
	"erp_payment_runs_failed_recent", "erp_bank_files_unacknowledged", "erp_bank_files_unacknowledged_age_seconds",
	"erp_bank_files_rejected_recent", "erp_approvals_pending", "erp_approvals_pending_age_seconds", "erp_einvoices_unacknowledged", "erp_periods_overdue"}

// scrape reads the business gauges. A read that outlasts its timeout on a
// loaded machine is left out of that scrape, so it scrapes until every
// gauge is there; a read the worker role may not make never is.
func scrape(t *testing.T, m *telemetry.Metrics) map[string]float64 {
	t.Helper()
	var got map[string]float64
	pgtest.Eventually(t, 30*time.Second, func() bool {
		got = samples(t, m)
		for _, name := range businessNames {
			if _, ok := got[name]; !ok {
				return false
			}
		}
		return true
	}, "a business gauge is missing")
	return got
}

// samples reads every unlabelled sample the metrics expose.
func samples(t *testing.T, m *telemetry.Metrics) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	got := map[string]float64{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(line, "#") || strings.Contains(name, "{") {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("sample %q: %v", line, err)
		}
		got[name] = v
	}
	return got
}

// gaugeFixtures writes, for one tenant, a row each gauge counts and a row
// beside it that it must not count. The foreign keys and triggers are off
// (replica) so the rows need no parties, banks or ledgers behind them; the
// checks still hold.
func gaugeFixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after the commit; on a failure the test has already failed
	ten, u := uuid.New(), uuid.New()
	for _, sql := range []string{
		`SET LOCAL session_replication_role = replica`,
		// On hold counts; parked counts only after 7 days.
		`INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
			currency, gross_amount, tax_amount, status, hold_reason, source, created_at, created_by, updated_by) VALUES
			($1, gen_random_uuid(), 'I1', $2, $2, 'R1', current_date, 'EUR', 100, 0, 'on_hold', 'price', 'manual', now(), $2, $2),
			($1, gen_random_uuid(), 'I2', $2, $2, 'R2', current_date, 'EUR', 100, 0, 'parked', NULL, 'manual', now() - interval '8 days', $2, $2),
			($1, gen_random_uuid(), 'I3', $2, $2, 'R3', current_date, 'EUR', 100, 0, 'parked', NULL, 'manual', now() - interval '6 days', $2, $2)`,
		// A proposed run counts; a failed one only within a day.
		`INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount,
			status, proposed_by, created_by, updated_by, updated_at) VALUES
			($1, gen_random_uuid(), 'P1', $2, $2, current_date, current_date, 'EUR', 0, 'proposed', $2, $2, $2, now()),
			($1, gen_random_uuid(), 'P2', $2, $2, current_date, current_date, 'EUR', 0, 'failed', $2, $2, $2, now()),
			($1, gen_random_uuid(), 'P3', $2, $2, current_date, current_date, 'EUR', 0, 'failed', $2, $2, $2, now() - interval '2 days')`,
		// A transmitted file counts, with its age; a rejected one only within a day.
		`INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_by, updated_at)
			SELECT $1, gen_random_uuid(), gen_random_uuid(), 'pain.001.001.09', 'f', sha256('f'), s, $2, now() - a
			FROM (VALUES ('transmitted', interval '400 days'), ('rejected', interval '0'), ('rejected', interval '2 days'), ('acknowledged', interval '0')) AS v(s, a)`,
		// A pending request counts, with its age; a decided one does not.
		`INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, status, completed_at, created_at, created_by) VALUES
			($1, gen_random_uuid(), 't', gen_random_uuid(), 't', $2, 'pending', NULL, now() - interval '400 days', $2),
			($1, gen_random_uuid(), 't', gen_random_uuid(), 't', $2, 'approved', now(), now() - interval '500 days', $2)`,
		// Only the e-invoice unacknowledged for over an hour counts: not
		// the one just received, nor the one acknowledged.
		`INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
			received_at, acknowledged_at, created_by, updated_by)
			SELECT $1, gen_random_uuid(), $2, m, 'received', sha256(m::bytea), 1, 'k', $2, now() - r, a, $2, $2
			FROM (VALUES ('M1', interval '2 hours', NULL::timestamptz), ('M2', interval '0', NULL), ('M3', interval '2 hours', now())) AS v(m, r, a)`,
		// Only the open normal period past its end counts: not the closed
		// one, the one still running, or the special period.
		`INSERT INTO ledger.fiscal_period (tenant_id, calendar_id, fiscal_year, period, start_date, end_date, is_special) VALUES
			($1, $2, 2020, 1, '2020-01-01', '2020-01-31', false), ($1, $2, 2020, 2, '2020-02-01', '2020-02-29', false),
			($1, $2, 2020, 13, '2020-02-01', '2020-02-29', true), ($1, $2, 2999, 1, '2999-01-01', '2999-01-31', false)`,
		`INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
			SELECT $1, gen_random_uuid(), $2, $2, $2, y, p, s, $2
			FROM (VALUES (2020, 1, 'open'), (2020, 2, 'closed'), (2020, 13, 'open'), (2999, 1, 'open')) AS v(y, p, s)`,
	} {
		args := []any{ten, u}
		if strings.HasPrefix(sql, "SET") {
			args = nil
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestBusinessGauges: each business gauge counts the rows it watches, as
// the worker role, and leaves the others out (ADR-0043, T54b).
func TestBusinessGauges(t *testing.T) {
	m := telemetry.NewMetrics()
	businessGauges(pgtest.Pool(t), config.Config{WorkerRole: "erp_worker"}, m)
	before := scrape(t, m)
	gaugeFixtures(t)
	after := scrape(t, m)
	for name, want := range map[string]float64{"erp_supplier_invoices_on_hold": 1, "erp_supplier_invoices_parked_stale": 1,
		"erp_payment_runs_proposed": 1, "erp_payment_runs_failed_recent": 1, "erp_bank_files_unacknowledged": 1,
		"erp_bank_files_rejected_recent": 1, "erp_approvals_pending": 1, "erp_einvoices_unacknowledged": 1, "erp_periods_overdue": 1} {
		if got := after[name] - before[name]; got != want {
			t.Errorf("%s: got %v more, want %v", name, got, want)
		}
	}
	for _, name := range []string{"erp_bank_files_unacknowledged_age_seconds", "erp_approvals_pending_age_seconds"} {
		if got := after[name]; got < 400*86400 {
			t.Errorf("%s: got %v, want at least 400 days", name, got)
		}
	}
}

// TestHorizonGauges: the horizon gauges read what readiness checks, as the
// worker role (T54e).
func TestHorizonGauges(t *testing.T) {
	m := telemetry.NewMetrics()
	// A fixed clock: the gauges and the check count from the same month.
	now := func() time.Time { return time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) }
	horizonGauges(pgtest.Pool(t), config.Config{WorkerRole: "erp_worker"}, m, now)
	months, years, err := db.Horizon(context.Background(), pgtest.Admin(t), now())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]float64
	pgtest.Eventually(t, 30*time.Second, func() bool {
		got = samples(t, m)
		_, a := got["erp_partition_horizon_audit_months"]
		_, l := got["erp_partition_horizon_ledger_years"]
		return a && l
	}, "a horizon gauge is missing")
	for name, want := range map[string]int{"erp_partition_horizon_audit_months": months, "erp_partition_horizon_ledger_years": years} {
		if got[name] != float64(want) {
			t.Errorf("%s: got %v, want %d", name, got[name], want)
		}
	}
}
