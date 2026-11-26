package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The asset history sheet's class totals per fiscal period (migration
// 000473; ADR-0057 As built, the consolidated sheet): per company, ledger,
// fiscal year, period and class, the sums of the movements by the sheet's
// columns.

// periodMovementSQL writes a movement of an asset (seq 2) in a ledger,
// fiscal year and period (a special period on period 12's dates): kind,
// cost, accumulated depreciation and its document's kind.
const periodMovementSQL = `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
		fiscal_year, fiscal_period, cost, accumulated, document_kind, document_id, created_by)
	SELECT $1, gen_random_uuid(), $2, 2, a.legal_entity_id, $3, a.class_code, $4, make_date($5, least($6, 12), 1), make_date($5, least($6, 12), 1), $5, $6,
		$7, $8, $9, gen_random_uuid(), $10 FROM assets.asset a WHERE a.id = $2`

// TestAssetClassPeriodTotalsBackfilled: 000473 totals the movements
// written before it, each kind in its column, per company, ledger, fiscal
// year, period and class.
func TestAssetClassPeriodTotalsBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000473_asset_class_period_totals")
	f := fixtureIDs()
	mill, plot, ledger2 := id(), id(), id()
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1,$2,'2L','Local',false,'HGB',$3)",
			f.tenant, ledger2, f.actor); err != nil {
			return err
		}
		for _, a := range []struct {
			id           uuid.UUID
			number, kind string
		}{{mill, "1", "MACHINES"}, {plot, "2", "LAND"}} {
			if err := exec(tx, assetSQL, f.tenant, a.id, f.entity, a.number, a.kind, "planned", f.actor); err != nil {
				return err
			}
		}
		for _, m := range []struct {
			asset, ledger  uuid.UUID
			kind, document string
			year, period   int
			cost, acc      int64
		}{
			{mill, f.ledger, "acquisition", "acquisition", 2025, 3, 1200, 0}, {mill, f.ledger, "depreciation", "acquisition", 2026, 1, 0, -10},
			{mill, f.ledger, "depreciation", "acquisition", 2026, 2, 0, -10}, {mill, f.ledger, "depreciation", "acquisition", 2026, 2, 0, -5},
			{mill, f.ledger, "retirement", "acquisition", 2026, 2, -300, 45}, {mill, f.ledger, "impairment", "impairment", 2026, 12, 0, -20},
			{mill, f.ledger, "transfer_out", "acquisition", 2026, 13, -300, 45}, {plot, f.ledger, "transfer_in", "acquisition", 2026, 13, 300, -45},
			{mill, ledger2, "acquisition", "acquisition", 2026, 2, 1200, 0},
		} {
			if err := exec(tx, periodMovementSQL, f.tenant, m.asset, m.ledger, m.kind, m.year, m.period, m.cost, m.acc, m.document, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	names := map[uuid.UUID]string{f.ledger: "0L", ledger2: "2L"}
	var got []string
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(context.Background(), `SELECT class_code, ledger_id, fiscal_year, fiscal_period, acquisitions, retired_cost, transferred_cost,
				depreciation, retired_accumulated, transferred_accumulated, impairment_losses, impairment_reversals
			FROM assets.class_period_total ORDER BY 1, 3, 4, ledger_id = $1 DESC`, f.ledger)
		var class string
		var ledger uuid.UUID
		var year, period int16
		var sums [8]int64
		_, err := pgx.ForEachRow(rows, []any{&class, &ledger, &year, &period, &sums[0], &sums[1], &sums[2], &sums[3], &sums[4], &sums[5], &sums[6],
			&sums[7]}, func() error {
			got = append(got, fmt.Sprintf("%s %s %d/%d: %v", class, names[ledger], year, period, sums))
			return nil
		})
		return err
	})
	want := []string{"LAND 0L 2026/13: [0 0 300 0 0 -45 0 0]", "MACHINES 0L 2025/3: [1200 0 0 0 0 0 0 0]", "MACHINES 0L 2026/1: [0 0 0 -10 0 0 0 0]",
		"MACHINES 0L 2026/2: [0 -300 0 -15 45 0 0 0]", "MACHINES 2L 2026/2: [1200 0 0 0 0 0 0 0]", "MACHINES 0L 2026/12: [0 0 0 0 0 0 -20 0]",
		"MACHINES 0L 2026/13: [0 0 -300 0 0 45 0 0]"}
	if g, w := strings.Join(got, "; "), strings.Join(want, "; "); g != w {
		t.Errorf("class period totals:\ngot  %s\nwant %s", g, w)
	}
}

// TestAssetClassPeriodTotalRules: the application adds to the sums of a
// tenant's class period totals, and never moves a row to another key,
// deletes one, writes another tenant's or a period outside 1-16.
func TestAssetClassPeriodTotalRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	insert := `INSERT INTO assets.class_period_total AS t (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, class_code, depreciation)
		VALUES ($1, $2, $3, 2026, $4, 'MACHINES', -10) ON CONFLICT (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, class_code)
		DO UPDATE SET depreciation = t.depreciation + EXCLUDED.depreciation`
	for range 2 {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, insert, f.tenant, f.entity, f.ledger, 6) })
	}
	var depreciation int64
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT depreciation FROM assets.class_period_total`).Scan(&depreciation)
	})
	if depreciation != -20 {
		t.Errorf("the sum added to: got %d, want -20", depreciation)
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a row moved to another class":  {`UPDATE assets.class_period_total SET class_code = 'LAND'`, nil, "permission denied"},
		"a row moved to another period": {`UPDATE assets.class_period_total SET fiscal_period = 7`, nil, "permission denied"},
		"a row deleted":                 {`DELETE FROM assets.class_period_total`, nil, "permission denied"},
		"another tenant's row":          {insert, []any{id(), f.entity, f.ledger, 6}, "row-level security"},
		"period 0":                      {insert, []any{f.tenant, f.entity, f.ledger, 0}, "class_period_total_fiscal_period_check"},
		"period 17":                     {insert, []any{f.tenant, f.entity, f.ledger, 17}, "class_period_total_fiscal_period_check"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
}
