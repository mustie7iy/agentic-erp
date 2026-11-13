package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Requests for quotation (T82a, migration 000285): what the database keeps
// of an RFQ, its items and the suppliers invited (ADR-0069 §1-§3).

const (
	rfqSQL = `INSERT INTO procurement.rfq (tenant_id, id, document_number, legal_entity_id, currency, due_date, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, '2026-07-31', $6, $7, $7)`
	rfqItemSQL = `INSERT INTO procurement.rfq_item (tenant_id, rfq_id, item_no, material_key, description, quantity, uom_code, account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	rfqSupplierSQL = `INSERT INTO procurement.rfq_supplier (tenant_id, rfq_id, supplier_id, invite_no) VALUES ($1, $2, $3, $4)`
)

// rfqFixture is a tenant with a draft RFQ and a published one of its legal
// entity, each with item 1 (CHAIR) and the fixture's supplier invited
// first.
type rfqFixture struct {
	fixture
	draft, published uuid.UUID
}

func newRFQs(t *testing.T) rfqFixture {
	t.Helper()
	f := rfqFixture{fixture: newFixture(t), draft: id(), published: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{rfqSQL, []any{f.tenant, f.draft, "RQ1", f.entity, "EUR", "draft", f.actor}},
			{rfqItemSQL, []any{f.tenant, f.draft, 1, "CHAIR", "Chairs", "10", "EA", f.expense}},
			{rfqSupplierSQL, []any{f.tenant, f.draft, f.supplier, 1}},
			{rfqSQL, []any{f.tenant, f.published, "RQ2", f.entity, "EUR", "draft", f.actor}},
			{rfqItemSQL, []any{f.tenant, f.published, 1, "CHAIR", "Chairs", "10", "EA", f.expense}},
			{rfqSupplierSQL, []any{f.tenant, f.published, f.supplier, 1}},
			{`UPDATE procurement.rfq SET status = 'published', version = version + 1 WHERE id = $1`, []any{f.published}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f rfqFixture) try(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
}

func TestRFQPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'procurement.rfq.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "procurement.rfq.award procurement.rfq.manage procurement.rfq.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

func TestRFQRules(t *testing.T) {
	need(t)
	f := newRFQs(t)
	const setRFQ = `UPDATE procurement.rfq SET version = version + 1, `
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"an RFQ born published":       {rfqSQL, []any{f.tenant, id(), "RQ9", f.entity, "EUR", "published", f.actor}, "starts as a draft"},
		"an unknown status":           {setRFQ + `status = 'closed' WHERE id = $1`, []any{f.draft}, "rfq_status_check"},
		"an unknown currency":         {rfqSQL, []any{f.tenant, id(), "RQ9", f.entity, "XXA", "draft", f.actor}, "rfq_currency_fkey"},
		"a number twice in a company": {rfqSQL, []any{f.tenant, id(), "RQ1", f.entity, "EUR", "draft", f.actor}, "rfq_number_once"},
		"a publication taken back":    {setRFQ + `status = 'draft' WHERE id = $1`, []any{f.published}, "stays published"},
		"an RFQ moved to another company": {setRFQ + `legal_entity_id = $2 WHERE id = $1`, []any{f.draft, id()},
			"keeps its company, number, currency and due date"},
		"an RFQ renumbered":    {setRFQ + `document_number = 'RQ7' WHERE id = $1`, []any{f.draft}, "keeps its company, number, currency and due date"},
		"another currency":     {setRFQ + `currency = 'USD' WHERE id = $1`, []any{f.draft}, "keeps its company, number, currency and due date"},
		"another due date":     {setRFQ + `due_date = '2026-08-31' WHERE id = $1`, []any{f.published}, "keeps its company, number, currency and due date"},
		"an RFQ deleted":       {`DELETE FROM procurement.rfq WHERE id = $1`, []any{f.draft}, "permission denied"},
		"item number zero":     {rfqItemSQL, []any{f.tenant, f.draft, 0, "DESK", "Desks", "1", "EA", f.expense}, "rfq_item_item_no_check"},
		"an item number twice": {rfqItemSQL, []any{f.tenant, f.draft, 1, "DESK", "Desks", "1", "EA", f.expense}, "rfq_item_pkey"},
		"a material key twice": {rfqItemSQL, []any{f.tenant, f.draft, 2, "CHAIR", "More chairs", "1", "EA", f.expense}, "rfq_item_material_key"},
		"an item without a material key": {rfqItemSQL, []any{f.tenant, f.draft, 2, "", "Desks", "1", "EA", f.expense},
			"rfq_item_material_key_check"},
		"an item without a description": {rfqItemSQL, []any{f.tenant, f.draft, 2, "DESK", " ", "1", "EA", f.expense}, "rfq_item_description_check"},
		"a quantity of zero":            {rfqItemSQL, []any{f.tenant, f.draft, 2, "DESK", "Desks", "0", "EA", f.expense}, "rfq_item_quantity_check"},
		"an unknown unit":               {rfqItemSQL, []any{f.tenant, f.draft, 2, "DESK", "Desks", "1", "NOUNIT", f.expense}, "rfq_item_uom_code_fkey"},
		"an item of no RFQ":             {rfqItemSQL, []any{f.tenant, id(), 1, "DESK", "Desks", "1", "EA", f.expense}, "is not a draft"},
		"an item added to a published RFQ": {rfqItemSQL, []any{f.tenant, f.published, 2, "DESK", "Desks", "1", "EA", f.expense},
			"is not a draft"},
		"an item changed": {`UPDATE procurement.rfq_item SET quantity = 5 WHERE rfq_id = $1`, []any{f.draft}, "permission denied"},
		"an item deleted": {`DELETE FROM procurement.rfq_item WHERE rfq_id = $1`, []any{f.draft}, "permission denied"},
		"a supplier invited twice": {rfqSupplierSQL, []any{f.tenant, f.draft, f.supplier, 2},
			"rfq_supplier_once"},
		"an invitation's place twice":       {rfqSupplierSQL, []any{f.tenant, f.draft, f.otherSupplier, 1}, "rfq_supplier_invite_no"},
		"an invitation's place zero":        {rfqSupplierSQL, []any{f.tenant, f.draft, f.otherSupplier, 0}, "rfq_supplier_invite_no_check"},
		"a supplier invited once published": {rfqSupplierSQL, []any{f.tenant, f.published, f.otherSupplier, 2}, "is not a draft"},
		"an invitation changed":             {`UPDATE procurement.rfq_supplier SET invite_no = 3 WHERE rfq_id = $1`, []any{f.draft}, "permission denied"},
		"an invitation withdrawn":           {`DELETE FROM procurement.rfq_supplier WHERE rfq_id = $1`, []any{f.draft}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("a second item and supplier of a draft", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, rfqItemSQL, f.tenant, f.draft, 2, "DESK", "Desks", "2.5", "EA", f.expense); err != nil {
				return err
			}
			return exec(tx, rfqSupplierSQL, f.tenant, f.draft, f.otherSupplier, 2)
		})
	})
	t.Run("the same number in another company", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, rfqSQL, f.tenant, id(), "RQ1", id(), "EUR", "draft", f.actor) })
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		for _, table := range []string{"rfq", "rfq_item", "rfq_supplier"} {
			var n int
			mustApp(t, id(), func(tx pgx.Tx) error {
				return tx.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{"procurement", table}.Sanitize()).Scan(&n)
			})
			if n != 0 {
				t.Errorf("rows of procurement.%s another tenant sees: got %d, want 0", table, n)
			}
		}
	})
}
