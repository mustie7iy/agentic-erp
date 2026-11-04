package db

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The asset history sheet's year totals (T57i, migration 000207): per
// asset, ledger and fiscal year, and per company, ledger, fiscal year and
// class, the sums of the movements by the sheet's columns (ADR-0057 §5,
// §9).

// yearMovementSQL writes a movement of an asset (seq 2) in a ledger and
// fiscal year: kind, cost and accumulated depreciation.
const yearMovementSQL = `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
		fiscal_year, fiscal_period, cost, accumulated, document_kind, document_id, created_by)
	SELECT $1, gen_random_uuid(), $2, 2, a.legal_entity_id, $3, a.class_code, $4, make_date($5, 6, 1), make_date($5, 6, 1), $5, 6, $6, $7, 'acquisition',
		gen_random_uuid(), $8 FROM assets.asset a WHERE a.id = $2`

// totalsOf reads the year totals, one line each: an asset's ("asset
// ledger year: sums") then the classes' ("class ledger year: sums").
func totalsOf(t *testing.T, conn *pgx.Conn, tenant uuid.UUID, names map[uuid.UUID]string) string {
	t.Helper()
	var out []string
	inTenant(t, conn, tenant, func(tx pgx.Tx) error {
		for _, q := range []string{`SELECT asset_id::text, ledger_id, fiscal_year, acquisitions, retired_cost, transferred_cost, depreciation,
				retired_accumulated, transferred_accumulated FROM assets.year_total ORDER BY 1, 2, 3`,
			`SELECT class_code, ledger_id, fiscal_year, acquisitions, retired_cost, transferred_cost, depreciation, retired_accumulated,
				transferred_accumulated FROM assets.class_year_total ORDER BY 1, 2, 3`} {
			rows, _ := tx.Query(context.Background(), q)
			var key string
			var ledger uuid.UUID
			var year int16
			var sums [6]int64
			if _, err := pgx.ForEachRow(rows, []any{&key, &ledger, &year, &sums[0], &sums[1], &sums[2], &sums[3], &sums[4], &sums[5]}, func() error {
				if u, err := uuid.Parse(key); err == nil { // an asset's, not a class's
					key = names[u]
				}
				out = append(out, fmt.Sprintf("%s %s %d: %v", key, names[ledger], year, sums))
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return strings.Join(out, "; ")
}

// TestAssetYearTotalsBackfilled: 000207 totals the movements written
// before it, each kind in its column, per asset, ledger and year and per
// class; and the application may add to the sums only.
func TestAssetYearTotalsBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 207)
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
			asset, ledger uuid.UUID
			kind          string
			year          int
			cost, acc     int64
		}{
			{mill, f.ledger, "acquisition", 2025, 1200, 0}, {mill, f.ledger, "depreciation", 2025, 0, -60}, {mill, f.ledger, "depreciation", 2026, 0, -120},
			{mill, f.ledger, "retirement", 2026, -300, 45}, {mill, f.ledger, "transfer_out", 2026, -300, 45}, {plot, f.ledger, "transfer_in", 2026, 300, -45},
			{mill, ledger2, "acquisition", 2026, 1200, 0}, {mill, ledger2, "acquisition", 2026, -200, 0},
		} {
			if err := exec(tx, yearMovementSQL, f.tenant, m.asset, m.ledger, m.kind, m.year, m.cost, m.acc, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	names := map[uuid.UUID]string{mill: "mill", plot: "plot", f.ledger: "0L", ledger2: "2L"}
	got := totalsOf(t, conn, f.tenant, names)
	want := strings.Join([]string{"LAND 0L 2026: [0 0 300 0 0 -45]", "MACHINES 0L 2025: [1200 0 0 -60 0 0]", "MACHINES 0L 2026: [0 -300 -300 -120 45 45]",
		"MACHINES 2L 2026: [1000 0 0 0 0 0]"}, "; ")
	for _, line := range []string{"mill 0L 2025: [1200 0 0 -60 0 0]", "mill 0L 2026: [0 -300 -300 -120 45 45]", "mill 2L 2026: [1000 0 0 0 0 0]",
		"plot 0L 2026: [0 0 300 0 0 -45]"} {
		if !strings.Contains(got, line) {
			t.Errorf("asset year totals: got %s, want %s among them", got, line)
		}
	}
	if !strings.HasSuffix(got, want) {
		t.Errorf("class year totals: got %s, want %s", got, want)
	}
}

// TestAssetYearTotalRules: the application adds to the sums of a tenant's
// year totals, and never moves a row to another key, deletes one or
// writes another tenant's; an asset's totals name an asset.
func TestAssetYearTotalRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	insert := `INSERT INTO assets.year_total AS t (tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code, acquisitions)
		VALUES ($1, $2, $3, 2026, $4, 'MACHINES', 100) ON CONFLICT (tenant_id, ledger_id, asset_id, fiscal_year)
		DO UPDATE SET acquisitions = t.acquisitions + EXCLUDED.acquisitions`
	classInsert := `INSERT INTO assets.class_year_total AS t (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, depreciation)
		VALUES ($1, $2, $3, 2026, 'MACHINES', -10) ON CONFLICT (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code)
		DO UPDATE SET depreciation = t.depreciation + EXCLUDED.depreciation`
	for range 2 {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, insert, f.tenant, f.ledger, f.active, f.entity); err != nil {
				return err
			}
			return exec(tx, classInsert, f.tenant, f.entity, f.ledger)
		})
	}
	var acquisitions, depreciation int64
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT y.acquisitions, c.depreciation FROM assets.year_total y, assets.class_year_total c`).
			Scan(&acquisitions, &depreciation)
	})
	if acquisitions != 200 || depreciation != -20 {
		t.Errorf("the sums added to: got %d %d, want 200 -20", acquisitions, depreciation)
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"an asset's row moved to another class": {`UPDATE assets.year_total SET class_code = 'LAND'`, nil, "permission denied"},
		"an asset's row moved to another year":  {`UPDATE assets.year_total SET fiscal_year = 2027`, nil, "permission denied"},
		"a class row moved to another class":    {`UPDATE assets.class_year_total SET class_code = 'LAND'`, nil, "permission denied"},
		"an asset's row deleted":                {`DELETE FROM assets.year_total`, nil, "permission denied"},
		"a class row deleted":                   {`DELETE FROM assets.class_year_total`, nil, "permission denied"},
		"another tenant's asset row":            {insert, []any{id(), f.ledger, f.active, f.entity}, "row-level security"},
		"another tenant's class row":            {classInsert, []any{id(), f.entity, f.ledger}, "row-level security"},
		"the totals of an unknown asset":        {insert, []any{f.tenant, f.ledger, id(), f.entity}, "year_total_tenant_id_asset_id_fkey"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
}

// TestAssetTermsBookedBackfilled: 000211 keeps on each asset's terms the
// depreciation booked in their ledger (T57f3), minus the sum of its
// depreciation movements there, a reversal's included; the cost and
// accumulated depreciation other movements move count nothing, and terms
// without depreciation stay at zero.
func TestAssetTermsBookedBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 211)
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
			id     uuid.UUID
			number string
		}{{mill, "1"}, {plot, "2"}} {
			if err := exec(tx, assetSQL, f.tenant, a.id, f.entity, a.number, "MACHINES", "planned", f.actor); err != nil {
				return err
			}
		}
		for _, terms := range [][2]uuid.UUID{{mill, f.ledger}, {mill, ledger2}, {plot, f.ledger}} {
			if err := exec(tx, termsSQL, f.tenant, terms[0], terms[1], "straight_line", 120, nil, 0, f.actor); err != nil {
				return err
			}
		}
		for _, m := range []struct {
			asset, ledger uuid.UUID
			kind          string
			year          int
			cost, acc     int64
		}{
			{mill, f.ledger, "acquisition", 2025, 1200, 0}, {mill, f.ledger, "depreciation", 2025, 0, -60}, {mill, f.ledger, "depreciation", 2026, 0, -120},
			{mill, f.ledger, "retirement", 2026, -300, 45}, {mill, f.ledger, "transfer_out", 2026, -300, 45}, {plot, f.ledger, "transfer_in", 2026, 300, -45},
			{mill, ledger2, "acquisition", 2026, 1200, 0}, {mill, ledger2, "depreciation", 2026, 0, -30}, {mill, ledger2, "depreciation", 2026, 0, 10},
		} {
			if err := exec(tx, yearMovementSQL, f.tenant, m.asset, m.ledger, m.kind, m.year, m.cost, m.acc, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	names := map[uuid.UUID]string{mill: "mill", plot: "plot", f.ledger: "0L", ledger2: "2L"}
	var got []string
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(context.Background(), `SELECT asset_id, ledger_id, booked_depreciation FROM assets.asset_terms`)
		var asset, ledger uuid.UUID
		var booked int64
		_, err := pgx.ForEachRow(rows, []any{&asset, &ledger, &booked}, func() error {
			got = append(got, fmt.Sprintf("%s %s: %d", names[asset], names[ledger], booked))
			return nil
		})
		return err
	})
	slices.Sort(got)
	if want := "mill 0L: 180; mill 2L: 20; plot 0L: 0"; strings.Join(got, "; ") != want {
		t.Errorf("the depreciation booked on the terms: got %s, want %s", strings.Join(got, "; "), want)
	}
}
