package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRequisitionDate: every requisition has a date (T72n5, migrations
// 000170-000171). The application dates it in its company's time zone; a
// row the previous release inserts without one is dated by its rule, UTC's
// today; none is without a date.
func TestRequisitionDate(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name, column string
		given        *string // the date inserted, when column names it
		want         string  // the constraint the insert fails, if any
	}{
		{"dated by the application", ", requisition_date", new("2026-12-31"), ""},
		{"dated by the previous release's rule", "", nil, ""},
		{"without a date", ", requisition_date", nil, "requisition_dated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			r, value := id(), ""
			if c.column != "" {
				value = ", $6::date"
			}
			args := []any{f.tenant, r, r.String()[24:], f.entity, f.actor}
			if c.column != "" {
				args = append(args, c.given)
			}
			var dated bool
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				if err := exec(tx, `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, status,
					created_by, updated_by`+c.column+`) VALUES ($1, $2, $3, $4, $5, 'USD', 'draft', $5, $5`+value+`)`, args...); err != nil {
					return err
				}
				return tx.QueryRow(context.Background(), `SELECT requisition_date = coalesce($2::date, (now() AT TIME ZONE 'UTC')::date)
					FROM procurement.requisition WHERE id = $1`, r, c.given).Scan(&dated)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if !dated {
				t.Errorf("requisition_date: got another date, want %v (nil: UTC's today)", c.given)
			}
		})
	}
}

// TestRequisitionDateBackfilled: a requisition raised before 000170 is
// dated as it was routed then, created_at's date in UTC, keeping its
// version: 23:30 on 31 December in UTC-5 is 1 January in UTC.
func TestRequisitionDateBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000170_requisition_date")
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
