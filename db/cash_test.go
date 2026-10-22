package db

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The cash context's configuration (T53b, migrations 000141-000143): a
// liquidity item's code, name and direction, its parent another item of
// the tenant, never itself; a planning group's code and name; a supplier's
// planning group a code of that form. Items and groups are deactivated,
// never deleted, and change by versions.
func TestCashConfigurationRules(t *testing.T) {
	need(t)
	const (
		item  = `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, parent_code, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $6)`
		group = `INSERT INTO cash.planning_group (tenant_id, code, name, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`
		set   = `INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code,
			planning_group, created_by, updated_by) VALUES ($1, $2, $3, $4, 'NT30', $5, $6, $6)`
	)
	top := func(f fixture) stmt { return step(item, f.tenant, "OPERATING", "Operating", "both", nil, f.actor) }
	for _, c := range []struct {
		name string
		run  func(tx pgx.Tx, f fixture) error
		want string // empty: allowed
	}{
		{"a tree of two items", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(item, f.tenant, "OP_RENT", "Rent", "outflow", "OPERATING", f.actor))
		}, ""},
		{"a child before its parent in one statement", func(tx pgx.Tx, f fixture) error {
			return exec(tx, `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, parent_code, created_by, updated_by)
				VALUES ($1, 'OP_RENT', 'Rent', 'outflow', 'OPERATING', $2, $2), ($1, 'OPERATING', 'Operating', 'both', NULL, $2, $2)`, f.tenant, f.actor)
		}, ""},
		{"an item renamed and deactivated", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(`UPDATE cash.liquidity_item SET name = 'Ops', is_active = false, version = version + 1 WHERE code = 'OPERATING'`))
		}, ""},
		{"an item changed without its version", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(`UPDATE cash.liquidity_item SET name = 'Ops' WHERE code = 'OPERATING'`))
		}, "optimistic lock"},
		{"an item deleted", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(`DELETE FROM cash.liquidity_item WHERE code = 'OPERATING'`))
		}, "permission denied"},
		{"an item code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "op", "Operating", "both", nil, f.actor)
		}, "liquidity_item_code_check"},
		{"an item without a name", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OPERATING", " ", "both", nil, f.actor)
		}, "liquidity_item_name_check"},
		{"an item's direction", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OPERATING", "Operating", "out", nil, f.actor)
		}, "liquidity_item_direction_check"},
		{"an item its own parent", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OPERATING", "Operating", "both", "OPERATING", f.actor)
		}, "liquidity_item_check"},
		{"an unknown parent", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OP_RENT", "Rent", "outflow", "OPERATING", f.actor)
		}, "liquidity_item_tenant_id_parent_code_fkey"},
		{"a planning group", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, "TAX_AUTHORITY", "Tax authorities", f.actor)
		}, ""},
		{"a planning group code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, "_TAX", "Tax authorities", f.actor)
		}, "planning_group_code_check"},
		{"a planning group without a name", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, "TAX", "", f.actor)
		}, "planning_group_name_check"},
		{"a planning group deleted", func(tx pgx.Tx, f fixture) error {
			return all(tx, step(group, f.tenant, "TAX", "Tax", f.actor), step(`DELETE FROM cash.planning_group WHERE code = 'TAX'`))
		}, "permission denied"},
		{"a supplier's planning group", func(tx pgx.Tx, f fixture) error {
			return exec(tx, set, f.tenant, f.supplier, f.entity, f.ap, "TAX_AUTHORITY", f.actor)
		}, ""},
		{"a supplier without one", func(tx pgx.Tx, f fixture) error {
			return exec(tx, set, f.tenant, f.supplier, f.entity, f.ap, nil, f.actor)
		}, ""},
		{"a supplier's planning group code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, set, f.tenant, f.supplier, f.entity, f.ap, "tax", f.actor)
		}, "supplier_company_planning_group"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return c.run(tx, f) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// A liquidity item's parent is the tenant's own: another tenant's item of
// the code is not one.
func TestCashParentIsTheTenants(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	mustApp(t, a.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, created_by, updated_by) VALUES ($1, 'OPERATING', 'Operating', 'both', $2, $2)`,
			a.tenant, a.actor)
	})
	err := asApp(t, b.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, parent_code, created_by, updated_by)
			VALUES ($1, 'OP_RENT', 'Rent', 'outflow', 'OPERATING', $2, $2)`, b.tenant, b.actor)
	})
	failsWith(t, err, "liquidity_item_tenant_id_parent_code_fkey")
}

