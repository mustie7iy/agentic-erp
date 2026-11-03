package db

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The asset master (T57b, migration 000191): what the database keeps of
// assets, their terms per ledger and their center assignments (ADR-0057
// §5, §7, §8).

// assetFixture is a tenant with a planned asset (last_seq 1, its first
// assignment), its straight-line terms in one ledger, and an active asset.
type assetFixture struct {
	fixture
	planned, active, ledger2, cc, pc uuid.UUID
}

const (
	assetSQL = `INSERT INTO assets.asset (tenant_id, id, legal_entity_id, asset_number, class_code, description, status, last_seq, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'A machine', $6, 1, $7, $7)`
	termsSQL = `INSERT INTO assets.asset_terms (tenant_id, asset_id, ledger_id, method, useful_life_months, declining_factor, residual_value, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $8)`
	assignmentSQL = `INSERT INTO assets.asset_assignment (tenant_id, asset_id, seq, valid_from, cost_center_id, profit_center_id, created_by)
		VALUES ($1, $2, $3, $4::date, $5, $6, $7)`
)

func newAssetFixture(t *testing.T) assetFixture {
	t.Helper()
	f := assetFixture{fixture: newFixture(t), planned: id(), active: id(), ledger2: id(), cc: id(), pc: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{assetSQL, []any{f.tenant, f.planned, f.entity, "1", "MACHINES", "planned", f.actor}},
			{assignmentSQL, []any{f.tenant, f.planned, 1, "-infinity", f.cc, f.pc, f.actor}},
			{termsSQL, []any{f.tenant, f.planned, f.ledger, "straight_line", 120, nil, 0, f.actor}},
			{assetSQL, []any{f.tenant, f.active, f.entity, "2", "MACHINES", "planned", f.actor}},
			{`UPDATE assets.asset SET status = 'active', capitalised_on = '2026-06-01', version = version + 1 WHERE id = $1`, []any{f.active}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f assetFixture) try(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
}

// TestAssetRules: every rule the database holds of the asset master.
func TestAssetRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	status := `UPDATE assets.asset SET status = $2, capitalised_on = $3::date, version = version + 1 WHERE id = $1`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"an asset number twice":        {assetSQL, []any{f.tenant, id(), f.entity, "1", "MACHINES", "planned", f.actor}, "asset_number_once"},
		"a class code SAP cannot hold": {assetSQL, []any{f.tenant, id(), f.entity, "3", "machines", "planned", f.actor}, "asset_class_code_check"},
		"an asset starting active":     {assetSQL, []any{f.tenant, id(), f.entity, "3", "MACHINES", "active", f.actor}, "an asset starts planned"},
		"an asset starting cancelled":  {assetSQL, []any{f.tenant, id(), f.entity, "3", "MACHINES", "cancelled", f.actor}, "an asset starts planned"},
		"active without a date":        {status, []any{f.planned, "active", nil}, "asset_capitalised"},
		"planned with a date":          {status, []any{f.active, "planned", "2026-06-01"}, "asset_capitalised"},
		"planned to retired":           {status, []any{f.planned, "retired", "2026-06-01"}, "cannot change from planned to retired"},
		"active to cancelled":          {status, []any{f.active, "cancelled", nil}, "cannot change from active to cancelled"},
		"another company":              {`UPDATE assets.asset SET legal_entity_id = $2, version = version + 1 WHERE id = $1`, []any{f.planned, id()}, "keeps its company"},
		"another class":                {`UPDATE assets.asset SET class_code = 'VEHICLES', version = version + 1 WHERE id = $1`, []any{f.planned}, "keeps its company"},
		"an asset with a last_seq of 0": {`INSERT INTO assets.asset (tenant_id, id, legal_entity_id, asset_number, class_code, description, status, last_seq, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, '9', 'MACHINES', 'd', 'planned', 0, $3, $3)`, []any{f.tenant, f.entity, f.actor}, "asset_last_seq_check"},
		"an asset deleted":                {`DELETE FROM assets.asset WHERE id = $1`, []any{f.planned}, "permission denied"},
		"terms twice in a ledger":         {termsSQL, []any{f.tenant, f.planned, f.ledger, "straight_line", 60, nil, 0, f.actor}, "asset_terms_pkey"},
		"terms of an unknown asset":       {termsSQL, []any{f.tenant, id(), f.ledger, "straight_line", 60, nil, 0, f.actor}, "asset_terms_tenant_id_asset_id_fkey"},
		"an unknown method":               {termsSQL, []any{f.tenant, f.planned, f.ledger2, "sum_of_digits", 60, nil, 0, f.actor}, "asset_terms_method_check"},
		"a useful life of 0":              {termsSQL, []any{f.tenant, f.planned, f.ledger2, "straight_line", 0, nil, 0, f.actor}, "asset_terms_useful_life_months_check"},
		"a useful life above 1200 months": {termsSQL, []any{f.tenant, f.planned, f.ledger2, "straight_line", 1201, nil, 0, f.actor}, "asset_terms_useful_life_months_check"},
		"declining balance without a factor": {termsSQL, []any{f.tenant, f.planned, f.ledger2, "declining_balance", 60, nil, 0, f.actor},
			"asset_terms_declining_factor"},
		"straight-line with a factor": {termsSQL, []any{f.tenant, f.planned, f.ledger2, "straight_line", 60, "2", 0, f.actor}, "asset_terms_declining_factor"},
		"a factor of 1":               {termsSQL, []any{f.tenant, f.planned, f.ledger2, "declining_balance", 60, "1", 0, f.actor}, "asset_terms_declining_factor_check"},
		"a factor above 10":           {termsSQL, []any{f.tenant, f.planned, f.ledger2, "declining_balance", 60, "10.5", 0, f.actor}, "asset_terms_declining_factor_check"},
		"a negative residual value":   {termsSQL, []any{f.tenant, f.planned, f.ledger2, "straight_line", 60, nil, -1, f.actor}, "asset_terms_residual_value_check"},
		"booked through a year alone": {`UPDATE assets.asset_terms SET booked_through_year = 2026, version = version + 1 WHERE asset_id = $1`, []any{f.planned},
			"asset_terms_booked_through"},
		"an assignment out of seq":       {assignmentSQL, []any{f.tenant, f.planned, 2, "2026-07-01", nil, f.pc, f.actor}, "an assignment takes the asset's last seq"},
		"an assignment of a seq taken":   {assignmentSQL, []any{f.tenant, f.planned, 1, "2026-07-01", nil, f.pc, f.actor}, "asset_assignment_pkey"},
		"a first assignment from a day":  {assignmentSQL, []any{f.tenant, f.active, 1, "2026-07-01", nil, f.pc, f.actor}, "only its first assignment is valid from -infinity"},
		"an assignment changed":          {`UPDATE assets.asset_assignment SET profit_center_id = NULL WHERE asset_id = $1`, []any{f.planned}, "permission denied"},
		"an assignment deleted":          {`DELETE FROM assets.asset_assignment WHERE asset_id = $1`, []any{f.planned}, "permission denied"},
		"an assignment of another asset": {assignmentSQL, []any{f.tenant, id(), 1, "-infinity", nil, f.pc, f.actor}, "an assignment takes the asset's last seq"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
	t.Run("the moves an asset may make", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			for _, m := range []struct {
				status string
				on     any
			}{{"retired", "2026-06-01"}, {"active", "2026-06-01"}, {"planned", nil}, {"active", "2026-07-01"}} {
				if err := exec(tx, status, f.active, m.status, m.on); err != nil {
					return fmt.Errorf("to %s: %w", m.status, err)
				}
			}
			return exec(tx, status, f.planned, "cancelled", nil)
		})
		failsWith(t, f.try(t, status, f.planned, "planned", nil), "cannot change from cancelled to planned")
		failsWith(t, f.try(t, `UPDATE assets.asset SET description = 'x', version = version + 1 WHERE id = $1`, f.planned), "cannot change from cancelled")
	})
}

