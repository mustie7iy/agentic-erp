package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Supplier quotations (T82b, migration 000286): what the database keeps
// of a quotation and its prices (ADR-0069 §7).

const (
	quotationSQL = `INSERT INTO procurement.supplier_quotation (tenant_id, id, rfq_id, supplier_id, supplier_reference, valid_until, status,
		created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '2026-12-31', $6, $7, $7)`
	quotationItemSQL = `INSERT INTO procurement.supplier_quotation_item (tenant_id, supplier_quotation_id, rfq_id, rfq_item_no, unit_price, price_unit)
		VALUES ($1, $2, $3, $4, $5, $6)`
)

// quotationFixture is newRFQs with a second published RFQ (other) and a
// quotation of the fixture's supplier for the published one, pricing its
// item 1.
type quotationFixture struct {
	rfqFixture
	other, quotation uuid.UUID
}

func newQuotations(t *testing.T) quotationFixture {
	t.Helper()
	f := quotationFixture{rfqFixture: newRFQs(t), other: id(), quotation: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{rfqSQL, []any{f.tenant, f.other, "RQ3", f.entity, "EUR", "draft", f.actor}},
			{rfqItemSQL, []any{f.tenant, f.other, 1, "CHAIR", "Chairs", "10", "EA", f.expense}},
			{rfqItemSQL, []any{f.tenant, f.other, 2, "DESK", "Desks", "2", "EA", f.expense}},
			{rfqSupplierSQL, []any{f.tenant, f.other, f.supplier, 1}},
			{rfqSupplierSQL, []any{f.tenant, f.other, f.otherSupplier, 2}},
			{`UPDATE procurement.rfq SET status = 'published', version = version + 1 WHERE id = $1`, []any{f.other}},
			{quotationSQL, []any{f.tenant, f.quotation, f.published, f.supplier, "Q-1", "recorded", f.actor}},
			{quotationItemSQL, []any{f.tenant, f.quotation, f.published, 1, 41250, 1}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func TestSupplierQuotationRules(t *testing.T) {
	need(t)
	f := newQuotations(t)
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a second quotation of the supplier": {quotationSQL, []any{f.tenant, id(), f.published, f.supplier, "Q-2", "recorded", f.actor}, "supplier_quotation_once"},
		"a supplier not invited":             {quotationSQL, []any{f.tenant, id(), f.published, f.otherSupplier, "Q-2", "recorded", f.actor}, "supplier_quotation_invited"},
		"a draft RFQ":                        {quotationSQL, []any{f.tenant, id(), f.draft, f.supplier, "Q-2", "recorded", f.actor}, "is not published"},
		"an unknown status":                  {quotationSQL, []any{f.tenant, id(), f.other, f.supplier, "Q-2", "awarded", f.actor}, "supplier_quotation_status_check"},
		"a blank reference":                  {quotationSQL, []any{f.tenant, id(), f.other, f.supplier, " ", "recorded", f.actor}, "supplier_quotation_reference_check"},
		"a quotation changed": {`UPDATE procurement.supplier_quotation SET supplier_reference = 'Q-9', version = version + 1 WHERE id = $1`,
			[]any{f.quotation}, "permission denied"},
		"a quotation deleted":      {`DELETE FROM procurement.supplier_quotation WHERE id = $1`, []any{f.quotation}, "permission denied"},
		"an RFQ item priced twice": {quotationItemSQL, []any{f.tenant, f.quotation, f.published, 1, 100, 1}, "supplier_quotation_item_pkey"},
		"an item the RFQ lacks":    {quotationItemSQL, []any{f.tenant, f.quotation, f.published, 2, 100, 1}, "supplier_quotation_item_rfq_item_fkey"},
		"an item of another RFQ":   {quotationItemSQL, []any{f.tenant, f.quotation, f.other, 2, 100, 1}, "supplier_quotation_item_quotation_fkey"},
		"a price of zero":          {quotationItemSQL, []any{f.tenant, f.quotation, f.published, 1, 0, 1}, "supplier_quotation_item_unit_price_check"},
		"a price unit of zero":     {quotationItemSQL, []any{f.tenant, f.quotation, f.published, 1, 100, 0}, "supplier_quotation_item_price_unit_check"},
		"a price of no quotation":  {quotationItemSQL, []any{f.tenant, id(), f.published, 1, 100, 1}, "supplier_quotation_item_quotation_fkey"},
		"a price changed":          {`UPDATE procurement.supplier_quotation_item SET unit_price = 1 WHERE supplier_quotation_id = $1`, []any{f.quotation}, "permission denied"},
		"a price deleted":          {`DELETE FROM procurement.supplier_quotation_item WHERE supplier_quotation_id = $1`, []any{f.quotation}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("each invited supplier quotes once, items left out", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			for _, supplier := range []uuid.UUID{f.supplier, f.otherSupplier} {
				q := id()
				if err := exec(tx, quotationSQL, f.tenant, q, f.other, supplier, "Q-3", "recorded", f.actor); err != nil {
					return err
				}
				if err := exec(tx, quotationItemSQL, f.tenant, q, f.other, 2, 5000, 3); err != nil {
					return err
				}
			}
			return nil
		})
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		for _, table := range []string{"supplier_quotation", "supplier_quotation_item"} {
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
