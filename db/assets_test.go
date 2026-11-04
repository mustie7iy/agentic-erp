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

// Asset acquisitions (T57c, migration 000192): what the database keeps of
// acquisitions, the use of capitalised lines, movements and the entries of
// assets documents (ADR-0057 §5, §6).

const (
	movementSQL = `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, cost, accumulated, cost_gc, accumulated_gc, residual, proceeds, document_kind, document_id, reverses_id, created_by)
		VALUES ($1, $2, $3, 2, $4, $5, 'MACHINES', $6, '2026-06-01', '2026-06-01', 2026, 6, $7, $8, $9, $10, $11, $12, 'acquisition', gen_random_uuid(), $13, $14)`
	acquisitionSQL = `INSERT INTO assets.acquisition (tenant_id, id, legal_entity_id, asset_id, seq, value_date, posting_date, fiscal_year, currency, amount,
			amount_lc, amount_gc, currency_lc, currency_gc, source_entry_year, source_entry_id, source_line_no, source_document_number, source_context,
			offset_account_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, '2026-06-01', '2026-06-01', 2026, 'EUR', $6, $6, $6, 'EUR', 'EUR', 2026, $7, 1, '1', 'ledger', $8, $9, $10, $10)`
	useSQL = `INSERT INTO assets.clearing_use (tenant_id, entry_year, entry_id, line_no, legal_entity_id, currency, line_amount, line_lc, line_gc,
			used, used_lc, used_gc) VALUES ($1, 2026, gen_random_uuid(), 1, $2, 'EUR', $3, $4, $5, $6, $7, $8)`
)

// movement inserts a movement of the active asset in the fixture's ledger:
// kind, amounts (cost, accumulated, cost_gc, accumulated_gc, residual,
// proceeds) and the movement it reverses (nil for none).
func (f assetFixture) movement(tx pgx.Tx, id uuid.UUID, kind string, amounts [6]int64, reverses any) error {
	return exec(tx, movementSQL, f.tenant, id, f.active, f.entity, f.ledger, kind, amounts[0], amounts[1], amounts[2], amounts[3], amounts[4], amounts[5],
		reverses, f.actor)
}

