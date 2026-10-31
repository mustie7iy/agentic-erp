package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRequisitionDate: every requisition has a date (T72n5, migrations
// 000170-000171). The application dates it in its company's time zone; a
// row the previous release inserts without one is dated by its rule, UTC;
// none is without a date.
func TestRequisitionDate(t *testing.T) {
	need(t)
	insert := func(f fixture, tx pgx.Tx, column, value string) error {
		r := id()
		return exec(tx, `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, status,
			created_by, updated_by`+column+`) VALUES ($1, $2, $3, $4, $5, 'USD', 'draft', $5, $5`+value+`)`, f.tenant, r, r.String()[24:], f.entity, f.actor)
	}
	for _, c := range []struct{ name, column, value, want string }{
		{"dated by the application", ", requisition_date", ", '2026-12-31'", ""},
		{"dated by the previous release's rule", "", "", ""},
		{"without a date", ", requisition_date", ", NULL", "requisition_dated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return insert(f, tx, c.column, c.value) })
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

// TestRequisitionDateBackfilled: a requisition raised before 000170 is
// dated as it was routed then, created_at's date in UTC, keeping its
// version: 23:30 on 31 December in UTC-5 is 1 January in UTC.
func TestRequisitionDateBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 170)
	f := fixtureIDs()
	r := id()
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, status,
			created_at, created_by, updated_by) VALUES ($1, $2, 'PR 1', $3, $4, 'USD', 'draft', '2026-12-31T23:30:00-05:00', $4, $4)`, f.tenant, r, f.entity, f.actor)
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var date time.Time
	var version int32
	if err := conn.QueryRow(context.Background(), `SELECT requisition_date, version FROM procurement.requisition WHERE id = $1`, r).Scan(&date, &version); err != nil {
		t.Fatal(err)
	}
	if got := date.Format(time.DateOnly); got != "2027-01-01" {
		t.Errorf("requisition_date: got %s, want 2027-01-01", got)
	}
	if version != 1 {
		t.Errorf("version: got %d, want 1", version)
	}
}
