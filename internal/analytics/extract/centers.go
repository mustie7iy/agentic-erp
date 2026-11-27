package extract

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The centers' snapshots (T56i2; ADR-0050 §7, ADR-0044 As built T56i2):
// the names of the cost centers, profit centers and segments the journal's
// lines carry by code, and the node of today's standard hierarchy each
// center is placed at, joined to the lines at read time.

type costCenterRow struct {
	TenantID       uuid.UUID `parquet:"tenant_id,uuid"`
	Company        string    `parquet:"company"`
	CostCenter     string    `parquet:"cost_center"`
	CostCenterName string    `parquet:"cost_center_name"`
	CostCenterNode *string   `parquet:"cost_center_node,optional"`
}

type profitCenterRow struct {
	TenantID         uuid.UUID `parquet:"tenant_id,uuid"`
	ProfitCenter     string    `parquet:"profit_center"`
	ProfitCenterName string    `parquet:"profit_center_name"`
	ProfitCenterNode *string   `parquet:"profit_center_node,optional"`
}

type segmentRow struct {
	TenantID    uuid.UUID `parquet:"tenant_id,uuid"`
	Segment     string    `parquet:"segment"`
	SegmentName string    `parquet:"segment_name"`
}

// The centers' queries; row-level security limits each to the tenant. Only
// active centers: a line carries an active center, a pending one carries
// none yet, and a rejected one's code is free for another (ADR-0050 §3), so
// the codes are unique among them as the dataset's key needs. A center's
// node is the one of the kind's standard hierarchy it is placed at, as
// GET /v1/hierarchies/{id} lists the tree: its place and node not removed.
const (
	costCentersSQL = `SELECT cc.tenant_id, le.code, cc.code, cc.name, n.code
		FROM ledger.cost_center cc
		JOIN tenancy.legal_entity le ON le.tenant_id = cc.tenant_id AND le.id = cc.legal_entity_id
		LEFT JOIN ledger.hierarchy h ON h.tenant_id = cc.tenant_id AND h.kind = 'cost_center' AND h.is_standard
		LEFT JOIN ledger.hierarchy_member m ON m.tenant_id = h.tenant_id AND m.hierarchy_id = h.id AND m.cost_center_id = cc.id
			AND m.removed_by_change_id IS NULL
		LEFT JOIN ledger.hierarchy_node n ON n.tenant_id = m.tenant_id AND n.id = m.node_id AND n.removed_by_change_id IS NULL
		WHERE cc.status = 'active'
		ORDER BY le.code, cc.code`
	profitCentersSQL = `SELECT pc.tenant_id, pc.code, pc.name, n.code
		FROM ledger.profit_center pc
		LEFT JOIN ledger.hierarchy h ON h.tenant_id = pc.tenant_id AND h.kind = 'profit_center' AND h.is_standard
		LEFT JOIN ledger.hierarchy_member m ON m.tenant_id = h.tenant_id AND m.hierarchy_id = h.id AND m.profit_center_id = pc.id
			AND m.removed_by_change_id IS NULL
		LEFT JOIN ledger.hierarchy_node n ON n.tenant_id = m.tenant_id AND n.id = m.node_id AND n.removed_by_change_id IS NULL
		WHERE pc.status = 'active'
		ORDER BY pc.code`
	segmentsSQL = `SELECT s.tenant_id, s.code, s.name FROM ledger.segment s ORDER BY s.code`
)

func (r *run) costCenters(ctx context.Context, tx pgx.Tx) error {
	var c costCenterRow
	return snapshot(ctx, tx, r, "cost_centers", costCentersSQL, []any{&c.TenantID, &c.Company, &c.CostCenter, &c.CostCenterName, &c.CostCenterNode},
		func() (costCenterRow, string, error) {
			row := c
			c.CostCenterNode = nil // the scan may reuse a non-nil pointer, which row holds
			return row, "", nil
		})
}

func (r *run) profitCenters(ctx context.Context, tx pgx.Tx) error {
	var p profitCenterRow
	return snapshot(ctx, tx, r, "profit_centers", profitCentersSQL, []any{&p.TenantID, &p.ProfitCenter, &p.ProfitCenterName, &p.ProfitCenterNode},
		func() (profitCenterRow, string, error) {
			row := p
			p.ProfitCenterNode = nil // the scan may reuse a non-nil pointer, which row holds
			return row, "", nil
		})
}

func (r *run) segments(ctx context.Context, tx pgx.Tx) error {
	var s segmentRow
	return snapshot(ctx, tx, r, "segments", segmentsSQL, []any{&s.TenantID, &s.Segment, &s.SegmentName},
		func() (segmentRow, string, error) { return s, "", nil })
}