// TestAssetMovementRules: movements are append-only; a reversal row has
// its original's kind, asset and ledger and exactly its negated amounts,
// an original is reversed at most once and a reversal never; depreciation
// changes accumulated depreciation alone; an acquisition may be negative
// (a credit line, ADR-0057 §6).
func TestAssetMovementRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	original, reversal, deprec := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, m := range []struct {
			id       uuid.UUID
			kind     string
			amounts  [6]int64
			reverses any
		}{
			{original, "acquisition", [6]int64{1000, 0, 900, 0, 0, 0}, nil},
			{id(), "acquisition", [6]int64{-300, 0, -270, 0, 0, 0}, nil},
			{deprec, "depreciation", [6]int64{0, -50, 0, -45, 0, 0}, nil},
			{reversal, "acquisition", [6]int64{-1000, 0, -900, 0, 0, 0}, original},
		} {
			if err := f.movement(tx, m.id, m.kind, m.amounts, m.reverses); err != nil {
				return fmt.Errorf("%s: %w", m.kind, err)
			}
		}
		return nil
	})
	try := func(kind string, amounts [6]int64, reverses any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error { return f.movement(tx, id(), kind, amounts, reverses) })
	}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"an unknown kind":                  {try("revaluation", [6]int64{1, 0, 1, 0, 0, 0}, nil), "movement_kind_check"},
		"depreciation of cost":             {try("depreciation", [6]int64{1, -1, 0, 0, 0, 0}, nil), "movement_depreciation"},
		"depreciation of group cost":       {try("depreciation", [6]int64{0, -1, 1, 0, 0, 0}, nil), "movement_depreciation"},
		"depreciation of a residual value": {try("depreciation", [6]int64{0, -1, 0, 0, 1, 0}, nil), "movement_depreciation"},
		"depreciation with proceeds":       {try("depreciation", [6]int64{0, -1, 0, 0, 0, 1}, nil), "movement_depreciation"},
		"a reversal of another kind":       {try("retirement", [6]int64{0, 50, 0, 45, 0, 0}, deprec), "does not negate movement"},
		"a reversal not negating":          {try("depreciation", [6]int64{0, 49, 0, 45, 0, 0}, deprec), "does not negate movement"},
		"a reversal not negating in group": {try("depreciation", [6]int64{0, 50, 0, 44, 0, 0}, deprec), "does not negate movement"},
		"a reversal of a reversal":         {try("acquisition", [6]int64{1000, 0, 900, 0, 0, 0}, reversal), "does not negate movement"},
		"an original reversed twice":       {try("acquisition", [6]int64{-1000, 0, -900, 0, 0, 0}, original), "movement_reversed_once"},
		"a reversal of nothing":            {try("acquisition", [6]int64{-1, 0, -1, 0, 0, 0}, id()), "does not negate movement"},
		"a movement changed":               {f.try(t, `UPDATE assets.movement SET cost = 1 WHERE id = $1`, original), "permission denied"},
		"a movement deleted":               {f.try(t, `DELETE FROM assets.movement WHERE id = $1`, original), "permission denied"},
		"a movement of an unknown asset": {f.try(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind,
			value_date, posting_date, fiscal_year, fiscal_period, document_kind, document_id, created_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 1, $2, $3,
			'MACHINES', 'acquisition', '2026-06-01', '2026-06-01', 2026, 6, 'acquisition', gen_random_uuid(), $4)`, f.tenant, f.entity, f.ledger, f.actor),
			"movement_tenant_id_asset_id_fkey"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, c.err, c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return f.movement(tx, id(), "depreciation", [6]int64{0, 50, 0, 45, 0, 0}, deprec)
	})
}

// TestAssetAcquisitionRules: an acquisition starts posted, is reversed at
// most once and keeps what it posted; it comes from a line or against an
// offset account, never both nor neither, and takes its asset's seq once.
func TestAssetAcquisitionRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	acq := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, acquisitionSQL, f.tenant, acq, f.entity, f.active, 2, 1000, id(), nil, "posted", f.actor)
	})
	status := `UPDATE assets.acquisition SET status = $2, reversal_posting_date = '2026-06-02', reversal_reason = 'r', version = version + 1 WHERE id = $1`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"an acquisition starting reversed": {acquisitionSQL, []any{f.tenant, id(), f.entity, f.active, 3, 1000, id(), nil, "reversed", f.actor}, "starts posted"},
		"an amount of zero":                {acquisitionSQL, []any{f.tenant, id(), f.entity, f.active, 3, 0, id(), nil, "posted", f.actor}, "acquisition_amount_check"},
		"a seq taken":                      {acquisitionSQL, []any{f.tenant, id(), f.entity, f.active, 2, 1000, id(), nil, "posted", f.actor}, "acquisition_seq_once"},
		"a line and an offset account":     {acquisitionSQL, []any{f.tenant, id(), f.entity, f.active, 3, 1000, id(), id(), "posted", f.actor}, "acquisition_source"},
		"neither": {`INSERT INTO assets.acquisition (tenant_id, id, legal_entity_id, asset_id, seq, value_date, posting_date, fiscal_year, currency, amount,
			amount_lc, amount_gc, currency_lc, currency_gc, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $3, 3, '2026-06-01', '2026-06-01',
			2026, 'EUR', 1, 1, 1, 'EUR', 'EUR', 'posted', $4, $4)`, []any{f.tenant, f.entity, f.active, f.actor}, "acquisition_source"},
		"an amount changed":      {`UPDATE assets.acquisition SET amount = 999, version = version + 1 WHERE id = $1`, []any{acq}, "keeps what it posted"},
		"an acquisition deleted": {`DELETE FROM assets.acquisition WHERE id = $1`, []any{acq}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, status, acq, "reversed") })
	failsWith(t, f.try(t, status, acq, "posted"), "cannot change from reversed to posted")
}

// TestClearingUseRules: what is used of a line stays between zero and the
// line's amount in each currency (the mirror for a credit line), and the
// line's amounts never change.
func TestClearingUseRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	for name, c := range map[string]struct {
		amounts [6]int64 // line_amount, line_lc, line_gc, used, used_lc, used_gc
		want    string
	}{
		"more than a debit line":       {[6]int64{100, 90, 80, 101, 90, 80}, "clearing_use_within_line"},
		"less than nothing of a debit": {[6]int64{100, 90, 80, -1, 0, 0}, "clearing_use_within_line"},
		"more than a credit line":      {[6]int64{-100, -90, -80, -101, -90, -80}, "clearing_use_within_line"},
		"a debit of a credit line":     {[6]int64{-100, -90, -80, 1, 0, 0}, "clearing_use_within_line"},
		"more local amount":            {[6]int64{100, 90, 80, 100, 91, 80}, "clearing_use_within_line"},
		"more group amount":            {[6]int64{100, 90, 80, 100, 90, 81}, "clearing_use_within_line"},
		"a line of zero":               {[6]int64{0, 0, 0, 0, 0, 0}, "clearing_use_line_amount_check"},
	} {
		t.Run(name, func(t *testing.T) {
			a := c.amounts
			failsWith(t, f.try(t, useSQL, f.tenant, f.entity, a[0], a[1], a[2], a[3], a[4], a[5]), c.want)
		})
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, useSQL, f.tenant, f.entity, -100, -90, -80, -100, -90, -80); err != nil {
			return err
		}
		return exec(tx, useSQL, f.tenant, f.entity, 100, 90, 80, 50, 45, 40)
	})
	failsWith(t, f.try(t, `UPDATE assets.clearing_use SET line_amount = 200, version = version + 1 WHERE line_amount = 100`), "keeps its amounts")
}

// TestDocumentEntryRules: an assets document's entry is its main one or a
// catch-up, once per document, ledger and part, and is written once.
func TestDocumentEntryRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	doc := id()
	entrySQL := `INSERT INTO assets.document_entry (tenant_id, document_kind, document_id, ledger_id, part, journal_entry_year, journal_entry_id, document_number)
		VALUES ($1, 'acquisition', $2, $3, $4, 2026, gen_random_uuid(), '1')`
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, entrySQL, f.tenant, doc, f.ledger, "main") })
	failsWith(t, f.try(t, entrySQL, f.tenant, doc, f.ledger, "main"), "document_entry_pkey")
	failsWith(t, f.try(t, entrySQL, f.tenant, doc, f.ledger, "extra"), "document_entry_part_check")
	failsWith(t, f.try(t, `UPDATE assets.document_entry SET document_number = '2' WHERE document_id = $1`, doc), "permission denied")
}

// TestAcquisitionReversalRules (T57d, migration 000195): an acquisition is
// reversed exactly when its reversal's date and reason are recorded, never
// dated before it, and keeps them; a reversal's line's use names the line
// it reverses, both or neither, and keeps it; a document's entry records
// its reversal entry once, with its number, and nothing else of it
// changes.
func TestAcquisitionReversalRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	acq := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, acquisitionSQL, f.tenant, acq, f.entity, f.active, 2, 1000, id(), nil, "posted", f.actor)
	})
	reverse := `UPDATE assets.acquisition SET status = 'reversed', reversal_posting_date = $2::date, reversal_reason = $3, version = version + 1 WHERE id = $1`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"reversed without its date":        {`UPDATE assets.acquisition SET status = 'reversed', version = version + 1 WHERE id = $1`, []any{acq}, "acquisition_reversal"},
		"a reversal recorded while posted": {`UPDATE assets.acquisition SET reversal_posting_date = '2026-06-02', reversal_reason = 'r', version = version + 1 WHERE id = $1`, []any{acq}, "acquisition_reversal"},
		"reversed without a reason":        {reverse, []any{acq, "2026-06-02", nil}, "acquisition_reversal"},
		"an empty reason":                  {reverse, []any{acq, "2026-06-02", ""}, "acquisition_reversal_reason_check"},
		"reversed before it was posted":    {reverse, []any{acq, "2026-05-31", "r"}, "acquisition_reversal_date"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, reverse, acq, "2026-06-01", "r") })
	failsWith(t, f.try(t, `UPDATE assets.acquisition SET reversal_reason = 'other', version = version + 1 WHERE id = $1`, acq), "keeps what it posted")

	reversal := `INSERT INTO assets.clearing_use (tenant_id, entry_year, entry_id, line_no, legal_entity_id, currency, line_amount, line_lc, line_gc,
		reverses_entry_year, reverses_entry_id) VALUES ($1, 2026, $2, 1, $3, 'EUR', -100, -100, -100, $4, $5)`
	failsWith(t, f.try(t, reversal, f.tenant, id(), f.entity, 2026, nil), "clearing_use_reverses")
	use := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, reversal, f.tenant, use, f.entity, 2026, id()) })
	failsWith(t, f.try(t, `UPDATE assets.clearing_use SET reverses_entry_id = gen_random_uuid(), version = version + 1 WHERE entry_id = $1`, use), "keeps its amounts")

	doc := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO assets.document_entry (tenant_id, document_kind, document_id, ledger_id, part, journal_entry_year, journal_entry_id,
			document_number) VALUES ($1, 'acquisition', $2, $3, 'main', 2026, gen_random_uuid(), '1')`, f.tenant, doc, f.ledger)
	})
	record := `UPDATE assets.document_entry SET reversal_entry_year = 2026, reversal_entry_id = gen_random_uuid(), reversal_document_number = $2 WHERE document_id = $1`
	failsWith(t, f.try(t, `UPDATE assets.document_entry SET reversal_entry_year = 2026, reversal_entry_id = gen_random_uuid() WHERE document_id = $1`, doc),
		"document_entry_reversal_number")
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, record, doc, "2") })
	failsWith(t, f.try(t, record, doc, "3"), "is reversed once")
	failsWith(t, f.try(t, `UPDATE assets.document_entry SET journal_entry_year = 2027 WHERE document_id = $1`, doc), "permission denied")
}

