package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Asset impairments (migrations 000469-000472; ADR-0057 As built,
// impairment): what the database keeps of impairment losses and their
// reversals, their movements and the history sheet's columns for them.

// impairmentSQL writes an impairment of f.active in f.ledger with the seq,
// kind, value date, amount, carrying and unimpaired amounts and text.
const impairmentSQL = `INSERT INTO assets.impairment (tenant_id, id, legal_entity_id, asset_id, seq, ledger_id, kind, value_date, posting_date,
		fiscal_year, fiscal_period, currency, amount, currency_gc, amount_gc, carrying_amount, unimpaired_amount, text, created_by)
	VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, $6, $7::date, '2026-06-15', 2026, 6, 'EUR', $8, 'USD', $8, $9, $10, $11, $12)`

// impairmentMovementSQL writes an impairment movement of f.active in
// f.ledger with the kind, cost, accumulated depreciation, document kind
// and the movement it reverses.
const impairmentMovementSQL = `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
		posting_date, fiscal_year, fiscal_period, cost, accumulated, accumulated_gc, document_kind, document_id, reverses_id, created_by)
	VALUES ($1, $2, $3, 3, $4, $5, 'MACHINES', $6, '2026-06-15', '2026-06-15', 2026, 6, $7, $8, $8, $9, gen_random_uuid(), $10, $11)`

// TestAssetImpairmentRules: an impairment is a loss or a reversal of a
// positive amount, valued in its posting period on or before its posting
// date, a loss at most the carrying amount and a reversal at most what
// the impairments took, takes its asset's seq once and is never changed
// or deleted, by the application or its owner; its movement changes the
// accumulated depreciation alone, a loss by a credit and a reversal by a
// debit, and only an impairment document writes one.
func TestAssetImpairmentRules(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	row := func(seq int, kind, value string, amount, carrying, unimpaired int, text any) error {
		return f.try(t, impairmentSQL, f.tenant, f.entity, f.active, seq, f.ledger, kind, value, amount, carrying, unimpaired, text, f.actor)
	}
	loss := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, impairmentSQL, f.tenant, f.entity, f.active, 2, f.ledger, "loss", "2026-06-15", 300, 1000, 1000, "Damaged", f.actor); err != nil {
			return err
		}
		return exec(tx, impairmentMovementSQL, f.tenant, loss, f.active, f.entity, f.ledger, "impairment", 0, -300, "impairment", nil, f.actor)
	})
	movement := func(kind string, cost, accumulated int, document string, reverses any) error {
		return f.try(t, impairmentMovementSQL, f.tenant, id(), f.active, f.entity, f.ledger, kind, cost, accumulated, document, reverses, f.actor)
	}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"another kind":                   {row(3, "write_up", "2026-06-15", 1, 1000, 1000, nil), "impairment_kind_check"},
		"an amount of zero":              {row(3, "loss", "2026-06-15", 0, 1000, 1000, nil), "impairment_amount_check"},
		"valued in the month before":     {row(3, "loss", "2026-05-31", 1, 1000, 1000, nil), "impairment_value_date"},
		"valued after its posting date":  {row(3, "loss", "2026-06-16", 1, 1000, 1000, nil), "impairment_value_date"},
		"a loss above the carrying":      {row(3, "loss", "2026-06-15", 1001, 1000, 1000, nil), "impairment_limit"},
		"a reversal above its ceiling":   {row(3, "reversal", "2026-06-15", 301, 700, 1000, nil), "impairment_limit"},
		"an empty text":                  {row(3, "loss", "2026-06-15", 1, 1000, 1000, ""), "impairment_text_check"},
		"a seq taken":                    {row(2, "loss", "2026-06-15", 1, 1000, 1000, nil), "impairment_seq_once"},
		"an impairment changed":          {f.try(t, `UPDATE assets.impairment SET amount = 1 WHERE asset_id = $1`, f.active), "permission denied"},
		"an impairment deleted":          {f.try(t, `DELETE FROM assets.impairment WHERE asset_id = $1`, f.active), "permission denied"},
		"an impairment of cost":          {movement("impairment", 1, -1, "impairment", nil), "movement_impairment"},
		"a loss debited":                 {movement("impairment", 0, 1, "impairment", nil), "movement_impairment"},
		"a reversal credited":            {movement("impairment_reversal", 0, -1, "impairment", nil), "movement_impairment"},
		"an impairment of nothing":       {movement("impairment_reversal", 0, 0, "impairment", nil), "movement_impairment"},
		"an impairment of a retirement":  {movement("impairment", 0, -1, "retirement", nil), "movement_impairment"},
		"an impairment document's other": {movement("depreciation", 0, -1, "impairment", nil), "movement_impairment_document"},
		"an unknown document kind":       {movement("depreciation", 0, -1, "revaluation", nil), "movement_document_kind_check"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, c.err, c.want) })
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, impairmentSQL, f.tenant, f.entity, f.active, 4, f.ledger, "reversal", "2026-06-01", 300, 700, 1000, nil, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO assets.document_entry (tenant_id, document_kind, document_id, ledger_id, part, journal_entry_year, journal_entry_id,
				document_number) VALUES ($1, 'impairment', gen_random_uuid(), $2, 'main', 2026, gen_random_uuid(), 'AA1')`, f.tenant, f.ledger); err != nil {
			return err
		}
		// A reversal row negating a loss has the reversal's sign.
		return exec(tx, impairmentMovementSQL, f.tenant, id(), f.active, f.entity, f.ledger, "impairment", 0, 300, "impairment", loss, f.actor)
	})
	for name, sql := range map[string]string{
		"changed by its owner": `UPDATE assets.impairment SET amount = 1 WHERE asset_id = $1`,
		"deleted by its owner": `DELETE FROM assets.impairment WHERE asset_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, asOwner(func(tx pgx.Tx) error { return exec(tx, sql, f.active) })), "insert-only")
		})
	}
}

