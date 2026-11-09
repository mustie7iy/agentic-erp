package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// The workflows context starts and drives the runs' workflows (ADR-0012);
// these are what it asks payments about them, so that it reads no table of
// this context. Queued and Releases are read as the worker role across
// tenants, active ones only (database.ActiveTenants, ADR-0015).

// Queued lists the proposals and releases waiting for a workflow, in the
// order they were queued: a proposal queued (or cancelled before its
// workflow started) whose run is still proposing, and a queued release of
// a releasing run, numbered among the run's release operations since a
// failed release is resumed by a new one.
func Queued(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
	rows, _ := tx.Query(ctx, `SELECT o.kind, o.tenant_id, o.id, r.id, r.legal_entity_id, r.document_number, 0 FROM platform.operation o
			JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.kind = $1 AND o.status IN ('queued', 'cancelled') AND r.status = 'proposing' AND o.workflow_id IS NULL AND o.tenant_id IN `+database.ActiveTenants+`
		UNION ALL
		SELECT o.kind, o.tenant_id, o.id, r.id, r.legal_entity_id, r.document_number,
			(SELECT count(*) FROM platform.operation x WHERE x.tenant_id = o.tenant_id AND x.kind = o.kind AND x.resource = o.resource AND x.id <= o.id)
			FROM platform.operation o
			JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.release_operation_id = o.id
			WHERE o.kind = $2 AND o.status = 'queued' AND r.status = 'releasing' AND o.workflow_id IS NULL AND o.tenant_id IN `+database.ActiveTenants+`
		ORDER BY 3`, ProposeKind, ReleaseKind)
	waiting, err := pgx.CollectRows(rows, operations.ScanWaiting)
	if err != nil {
		return nil, fmt.Errorf("payments: list the proposals and releases waiting for a workflow: %w", err)
	}
	return waiting, nil
}

// Releases lists the queued and running release operations of releasing
// runs, oldest first: the work of the in-process releaser of a cell
// without a workflow engine. Only the kind, tenant, operation and run are
// set.
func Releases(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
	rows, _ := tx.Query(ctx, `SELECT o.kind, o.tenant_id, o.id, r.id, r.legal_entity_id, '', 0 FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.release_operation_id = o.id
		WHERE o.kind = $1 AND o.status IN ('queued', 'running') AND r.status = 'releasing' AND o.tenant_id IN `+database.ActiveTenants+` ORDER BY o.id`, ReleaseKind)
	waiting, err := pgx.CollectRows(rows, operations.ScanWaiting)
	if err != nil {
		return nil, fmt.Errorf("payments: list the release operations to run: %w", err)
	}
	return waiting, nil
}

// ReleasingUnder reports whether the run is releasing under the operation.
// IS NOT DISTINCT FROM rather than =: release_operation_id is nullable
// (runs released before migration 000029 have none), and = would make the
// result NULL, which does not scan into a bool.
func ReleasingUnder(ctx context.Context, tx pgx.Tx, run, operation uuid.UUID) (bool, error) {
	var live bool
	if err := tx.QueryRow(ctx, `SELECT status = 'releasing' AND release_operation_id IS NOT DISTINCT FROM $2 FROM payments.payment_run WHERE id = $1`,
		run, operation).Scan(&live); err != nil {
		return false, fmt.Errorf("payments: read the release of run %s: %w", run, err)
	}
	return live, nil
}
