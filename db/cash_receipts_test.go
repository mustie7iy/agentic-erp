package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// receiptRow is a snapshot line of a receipt from a customer (T53l1): a
// document-level inflow of a journal entry under REC, with the columns of
// set changed.
func receiptRow(f fixture, sid uuid.UUID, line int, set map[string]any) stmt {
	row := map[string]any{"value_date": "2026-04-21", "bucket_end": nil, "customer_id": id(), "supplier_id": nil, "document_kind": "journal_entry",
		"source": "receivable"}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, bucket_end, currency, source, certainty,
			liquidity_item, amount, flow_count, supplier_id, customer_id, document_kind, document_id, document_number)
		VALUES ($1, $2, $3, 1, $4, $5, 'EUR', $6, 'expected', 'REC', 119000, 1, $7, $8, $9, CASE WHEN $9::text IS NULL THEN NULL ELSE gen_random_uuid() END,
			NULL)`,
		f.tenant, sid, line, row["value_date"], row["bucket_end"], row["source"], row["supplier_id"], row["customer_id"], row["document_kind"])
}

// TestCashReceiptSnapshotRules: what 000490 adds to a snapshot's lines
// (T53l1): a line of a receipt names its customer, a sales order's line
// names the order; a line names a supplier or a customer, never both, and
// a bucket's sum names no customer.
func TestCashReceiptSnapshotRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name string
		row  func(f fixture) map[string]any
		want string // empty: allowed
	}{
		{"a customer item's receipt", func(fixture) map[string]any { return nil }, ""},
		{"a sales order's receipt", func(fixture) map[string]any {
			return map[string]any{"source": "sales_order", "document_kind": "sales_order"}
		}, ""},
		{"a receipt held back", func(fixture) map[string]any { return map[string]any{"source": "receivable_blocked"} }, ""},
		{"a customer and a supplier", func(f fixture) map[string]any { return map[string]any{"supplier_id": f.supplier} }, "snapshot_flow_customer"},
		{"a bucket's sum naming a customer", func(fixture) map[string]any {
			return map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "document_kind": nil}
		}, "snapshot_flow_customer"},
		{"a bucket's sum of receipts", func(fixture) map[string]any {
			return map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "document_kind": nil, "customer_id": nil}
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, sid := newFixture(t), id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return all(tx, memoItems(f), snapshotRow(f, sid, nil), receiptRow(f, sid, 1, c.row(f)))
			})
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

// TestCashReceiptsValidated: 000491 validates the checks 000490 added
// and widened NOT VALID.
func TestCashReceiptsValidated(t *testing.T) {
	need(t)
	for _, name := range []string{"snapshot_flow_document_kind_check", "snapshot_flow_customer"} {
		var validated bool
		if err := testDB.QueryRow(context.Background(), `SELECT convalidated FROM pg_constraint WHERE conname = $1
				AND conrelid = 'cash.snapshot_flow'::regclass`, name).Scan(&validated); err != nil {
			t.Fatal(err)
		}
		if !validated {
			t.Errorf("%s: got not validated, want 000491 to validate it", name)
		}
	}
}

// TestCashReceiptsDownRefused (T53l1, migration 000490): the down
// refuses, rather than drops, a snapshot line of a receipt from a
// customer, of a source the previous version does not produce, or naming
// a sales order, which the previous version cannot hold (06 §6.4).
func TestCashReceiptsDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000490_cash_receipts").Down
	for name, set := range map[string]map[string]any{
		"a line naming a customer":    nil,
		"a line of a receipt":         {"customer_id": nil},
		"a line naming a sales order": {"customer_id": nil, "source": "payable", "document_kind": "sales_order"},
	} {
		t.Run(name, func(t *testing.T) {
			f, sid := newFixture(t), id()
			downRefused(t, down, f.tenant, []stmt{memoItems(f), snapshotRow(f, sid, nil), receiptRow(f, sid, 1, set)},
				"snapshot lines of receipts from customers exist")
		})
	}
}