// TestCashPositionPermission: the cash position's read permission is
// seeded and is no approval permission (T53c, 000149).
func TestCashPositionPermission(t *testing.T) {
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'cash.position.read'`).Scan(&approval); err != nil {
		t.Fatalf("cash.position.read: %v", err)
	}
	if approval {
		t.Error("cash.position.read: got an approval permission, want a read permission")
	}
}

// memoRow is a memo record to insert: a one-off expected receipt of the
// fixture's company into REC by default, with the columns of set changed.
func memoRow(f fixture, set map[string]any) stmt {
	row := map[string]any{"tenant_id": f.tenant, "id": id(), "legal_entity_id": f.entity, "currency": "EUR", "amount": 125000,
		"value_date": "2026-10-30", "expires_on": "2026-11-06", "counts_until": "2026-11-06", "certainty": "expected", "liquidity_item": "REC",
		"status": "active", "origin": "manual", "created_by": f.actor, "updated_by": f.actor}
	for k, v := range set {
		row[k] = v
	}
	cols := slices.Sorted(maps.Keys(row))
	args, marks := make([]any, len(cols)), make([]string, len(cols))
	for i, c := range cols {
		args[i], marks[i] = row[c], fmt.Sprintf("$%d", i+1)
	}
	return step(`INSERT INTO cash.memo_record (`+strings.Join(cols, ", ")+`) VALUES (`+strings.Join(marks, ", ")+`)`, args...)
}

// memoItems are the liquidity items memo records name: REC for inflows,
// PAY for outflows, BOTH, and OLD, inactive.
func memoItems(f fixture) stmt {
	return step(`INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, is_active, created_by, updated_by)
		VALUES ($1, 'REC', 'Receipts', 'inflow', true, $2, $2), ($1, 'PAY', 'Payments', 'outflow', true, $2, $2),
		       ($1, 'BOTH', 'Either', 'both', true, $2, $2), ($1, 'OLD', 'Old', 'both', false, $2, $2)`, f.tenant, f.actor)
}

// memoCase is a schema rule of memo records: the statements after the
// items, and the error they fail with (empty: allowed).
type memoCase struct {
	name string
	run  func(f fixture) []stmt
	want string
}

func one(set map[string]any) func(f fixture) []stmt {
	return func(f fixture) []stmt { return []stmt{memoRow(f, set)} }
}

// series is a monthly series of a year, and an override of its first
// occurrence with set changed.
func series(f fixture, parent uuid.UUID, set map[string]any) []stmt {
	override := map[string]any{"recurrence_of": parent, "occurrence_date": "2026-10-30"}
	for k, v := range set {
		override[k] = v
	}
	return []stmt{memoRow(f, map[string]any{"id": parent, "recurrence": "monthly", "recurrence_count": 12, "counts_until": "2027-10-07"}), memoRow(f, override)}
}

var memoCases = []memoCase{
	{"a one-off receipt", one(nil), ""},
	{"a payment on an item for both", one(map[string]any{"amount": -500, "liquidity_item": "BOTH"}), ""},
	{"no amount", one(map[string]any{"amount": 0}), "memo_record_amount_check"},
	{"a certainty", one(map[string]any{"certainty": "actual"}), "memo_record_certainty_check"},
	{"an unknown currency", one(map[string]any{"currency": "XXZ"}), "memo_record_currency_fkey"},
	{"expiring before its value date", one(map[string]any{"expires_on": "2026-10-29", "counts_until": "2026-10-29"}), "memo_record_expiry"},
	{"a one-off counting past its expiry", one(map[string]any{"counts_until": "2026-12-31"}), "memo_record_counts_until"},
	{"a one-off counting without end", one(map[string]any{"counts_until": nil}), "memo_record_counts_until"},
	{"a series without end", one(map[string]any{"recurrence": "weekly", "counts_until": nil}), ""},
	{"a series ending on a day and after a count", one(map[string]any{"recurrence": "weekly", "recurrence_until": "2027-01-01", "recurrence_count": 3}),
		"memo_record_recurrence"},
	{"a series ending before it starts", one(map[string]any{"recurrence": "weekly", "recurrence_until": "2026-10-01"}), "memo_record_recurrence"},
	{"a one-off with a count", one(map[string]any{"recurrence_count": 3}), "memo_record_recurrence"},
	{"a frequency", one(map[string]any{"recurrence": "daily"}), "memo_record_recurrence_check"},
	{"a count of one", one(map[string]any{"recurrence": "weekly", "recurrence_count": 1}), "memo_record_recurrence_count_check"},
	{"an override of an occurrence", func(f fixture) []stmt { return series(f, id(), nil) }, ""},
	{"an override without its occurrence", func(f fixture) []stmt { return series(f, id(), map[string]any{"occurrence_date": nil}) }, "memo_record_override"},
	{"an override that is a series", func(f fixture) []stmt {
		return series(f, id(), map[string]any{"recurrence": "weekly", "counts_until": nil})
	}, "memo_record_override"},
	{"an occurrence overridden twice", func(f fixture) []stmt {
		parent := id()
		return append(series(f, parent, nil), memoRow(f, map[string]any{"recurrence_of": parent, "occurrence_date": "2026-10-30"}))
	}, "memo_record_override_once"},
	{"an override of no record", one(map[string]any{"recurrence_of": id(), "occurrence_date": "2026-10-30"}), "memo_record_tenant_id_recurrence_of_fkey"},
	{"an agent's proposal", one(map[string]any{"origin": "agent", "status": "proposed", "session_id": id(), "reason": "monthly"}), ""},
	{"an agent's proposal without its session", one(map[string]any{"origin": "agent", "status": "proposed", "reason": "monthly"}), "memo_record_agent"},
	{"an agent's proposal without a reason", one(map[string]any{"origin": "agent", "status": "proposed", "session_id": id()}), "memo_record_agent"},
	{"a person's record proposed", one(map[string]any{"status": "proposed"}), "memo_record_agent"},
	{"a person's record rejected", one(map[string]any{"status": "rejected"}), "memo_record_agent"},
	{"a person's record with a session", one(map[string]any{"session_id": id()}), "memo_record_agent"},
	{"an uploaded record", one(map[string]any{"origin": "upload", "batch_key": "billing-10", "reference": "INV-1"}), ""},
	{"an uploaded record without its batch", one(map[string]any{"origin": "upload", "reference": "INV-1"}), "memo_record_batch"},
	{"an uploaded record without a reference", one(map[string]any{"origin": "upload", "batch_key": "billing-10"}), "memo_record_batch"},
	{"a batch key", one(map[string]any{"origin": "upload", "batch_key": "billing 10", "reference": "INV-1"}), "memo_record_batch_key_check"},
	{"a reference twice in a batch", func(f fixture) []stmt {
		up := map[string]any{"origin": "upload", "batch_key": "billing-10", "reference": "INV-1"}
		return []stmt{memoRow(f, up), memoRow(f, up)}
	}, "memo_record_batch_reference"},
	{"realised by a statement line", one(map[string]any{"status": "realised", "realised_statement_id": id(), "realised_line_no": 1}), ""},
	{"realised by a person", one(map[string]any{"status": "realised", "decided_by": id(), "decided_at": "2026-10-30T10:00:00Z"}), ""},
	{"a statement without its line", one(map[string]any{"status": "realised", "realised_statement_id": id()}), "memo_record_realised"},
	{"an active record with a statement line", one(map[string]any{"realised_statement_id": id(), "realised_line_no": 1}), "memo_record_realised"},
	{"a series realised", one(map[string]any{"status": "realised", "recurrence": "weekly", "counts_until": nil}), "memo_record_realised"},
	{"a decision without its time", one(map[string]any{"decided_by": id()}), "memo_record_decided"},
	{"evidence not a list", one(map[string]any{"evidence": `{"line": 1}`}), "memo_record_evidence_check"},
	{"an unknown liquidity item", one(map[string]any{"liquidity_item": "NONE"}), "memo_record_item_active"},
	{"an inactive liquidity item", one(map[string]any{"liquidity_item": "OLD"}), "memo_record_item_active"},
	{"a receipt on an item for outflows", one(map[string]any{"liquidity_item": "PAY"}), "memo_record_item_direction"},
	{"a payment on an item for inflows", one(map[string]any{"amount": -500}), "memo_record_item_direction"},
	{"a record realised after its item became inactive", func(f fixture) []stmt {
		r := id()
		return []stmt{memoRow(f, map[string]any{"id": r, "liquidity_item": "BOTH"}),
			step(`UPDATE cash.liquidity_item SET is_active = false, version = version + 1 WHERE code = 'BOTH'`),
			step(`UPDATE cash.memo_record SET status = 'realised', decided_by = $2, decided_at = now(), version = version + 1 WHERE id = $1`, r, f.actor)}
	}, ""},
	{"a cancelled record made active on an inactive item", func(f fixture) []stmt {
		r := id()
		return []stmt{memoRow(f, map[string]any{"id": r, "liquidity_item": "BOTH", "status": "cancelled"}),
			step(`UPDATE cash.liquidity_item SET is_active = false, version = version + 1 WHERE code = 'BOTH'`),
			step(`UPDATE cash.memo_record SET status = 'active', version = version + 1 WHERE id = $1`, r)}
	}, "memo_record_item_active"},
	{"a record moved to an inactive item", func(f fixture) []stmt {
		r := id()
		return []stmt{memoRow(f, map[string]any{"id": r}), step(`UPDATE cash.memo_record SET liquidity_item = 'OLD', version = version + 1 WHERE id = $1`, r)}
	}, "memo_record_item_active"},
	{"a record's amount turned against its item", func(f fixture) []stmt {
		r := id()
		return []stmt{memoRow(f, map[string]any{"id": r}), step(`UPDATE cash.memo_record SET amount = -1, version = version + 1 WHERE id = $1`, r)}
	}, "memo_record_item_direction"},
	{"a record changed without its version", func(f fixture) []stmt {
		r := id()
		return []stmt{memoRow(f, map[string]any{"id": r}), step(`UPDATE cash.memo_record SET amount = 1 WHERE id = $1`, r)}
	}, "optimistic lock"},
	{"a record deleted", func(f fixture) []stmt {
		r := id()
		return []stmt{memoRow(f, map[string]any{"id": r}), step(`DELETE FROM cash.memo_record WHERE id = $1`, r)}
	}, "permission denied"},
	{"an event of the cash context", func(f fixture) []stmt {
		return []stmt{step(`INSERT INTO cash.outbox (tenant_id, id, aggregate_type, aggregate_id, event_type, payload, actor_id)
			VALUES ($1, gen_random_uuid(), 'cash.memo_record', gen_random_uuid(), 'com.example.erp.cash.memo_record_changed.v1', '{}', $2)`, f.tenant, f.actor)}
	}, ""},
	{"an event of another context", func(f fixture) []stmt {
		return []stmt{step(`INSERT INTO cash.outbox (tenant_id, id, aggregate_type, aggregate_id, event_type, payload, actor_id)
			VALUES ($1, gen_random_uuid(), 'cash.memo_record', gen_random_uuid(), 'com.example.erp.payments.memo_record_changed.v1', '{}', $2)`, f.tenant, f.actor)}
	}, "outbox_event_type_check"},
}

// TestMemoRecordRules: every rule the database enforces on memo records
// (T53d, 000152): amounts, certainty, expiry, series and their overrides,
// agents' proposals, upload batches, realisation, an active item of the
// record's direction checked as the item or amount is written, versions,
// no deletion; and the cash context's outbox takes only its own events.
func TestMemoRecordRules(t *testing.T) {
	need(t)
	for _, c := range memoCases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, append([]stmt{memoItems(f)}, c.run(f)...)...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			// A trigger's violation names its rule as the constraint.
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.ConstraintName == c.want {
				return
			}
			failsWith(t, err, c.want)
		})
	}
}

// TestMemoRecordPermissions: reading, writing and proposing memo records
// are seeded and none is an approval permission (T53d, 000152).
func TestMemoRecordPermissions(t *testing.T) {
	for _, code := range []string{"cash.memo.read", "cash.memo.write", "cash.memo.propose"} {
		t.Run(code, func(t *testing.T) {
			var approval bool
			if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = $1`, code).Scan(&approval); err != nil {
				t.Fatalf("%s: %v", code, err)
			}
			if approval {
				t.Errorf("%s: got an approval permission, want none", code)
			}
		})
	}
}

// TestCashOutboxRelay: the outbox relay reads the cash context's events
// and marks them published, and nothing else of the schema.
func TestCashOutboxRelay(t *testing.T) {
	need(t)
	for table, want := range map[string]bool{"cash.outbox": true, "cash.memo_record": false} {
		t.Run(table, func(t *testing.T) {
			var ok bool
			if err := testDB.QueryRow(context.Background(), `SELECT has_table_privilege('erp_relay', $1, 'SELECT')`, table).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok != want {
				t.Errorf("erp_relay reads %s: got %v, want %v", table, ok, want)
			}
		})
	}
}