// Depreciation runs (T57f, migration 000200).
const (
	depreciationRunSQL = `INSERT INTO assets.depreciation_run (tenant_id, id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, posting_date, currency, status,
		operation_id, created_by, updated_by) VALUES ($1, $2, $3, $4, 2026, $5, '2026-06-30', 'EUR', $6, gen_random_uuid(), $7, $7)`
	depreciationBatchSQL = `INSERT INTO assets.depreciation_batch (tenant_id, run_id, batch_no, asset_ids, status) VALUES ($1, $2, $3, $4, $5)`
)

// TestDepreciationRunRules: a run starts queued and moves queued ->
// running -> posted, or to failed from queued or running, posted and
// failed being final; it is planned once running, has its totals exactly
// when posted and its problem exactly when failed, and keeps what it
// runs; one run of a company and ledger is queued or running at a time; a
// batch holds 1 to 500 assets, starts pending, is posted once with its
// totals, has its entry only when posted, and keeps its assets.
func TestDepreciationRunRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	run, other := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, depreciationRunSQL, f.tenant, run, f.entity, f.ledger, 6, "queued", f.actor)
	})
	running := `UPDATE assets.depreciation_run SET status = 'running', assets_planned = $2, version = version + 1 WHERE id = $1`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a run starting running":     {depreciationRunSQL, []any{f.tenant, id(), f.entity, f.ledger2, 6, "running", f.actor}, "starts queued"},
		"a second run of the ledger": {depreciationRunSQL, []any{f.tenant, other, f.entity, f.ledger, 7, "queued", f.actor}, "depreciation_run_one_at_a_time"},
		"a special period":           {depreciationRunSQL, []any{f.tenant, id(), f.entity, f.ledger2, 13, "queued", f.actor}, "depreciation_run_fiscal_period_check"},
		"running without a plan":     {`UPDATE assets.depreciation_run SET status = 'running', version = version + 1 WHERE id = $1`, []any{run}, "depreciation_run_planned"},
		"posted from queued":         {`UPDATE assets.depreciation_run SET status = 'posted', amount = 0, assets_posted = 0, assets_skipped = 0, version = version + 1 WHERE id = $1`, []any{run}, "cannot change from queued to posted"},
		"failed without its problem": {`UPDATE assets.depreciation_run SET status = 'failed', version = version + 1 WHERE id = $1`, []any{run}, "depreciation_run_error"},
		"a problem while queued":     {`UPDATE assets.depreciation_run SET error = '{}', version = version + 1 WHERE id = $1`, []any{run}, "depreciation_run_error"},
		"totals while queued":        {`UPDATE assets.depreciation_run SET amount = 0, assets_posted = 0, assets_skipped = 0, version = version + 1 WHERE id = $1`, []any{run}, "depreciation_run_totals"},
		"its period changed":         {`UPDATE assets.depreciation_run SET fiscal_period = 7, version = version + 1 WHERE id = $1`, []any{run}, "keeps what it runs"},
		"a run deleted":              {`DELETE FROM assets.depreciation_run WHERE id = $1`, []any{run}, "permission denied"},
		"a batch of no assets":       {depreciationBatchSQL, []any{f.tenant, run, 1, []uuid.UUID{}, "pending"}, "depreciation_batch_asset_ids_check"},
		"a batch of 501 assets":      {depreciationBatchSQL, []any{f.tenant, run, 1, make([]uuid.UUID, 501), "pending"}, "depreciation_batch_asset_ids_check"},
		"a batch starting posted":    {depreciationBatchSQL, []any{f.tenant, run, 1, []uuid.UUID{f.active}, "posted"}, "starts pending"},
		"a batch of no run":          {depreciationBatchSQL, []any{f.tenant, id(), 1, []uuid.UUID{f.active}, "pending"}, "depreciation_batch_tenant_id_run_id_fkey"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, running, run, 1); err != nil {
			return err
		}
		return exec(tx, depreciationBatchSQL, f.tenant, run, 1, []uuid.UUID{f.active}, "pending")
	})
	post := `UPDATE assets.depreciation_batch SET status = 'posted', amount = 1000, assets_posted = 1, assets_skipped = 0, journal_entry_year = $2,
		journal_entry_id = $3, document_number = $4, version = version + 1 WHERE run_id = $1`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"planned again":                 {running, []any{run, 2}, "keeps what it runs, its plan"},
		"a batch posted without totals": {`UPDATE assets.depreciation_batch SET status = 'posted', version = version + 1 WHERE run_id = $1`, []any{run}, "depreciation_batch_posted"},
		"an entry without its number":   {post, []any{run, 2026, id(), nil}, "depreciation_batch_entry"},
		"its assets changed":            {`UPDATE assets.depreciation_batch SET asset_ids = $2, version = version + 1 WHERE run_id = $1`, []any{run, []uuid.UUID{f.planned}}, "keeps its assets"},
		"an entry while pending":        {`UPDATE assets.depreciation_batch SET journal_entry_year = 2026, journal_entry_id = gen_random_uuid(), document_number = '1', version = version + 1 WHERE run_id = $1`, []any{run}, "depreciation_batch_entry"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, f.try(t, c.sql, c.args...), c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, post, run, 2026, id(), "AF-1"); err != nil {
			return err
		}
		return exec(tx, `UPDATE assets.depreciation_run SET status = 'posted', amount = 1000, assets_posted = 1, assets_skipped = 0, version = version + 1 WHERE id = $1`, run)
	})
	failsWith(t, f.try(t, post, run, 2026, id(), "AF-2"), "is posted once")
	failsWith(t, f.try(t, `UPDATE assets.depreciation_run SET amount = 2000, version = version + 1 WHERE id = $1`, run), "its outcome")
	failsWith(t, f.try(t, `UPDATE assets.depreciation_run SET status = 'failed', error = '{}', amount = NULL, assets_posted = NULL, assets_skipped = NULL,
		version = version + 1 WHERE id = $1`, run), "cannot change from posted to failed")
	// The run posted, another of the ledger may start; one failed is final.
	failed := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, depreciationRunSQL, f.tenant, failed, f.entity, f.ledger, 7, "queued", f.actor); err != nil {
			return err
		}
		return exec(tx, `UPDATE assets.depreciation_run SET status = 'failed', error = '{"code": "x"}', version = version + 1 WHERE id = $1`, failed)
	})
	failsWith(t, f.try(t, `UPDATE assets.depreciation_run SET status = 'running', assets_planned = 0, error = NULL, version = version + 1 WHERE id = $1`, failed),
		"cannot change from failed to running")
}

