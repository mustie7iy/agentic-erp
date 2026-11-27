package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// deliveryIssueLine writes a goods issue line of the scene's delivery's
// item 1.
const deliveryIssueLine = `INSERT INTO shipping.outbound_delivery_issue_line (tenant_id, outbound_delivery_id, line_no, item_no, quantity, batch_id,
	batch_number, serial_numbers) VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8)`

// TestDeliveryIssueTracking (S2f2, S2g2; migration 000491): a goods issue
// line names an item of its delivery, a line number from 1 to 200 once
// per delivery and a quantity above zero, its batch by id and number
// together, one to 1000 serial numbers, and at least a batch or serial
// numbers. It is insert-only, and only its delivery's tenant writes it.
func TestDeliveryIssueTracking(t *testing.T) {
	need(t)
	serials := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "G" + strings.Repeat("1", 1+i%5)
		}
		return out
	}
	batch, number := uuid.New(), "B1"
	type line struct {
		no, item int
		qty      string
		batch    *uuid.UUID
		number   *string
		serials  []string
	}
	write := func(s shippingScene, l line) stmt {
		return step(deliveryIssueLine, s.tenant, s.delivery, l.no, l.item, l.qty, l.batch, l.number, l.serials)
	}
	for _, c := range []struct {
		name string
		run  func(s shippingScene) []stmt
		want string // empty: allowed
	}{
		{"a batch by id and number", func(s shippingScene) []stmt { return append(s.made(), write(s, line{1, 1, "2", &batch, &number, nil})) }, ""},
		{"serial numbers", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", nil, nil, []string{"G1", "G2"}}))
		}, ""},
		{"a batch with 1000 serial numbers", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{200, 1, "1000", &batch, &number, serials(1000)}))
		}, ""},
		{"two lines of an item", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", &batch, &number, nil}), write(s, line{2, 1, "3", &batch, &number, nil}))
		}, ""},
		{"a line twice", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", &batch, &number, nil}), write(s, line{1, 1, "3", &batch, &number, nil}))
		}, "outbound_delivery_issue_line_pkey"},
		{"line 0", func(s shippingScene) []stmt { return append(s.made(), write(s, line{0, 1, "2", &batch, &number, nil})) },
			"outbound_delivery_issue_line_no_check"},
		{"line 201", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{201, 1, "2", &batch, &number, nil}))
		},
			"outbound_delivery_issue_line_no_check"},
		{"no quantity", func(s shippingScene) []stmt { return append(s.made(), write(s, line{1, 1, "0", &batch, &number, nil})) },
			"outbound_delivery_issue_line_quantity_check"},
		{"an item the delivery lacks", func(s shippingScene) []stmt { return append(s.made(), write(s, line{1, 2, "2", &batch, &number, nil})) },
			"outbound_delivery_issue_line_item_fkey"},
		{"a batch without its number", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", &batch, nil, []string{"G1"}}))
		},
			"outbound_delivery_issue_line_batch"},
		{"a batch number without its batch", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", nil, &number, []string{"G1"}}))
		}, "outbound_delivery_issue_line_batch"},
		{"neither a batch nor serial numbers", func(s shippingScene) []stmt { return append(s.made(), write(s, line{1, 1, "2", nil, nil, nil})) },
			"outbound_delivery_issue_line_tracked"},
		{"no serial numbers", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", &batch, &number, []string{}}))
		},
			"outbound_delivery_issue_line_serial_numbers"},
		{"1001 serial numbers", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", nil, nil, serials(1001)}))
		},
			"outbound_delivery_issue_line_serial_numbers"},
		{"a line changed", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", &batch, &number, nil}), step(`UPDATE shipping.outbound_delivery_issue_line SET quantity = 1`))
		}, "permission denied"},
		{"a line deleted", func(s shippingScene) []stmt {
			return append(s.made(), write(s, line{1, 1, "2", &batch, &number, nil}), step(`DELETE FROM shipping.outbound_delivery_issue_line`))
		}, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("another tenant's line", func(t *testing.T) {
		s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			return all(tx, append(s.made(), step(deliveryIssueLine, id(), s.delivery, 1, 1, "2", &batch, &number, nil))...)
		})
		failsWith(t, err, "row-level security")
	})
}

// TestDeliveryIssueTrackingAsOwner: past the grants, as the owner, a goods
// issue line is neither changed nor deleted.
func TestDeliveryIssueTrackingAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	batch, number := uuid.New(), "B1"
	for name, change := range map[string]string{
		"changed": `UPDATE shipping.outbound_delivery_issue_line SET quantity = 1 WHERE outbound_delivery_id = $1`,
		"deleted": `DELETE FROM shipping.outbound_delivery_issue_line WHERE outbound_delivery_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, append(s.made(), step(deliveryIssueLine, s.tenant, s.delivery, 1, 1, "2", &batch, &number, nil))...); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, change, s.delivery), "insert-only")
		})
	}
}

// TestDeliveryIssueTrackingDownRefused: 000491's down migration refuses to
// drop goods issue lines (06 §6.4: a down migration loses no data),
// reading every tenant's rows.
func TestDeliveryIssueTrackingDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, delivery, batch := id(), id(), id()
	// The line is written with the triggers and foreign keys off (its item
	// would be a fixture's), as the guard reads the table alone.
	if err := all(tx, step(`SET LOCAL session_replication_role = replica`),
		step(deliveryIssueLine, tenant, delivery, 1, 1, "1", batch, "B1", nil),
		step(`SET LOCAL session_replication_role = origin`), step(`SAVEPOINT migrator`)); err != nil {
		t.Fatal(err)
	}
	down := migrationNamed(t, "000491_delivery_issue_tracking").Down
	// A migrating role held to row-level security fails the guard rather
	// than finding no rows (row_security = off).
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "goods issues name batches or serial numbers")
}
