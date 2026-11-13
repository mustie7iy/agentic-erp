package controlling

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// write replaces the plan of the batch's category, company and year on
// the centers given by its lines: the batch is kept with the number of
// the centers' current lines, which it then marks replaced, and its lines
// are inserted. Nothing is deleted, so every earlier plan stays readable.
// The category is held FOR UPDATE by the caller, so batches of it run one
// after the other and a lock waits for them.
func write(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b Batch, lines []planLine, of planCenters) (Batch, error) {
	const current = `FROM controlling.plan_line WHERE category_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3
		AND (cost_center_id = ANY($4) OR profit_center_id = ANY($5)) AND replaced_by_batch_id IS NULL`
	b, err := scanBatch(tx.QueryRow(ctx, `INSERT INTO controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year, batch_key, digest,
		line_count, replaced_count, created_by, copied_from_category_id, copy_percentage)
		SELECT $6, $7, $1, $2, $3, $8, $9, $10, (SELECT count(*) `+current+`), $11, $12, $13::numeric RETURNING `+batchColumns,
		b.CategoryID, b.LegalEntityID, b.FiscalYear, of.cost, of.profit, p.TenantID, b.ID, b.Key, b.Digest, b.Lines, p.ID, b.CopiedFrom, b.CopyPercentage))
	if err != nil {
		return Batch{}, fmt.Errorf("controlling: insert plan batch %s: %w", b.Key, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE controlling.plan_line SET replaced_by_batch_id = $6 WHERE id IN (SELECT id `+current+`)`,
		b.CategoryID, b.LegalEntityID, b.FiscalYear, of.cost, of.profit, b.ID); err != nil {
		return Batch{}, fmt.Errorf("controlling: replace the plan of batch %s: %w", b.Key, err)
	}
	ids, periods := make([]uuid.UUID, len(lines)), make([]int16, len(lines))
	ccOf, pcOf, accounts := make([]*uuid.UUID, len(lines)), make([]*uuid.UUID, len(lines)), make([]uuid.UUID, len(lines))
	amounts, quantities, units := make([]int64, len(lines)), make([]*string, len(lines)), make([]*string, len(lines))
	for i, l := range lines {
		ids[i], periods[i], ccOf[i], pcOf[i], accounts[i] = uuid.Must(uuid.NewV7()), l.period, l.costCenter, l.profitCenter, l.account
		amounts[i], quantities[i], units[i] = int64(l.amount), l.quantity, l.unit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO controlling.plan_line (tenant_id, id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period,
			cost_center_id, profit_center_id, account_id, amount, quantity, uom_code, created_by)
		SELECT $1, l.id, $2, $3, $4, $5, l.period, l.cc, l.pc, l.account, l.amount, l.quantity::numeric, l.unit, $6
		FROM unnest($7::uuid[], $8::smallint[], $9::uuid[], $10::uuid[], $11::uuid[], $12::bigint[], $13::text[], $14::text[])
			AS l(id, period, cc, pc, account, amount, quantity, unit)`,
		p.TenantID, b.ID, b.CategoryID, b.LegalEntityID, b.FiscalYear, p.ID, ids, periods, ccOf, pcOf, accounts, amounts, quantities, units); err != nil {
		return Batch{}, fmt.Errorf("controlling: insert the lines of plan batch %s: %w", b.Key, err)
	}
	return b, nil
}

// recordBatch audits the batch and publishes it with the centers whose
// plan it replaced; a copy's with the category copied from and the
// percentage.
func recordBatch(ctx context.Context, b Batch, of planCenters) error {
	detail := map[string]any{"plan_category_id": b.CategoryID, "legal_entity_id": b.LegalEntityID, "fiscal_year": b.FiscalYear, "batch_key": b.Key,
		"line_count": b.Lines, "replaced_count": b.Replaced, "cost_center_ids": of.cost, "profit_center_ids": of.profit}
	payload := map[string]any{"plan_batch_id": b.ID, "plan_category_id": b.CategoryID, "legal_entity_id": b.LegalEntityID,
		"fiscal_year": b.FiscalYear, "batch_key": b.Key, "line_count": b.Lines, "replaced_count": b.Replaced,
		"cost_center_ids": of.cost, "profit_center_ids": of.profit}
	if b.CopiedFrom != nil {
		for _, m := range []map[string]any{detail, payload} {
			m["copied_from_plan_category_id"], m["copy_percentage"] = *b.CopiedFrom, *b.CopyPercentage
		}
	}
	return audit.Record(ctx, "controlling.plan_batch.apply", "controlling.plan_batch", b.ID, detail,
		outbox.Event{Context: "controlling", Name: "plan_batch_applied", AggregateType: "controlling.plan_batch", AggregateID: b.ID, Payload: payload})
}