// Asset retirements (T57g, migration 000203).
const retirementSQL = `INSERT INTO assets.retirement (tenant_id, id, legal_entity_id, asset_id, seq, kind, value_date, posting_date, fiscal_year,
		fiscal_period, currency, share_percent, share_cost, proceeds, currency_gc, proceeds_gc, status, created_by, updated_by)
	VALUES ($1, $2, $3, $4, $5, $6, $7::date, '2026-06-15', 2026, 6, 'EUR', $8::numeric, $9, $10, 'USD', $11, $12, $13, $13)`

// TestAssetRetirementRules: a retirement starts posted, is reversed at
// most once, never before it was posted, and keeps what it posted; it is
// a sale or a scrapping, a scrapping without proceeds and no group amount
// without proceeds; its share is a percent above 0 and at most 100 or a
// positive cost, not both; its value date lies in its posting date's
// month, on or before it; it takes its asset's seq once; a movement's
// share lies above 0 and at most 1.
func TestAssetRetirementRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	ret := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, retirementSQL, f.tenant, ret, f.entity, f.active, 2, "sale", "2026-06-15", "50", nil, 700, 760, "posted", f.actor)
	})
	row := func(seq int, kind, value string, percent, cost any, proceeds, proceedsGC int, status string) error {
		return f.try(t, retirementSQL, f.tenant, id(), f.entity, f.active, seq, kind, value, percent, cost, proceeds, proceedsGC, status, f.actor)
	}
	reverse := `UPDATE assets.retirement SET status = 'reversed', reversal_posting_date = $2::date, reversal_reason = $3, version = version + 1 WHERE id = $1`
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"a retirement starting reversed":  {row(3, "scrapping", "2026-06-15", nil, nil, 0, 0, "reversed"), "starts posted"},
		"another kind":                    {row(3, "donation", "2026-06-15", nil, nil, 0, 0, "posted"), "retirement_kind_check"},
		"a scrapping with proceeds":       {row(3, "scrapping", "2026-06-15", nil, nil, 1, 1, "posted"), "retirement_proceeds"},
		"a group amount without proceeds": {row(3, "sale", "2026-06-15", nil, nil, 0, 1, "posted"), "retirement_proceeds"},
		"negative proceeds":               {row(3, "sale", "2026-06-15", nil, nil, -1, -1, "posted"), "retirement_proceeds_check"},
		"a percent and a cost":            {row(3, "scrapping", "2026-06-15", "5", 100, 0, 0, "posted"), "retirement_share"},
		"a percent of zero":               {row(3, "scrapping", "2026-06-15", "0", nil, 0, 0, "posted"), "retirement_share_percent_check"},
		"a percent above 100":             {row(3, "scrapping", "2026-06-15", "100.01", nil, 0, 0, "posted"), "retirement_share_percent_check"},
		"a cost of zero":                  {row(3, "scrapping", "2026-06-15", nil, 0, 0, 0, "posted"), "retirement_share_cost_check"},
		"valued in the month before":      {row(3, "scrapping", "2026-05-31", nil, nil, 0, 0, "posted"), "retirement_value_date"},
		"valued after its posting date":   {row(3, "scrapping", "2026-06-16", nil, nil, 0, 0, "posted"), "retirement_value_date"},
		"a seq taken":                     {row(2, "scrapping", "2026-06-15", nil, nil, 0, 0, "posted"), "retirement_seq_once"},
		"reversed without its date":       {f.try(t, `UPDATE assets.retirement SET status = 'reversed', version = version + 1 WHERE id = $1`, ret), "retirement_reversal"},
		"reversed without a reason":       {f.try(t, reverse, ret, "2026-06-16", nil), "retirement_reversal"},
		"reversed before it was posted":   {f.try(t, reverse, ret, "2026-06-14", "r"), "retirement_reversal_date"},
		"proceeds changed":                {f.try(t, `UPDATE assets.retirement SET proceeds = 1, version = version + 1 WHERE id = $1`, ret), "keeps what it posted"},
		"a retirement deleted":            {f.try(t, `DELETE FROM assets.retirement WHERE id = $1`, ret), "permission denied"},
		"a movement's share of zero":      {f.try(t, movementShareSQL, f.tenant, f.active, f.entity, f.ledger, "0", f.actor), "movement_share"},
		"a movement's share above all":    {f.try(t, movementShareSQL, f.tenant, f.active, f.entity, f.ledger, "1.5", f.actor), "movement_share"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, c.err, c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, movementShareSQL, f.tenant, f.active, f.entity, f.ledger, "1", f.actor)
	})
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, reverse, ret, "2026-06-15", "r") })
	failsWith(t, f.try(t, `UPDATE assets.retirement SET status = 'posted', version = version + 1 WHERE id = $1`, ret), "cannot change from reversed to posted")
	failsWith(t, f.try(t, `UPDATE assets.retirement SET reversal_reason = 'other', version = version + 1 WHERE id = $1`, ret), "keeps what it posted")
}

// movementShareSQL writes a retirement movement taking a share.
const movementShareSQL = `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
		fiscal_year, fiscal_period, share, document_kind, document_id, created_by)
	VALUES ($1, gen_random_uuid(), $2, 2, $3, $4, 'MACHINES', 'retirement', '2026-06-15', '2026-06-15', 2026, 6, $5::numeric, 'retirement', gen_random_uuid(), $6)`
