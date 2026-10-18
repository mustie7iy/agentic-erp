package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Returns to the supplier (T47b, migration 000064): a 122 sends goods back
// with its reason, a 123 reverses it, each reverses only its own kind, and
// every movement moves its quantity its own way.
func TestGoodsReturns(t *testing.T) {
	need(t)
	price := int64(250)
	// scene is a released order of 10 EA with 10 received.
	scene := func(t *testing.T) (fixture, uuid.UUID, uuid.UUID) {
		t.Helper()
		f := newFixture(t)
		var po, receipt uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
			if po, err = f.order(tx, &price); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
		})
		mustApp(t, f.tenant, f.move(&receipt, po, "101", nil, nil, "10"))
		return f, po, receipt
	}
	reason := "Damaged"
	for _, c := range []struct {
		name     string
		mt       string
		reverses bool // the receipt
		reason   *string
		quantity string
		want     string // empty: allowed
	}{
		{"a return", "122", false, &reason, "-4", ""},
		{"a return without its reason", "122", false, nil, "-4", "goods_movement_return_reason"},
		{"a reason on a receipt", "101", false, &reason, "1", "goods_movement_return_reason"},
		{"a return of more than was received", "122", false, &reason, "-11", "reversal exceeds"},
		{"a return taking goods in", "122", false, &reason, "4", "moves its quantity the other way"},
		{"a receipt sending goods back", "101", false, nil, "-1", "moves its quantity the other way"},
		{"a return reversing a receipt", "122", true, &reason, "-1", "does not reverse that movement"},
		{"a 123 reversing a receipt", "123", true, nil, "1", "does not reverse that movement"},
		{"a 123 reversing nothing", "123", false, nil, "1", "goods_movement_reversal_pair"},
		{"an empty reason", "122", false, new(""), "-1", "goods_movement_reason_check"},
		{"a reason of 201 characters", "122", false, new(strings.Repeat("r", 201)), "-1", "goods_movement_reason_check"},
		{"a 102 taking goods in", "102", true, nil, "1", "moves its quantity the other way"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, po, receipt := scene(t)
			var of *uuid.UUID
			if c.reverses {
				of = &receipt
			}
			var gm uuid.UUID
			err := asApp(t, f.tenant, f.move(&gm, po, c.mt, of, c.reason, c.quantity))
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("a 123 reversing a return", func(t *testing.T) {
		f, po, _ := scene(t)
		var ret, rev uuid.UUID
		mustApp(t, f.tenant, f.move(&ret, po, "122", nil, &reason, "-4"))
		failsWith(t, asApp(t, f.tenant, f.move(&rev, po, "123", &ret, nil, "-4")), "moves its quantity the other way")
		mustApp(t, f.tenant, f.move(&rev, po, "123", &ret, nil, "4"))
		failsWith(t, asApp(t, f.tenant, f.move(&rev, po, "102", &ret, nil, "-4")), "does not reverse that movement")
	})
}

// move inserts a goods movement of type mt on item 1 of po, reversing of
// when set, and sets *id to it.
func (f fixture) move(id *uuid.UUID, po uuid.UUID, mt string, of *uuid.UUID, reason *string, quantity string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		*id = uuid.Must(uuid.NewV7())
		if err := exec(tx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date,
			posting_date, reversal_of_id, reason, posted_by) VALUES ($1,$2,$3,$4,$5,'2026-08-05','2026-08-05',$6,$7,$8)`,
			f.tenant, *id, id.String()[24:], f.entity, mt, of, reason, f.actor); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO procurement.goods_movement_item VALUES ($1,$2,1,$3,1,$4,'EA')", f.tenant, *id, po, quantity)
	}
}