// TestAssetAssignmentsInSeqOrder: each assignment takes the asset's
// last_seq, raised first under the asset's lock, so the assignment with
// the highest seq is the latest and names the asset's centers.
func TestAssetAssignmentsInSeqOrder(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	later := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `UPDATE assets.asset SET last_seq = 2, version = version + 1 WHERE id = $1`, f.planned); err != nil {
			return err
		}
		if err := exec(tx, assignmentSQL, f.tenant, f.planned, 2, "2026-07-01", nil, later, f.actor); err != nil {
			return err
		}
		return exec(tx, `UPDATE assets.asset SET last_seq = 3, version = version + 1 WHERE id = $1`, f.planned)
	})
	failsWith(t, f.try(t, assignmentSQL, f.tenant, f.planned, 2, "2026-08-01", nil, f.pc, f.actor), "takes the asset's last seq 3, not 2")
	failsWith(t, f.try(t, assignmentSQL, f.tenant, f.planned, 4, "2026-08-01", nil, f.pc, f.actor), "an assignment takes the asset's last seq")
	failsWith(t, f.try(t, assignmentSQL, f.tenant, f.planned, 3, "-infinity", nil, f.pc, f.actor), "only its first assignment is valid from -infinity")
	failsWith(t, f.try(t, `UPDATE assets.asset SET last_seq = 2, version = version + 1 WHERE id = $1`, f.planned), "last_seq goes from 3 back to 2")
	var current uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT profit_center_id FROM assets.asset_assignment WHERE asset_id = $1 ORDER BY seq DESC LIMIT 1`, f.planned).
			Scan(&current)
	})
	if current != later {
		t.Errorf("the asset's profit center: got %v, want the latest assignment's %v", current, later)
	}
}

// TestAssetTermsInUse: terms are fixed once depreciation is posted in their
// ledger; booked_through itself still moves.
func TestAssetTermsInUse(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `UPDATE assets.asset_terms SET useful_life_months = 96, version = version + 1 WHERE asset_id = $1`, f.planned); err != nil {
			return err
		}
		return exec(tx, `UPDATE assets.asset_terms SET booked_through_year = 2026, booked_through_period = 6, version = version + 1 WHERE asset_id = $1`, f.planned)
	})
	for name, set := range map[string]string{
		"method":             "method = 'declining_balance', declining_factor = 2",
		"useful life":        "useful_life_months = 60",
		"residual value":     "residual_value = 100",
		"depreciation start": "depreciation_start = '2026-07-01'",
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, `UPDATE assets.asset_terms SET `+set+`, version = version + 1 WHERE asset_id = $1`, f.planned), "are fixed, depreciation having been posted")
		})
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE assets.asset_terms SET booked_through_period = 7, version = version + 1 WHERE asset_id = $1`, f.planned)
	})
}
