package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// reader returns a gauge read: one value from a read-only query run as the
// role, on each scrape.
//
//nolint:forbidigo // a count or an age read as a Prometheus gauge value, which is float64; no amount or quantity
func reader(pool *database.Pool) func(role, sql string, args ...any) func(context.Context) (float64, error) {
	return func(role, sql string, args ...any) func(context.Context) (float64, error) {
		return func(ctx context.Context) (float64, error) {
			var n float64
			err := pool.InTx(ctx, database.Scope{Role: role, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql, args...).Scan(&n)
			})
			return n, err
		}
	}
}

// businessGauges expose the work waiting in each context, cell-wide and
// read on each scrape (ADR-0043, T54b): no tenant, company or document is a
// label, so the series stay fixed however many customers the cell holds;
// which tenant's work waits is found in the database, under RLS. Each query
// reads a partial index of the rows it watches (000092–000095), as the
// worker role, through the columns 000091 grants it, which is the accepted
// exception to reading through the owning context that backlogs describes.
// Terminal states (a failed run, a rejected file) count over the last day,
// so the gauge falls again once the failures stop.
func businessGauges(pool *database.Pool, cfg config.Config, m *telemetry.Metrics) {
	count := reader(pool)
	m.Gauge("erp_supplier_invoices_on_hold", "Supplier invoices on hold.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payables.supplier_invoice WHERE status = 'on_hold'`))
	// Measured from entry, not from parking: an invoice released from hold
	// is parked again, and the work has still waited since it arrived.
	m.Gauge("erp_supplier_invoices_parked_stale", "Supplier invoices entered more than 7 days ago and still parked.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payables.supplier_invoice WHERE status = 'parked' AND created_at < now() - interval '7 days'`))
	m.Gauge("erp_payment_runs_proposed", "Payment runs proposed and not yet released or rejected.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payments.payment_run WHERE status = 'proposed'`))
	m.Gauge("erp_payment_runs_failed_recent", "Payment runs that failed in the last 24 hours.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payments.payment_run WHERE status = 'failed' AND updated_at > now() - interval '24 hours'`))
	// A transmitted file waits for the bank's acknowledgement; its age runs
	// from the transmission, its last change. T44's worker adds statuses
	// these gauges do not see yet (tracked).
	m.Gauge("erp_bank_files_unacknowledged", "Bank files transmitted and not yet acknowledged or rejected.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payments.bank_file WHERE status = 'transmitted'`))
	m.Gauge("erp_bank_files_unacknowledged_age_seconds", "Age of the oldest bank file transmitted and not yet acknowledged or rejected.",
		count(cfg.WorkerRole, `SELECT coalesce(extract(epoch FROM now() - min(updated_at)), 0)::float8 FROM payments.bank_file WHERE status = 'transmitted'`))
	m.Gauge("erp_bank_files_rejected_recent", "Bank files the bank rejected in the last 24 hours.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payments.bank_file WHERE status = 'rejected' AND updated_at > now() - interval '24 hours'`))
	// Pending requests are few, and approval_request_one_pending holds
	// exactly them: no index of its own is needed.
	m.Gauge("erp_approvals_pending", "Approval requests waiting for a decision.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM workflow.approval_request WHERE status = 'pending'`))
	m.Gauge("erp_approvals_pending_age_seconds", "Age of the oldest approval request waiting for a decision.",
		count(cfg.WorkerRole, `SELECT coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8 FROM workflow.approval_request WHERE status = 'pending'`))
	// Special periods share the year's last end date and stay open for
	// the auditors' adjustments on purpose, so only normal periods count.
	m.Gauge("erp_periods_overdue", "Posting periods past their end date and still open.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM ledger.period_status s
			JOIN ledger.fiscal_period f USING (tenant_id, calendar_id, fiscal_year, period)
			WHERE s.status = 'open' AND f.end_date < current_date AND NOT f.is_special`))
}

// horizonGauges export the partition horizon readiness requires (db.Horizon),
// so an alert asks for the maintenance job before readiness fails (T54e).
// The catalog it reads is open to the worker role; now is the clock the
// horizon is counted from.
//
//nolint:forbidigo // a count of partitions read as a Prometheus gauge value, which is float64; no amount or quantity
func horizonGauges(pool *database.Pool, cfg config.Config, m *telemetry.Metrics, now func() time.Time) {
	read := func(pick func(months, years int) int) func(context.Context) (float64, error) {
		return func(ctx context.Context) (float64, error) {
			var n int
			err := pool.InTx(ctx, database.Scope{Role: cfg.WorkerRole, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
				months, years, err := db.Horizon(ctx, tx, now())
				n = pick(months, years)
				return err
			})
			return float64(n), err
		}
	}
	m.Gauge("erp_partition_horizon_audit_months", "Audit partitions ahead of the current month.",
		read(func(months, _ int) int { return months }))
	m.Gauge("erp_partition_horizon_ledger_years", "Journal partitions ahead of the current year.",
		read(func(_, years int) int { return years }))
}