// TestAssetImpairmentTotals: the application adds to the year totals'
// impairment columns, as to the others.
func TestAssetImpairmentTotals(t *testing.T) {
	need(t)
	f := newAssetFixture(t)
	for range 2 {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO assets.year_total AS t (tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code,
					impairment_losses, impairment_reversals) VALUES ($1, $2, $3, 2026, $4, 'MACHINES', -100, 40)
				ON CONFLICT (tenant_id, ledger_id, asset_id, fiscal_year) DO UPDATE SET impairment_losses = t.impairment_losses + EXCLUDED.impairment_losses,
					impairment_reversals = t.impairment_reversals + EXCLUDED.impairment_reversals`, f.tenant, f.ledger, f.active, f.entity); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO assets.class_year_total AS t (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, impairment_losses,
					impairment_reversals) VALUES ($1, $2, $3, 2026, 'MACHINES', -100, 40)
				ON CONFLICT (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code) DO UPDATE SET
					impairment_losses = t.impairment_losses + EXCLUDED.impairment_losses, impairment_reversals = t.impairment_reversals + EXCLUDED.impairment_reversals`,
				f.tenant, f.entity, f.ledger)
		})
	}
	var losses, reversals, classLosses, classReversals int64
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT y.impairment_losses, y.impairment_reversals, c.impairment_losses, c.impairment_reversals
			FROM assets.year_total y, assets.class_year_total c`).Scan(&losses, &reversals, &classLosses, &classReversals)
	})
	want := [4]int64{-200, 80, -200, 80}
	if got := [4]int64{losses, reversals, classLosses, classReversals}; got != want {
		t.Errorf("the impairment columns added to: got %v, want %v", got, want)
	}
}

// TestAssetImpairmentsDown: 000469's down refuses, rather than drops, the
// impairments and their movements the previous version cannot hold (06
// §6.4), and a role held to row-level security that would see none of
// them fails rather than drops them.
func TestAssetImpairmentsDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000469_asset_impairments").Down
	for name, seed := range map[string]string{
		"an impairment": `INSERT INTO assets.impairment (tenant_id, id, legal_entity_id, asset_id, seq, ledger_id, kind, value_date, posting_date,
			fiscal_year, fiscal_period, currency, amount, currency_gc, amount_gc, carrying_amount, unimpaired_amount, created_by)
			VALUES ($1, gen_random_uuid(), $1, gen_random_uuid(), 1, $1, 'loss', '2026-06-15', '2026-06-15', 2026, 6, 'EUR', 1, 'USD', 1, 1, 1, $1)`,
		"a movement": `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, accumulated, document_kind, document_id, created_by)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 1, $1, $1, 'MACHINES', 'impairment', '2026-06-15', '2026-06-15', 2026, 6, -1, 'impairment',
				gen_random_uuid(), $1)`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant := id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(seed, tenant), step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`),
				step(`SAVEPOINT migrator`)); err != nil {
				t.Fatal(err)
			}
			if err := asMigrator(tx); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "row-level security")
			if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "asset impairments exist")
		})
	}
}
