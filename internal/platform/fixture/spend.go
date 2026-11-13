package fixture

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Purchasing documents written straight into the procurement, payables
// and ledger contexts' tables, for the tests of what reads them (the
// analytical store's spend lines, T91b): those contexts' own tests create
// theirs through the API.

// CostCenter writes an active cost center of the legal entity and returns
// its id. It is written without the table's triggers, as the bulk rows
// are: an active center otherwise needs its place in the standard
// hierarchy, which the cost center's own operations give it.
func (e Env) CostCenter(tb testing.TB, legalEntity uuid.UUID, code string) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	admin := pgtest.Admin(tb)
	_, err := admin.Exec(ctx, `SET session_replication_role = replica`)
	if err == nil {
		_, err = admin.Exec(ctx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 'Cost center ' || $4, 'J. Doe', 'active', $5, $5)`, e.Tenant.ID, id, legalEntity, code, e.Tenant.Human.ID)
	}
	if err == nil {
		_, err = admin.Exec(ctx, `RESET session_replication_role`)
	}
	if err != nil {
		tb.Fatalf("cost center %s: %v", code, err)
	}
	return id
}

// Contract writes an active contract of the legal entity with the
// supplier, numbered number, and returns its id.
func (e Env) Contract(tb testing.TB, legalEntity, supplier uuid.UUID, currency, number string) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from, valid_to,
			target_value, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, '2026-01-01', '2026-12-31', 100000000, 'active', $7, $7)`,
		e.Tenant.ID, id, number, legalEntity, supplier, currency, e.Tenant.Human.ID)
	return id
}

// ContractOrder writes a released purchase order of the legal entity with
// the supplier, made on the contract (nil: on none), with one item, number
// 10, of one unit on the account and the cost center (nil: none), and
// returns its id.
func (e Env) ContractOrder(tb testing.TB, legalEntity, supplier uuid.UUID, contract *uuid.UUID, currency, date string, account uuid.UUID, costCenter *uuid.UUID) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
			payment_term_code, contract_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'NT30', $8, 'draft', $9, $9)`,
		e.Tenant.ID, id, "PO"+id.String()[24:], legalEntity, supplier, currency, date, contract, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id,
			account_assignment, cost_center_id)
		VALUES ($1, $2, 10, 'Item', 1, 'EA', 100, $3, CASE WHEN $4::uuid IS NULL THEN 'none' ELSE 'cost_center' END, $4)`,
		e.Tenant.ID, id, account, costCenter)
	// Released once it has its item: a released order's items do not change.
	pgtest.Exec(tb, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, id)
	return id
}

// InvoiceItem is an item SupplierDocument writes: its net amount in minor
// units and either the purchase order whose item 10 it invoices or its
// own account and cost center (nil: none).
type InvoiceItem struct {
	Net        int64
	Order      *uuid.UUID
	Account    *uuid.UUID
	CostCenter *uuid.UUID
}

// SupplierDocument writes a supplier document of the legal entity and the
// supplier of the kind (invoice, credit_memo, subsequent_debit,
// subsequent_credit) in status, dated and, when posted or reversed,
// posted on date, with the items numbered from 1, as entering and posting
// it would leave it but for its journal entry, which is not written, and
// returns its id.
func (e Env) SupplierDocument(tb testing.TB, legalEntity, supplier uuid.UUID, kind, status, currency, date string, items ...InvoiceItem) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	var net int64
	for _, it := range items {
		net += it.Net
	}
	// The document is on its first item's order, as one entered on an
	// order is; a subsequent debit or credit must be on one.
	var order *uuid.UUID
	if len(items) > 0 {
		order = items[0].Order
	}
	pgtest.Exec(tb, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
			posting_date, currency, gross_amount, tax_amount, purchase_order_id, kind, status, source, journal_entry_year, journal_entry_id,
			reversal_entry_year, reversal_entry_id, created_by, updated_by)
		SELECT $1, $2, $3, $4, $5, $3, $6::date, CASE WHEN p.posted THEN $6::date END, $7, $8, 0, $12, $9, $10, 'manual',
			CASE WHEN p.posted THEN 2026 END, CASE WHEN p.posted THEN gen_random_uuid() END,
			CASE WHEN $10 = 'reversed' THEN 2026 END, CASE WHEN $10 = 'reversed' THEN gen_random_uuid() END, $11, $11
		FROM (SELECT $10 IN ('posted', 'reversed') AS posted) p`,
		e.Tenant.ID, id, "SI"+id.String()[24:], legalEntity, supplier, date, currency, net, kind, status, e.Tenant.Human.ID, order)
	// A subsequent debit or credit revalues its order items without quantity.
	quantity := 1
	if strings.HasPrefix(kind, "subsequent_") {
		quantity = 0
	}
	for i, it := range items {
		var poItem *int
		if it.Order != nil {
			poItem = new(int)
			*poItem = 10
		}
		pgtest.Exec(tb, `INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no, description,
				quantity, uom_code, amount, account_id, cost_center_id)
			VALUES ($1, $2, $3, $4, $5, 'Item', $9, 'EA', $6, $7, $8)`, e.Tenant.ID, id, i+1, it.Order, poItem, it.Net, it.Account, it.CostCenter, quantity)
	}
	return id
}

// SpendVolume writes a year of posted supplier invoices of the legal
// entity for a budget test: suppliers suppliers, fifty contracts, twenty
// cost centers, a tenth as many purchase orders as invoices, every other
// one made on a contract, and invoices posted invoices in EUR spread over
// the days of 2026 and the suppliers, each of two items: 1.00 on an
// order's item and 2.00 on account 520000 and a cost center. The rows are
// written in bulk without the tables' triggers, as the documents' own
// operations would leave them but for their journal entries.
func (e Env) SpendVolume(tb testing.TB, legalEntity uuid.UUID, suppliers, invoices int) {
	tb.Helper()
	ctx := context.Background()
	admin := pgtest.Admin(tb)
	exec := func(sql string, args ...any) {
		tb.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			tb.Fatalf("%v\n%s", err, sql)
		}
	}
	office, paper := e.accountOf(tb, legalEntity, "520000"), e.accountOf(tb, legalEntity, "610000")
	exec(`SET session_replication_role = replica`)
	exec(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		SELECT $1, md5('s' || n)::uuid, 'BP5' || lpad(n::text, 7, '0'), 'organization', 'Supplier ' || n, 'DE', 'active', $2, $2
		FROM generate_series(1, $3::int) n`, e.Tenant.ID, e.Tenant.Human.ID, suppliers)
	exec(`INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by)
		SELECT $1, md5('s' || n)::uuid, 'supplier', $2 FROM generate_series(1, $3::int) n`, e.Tenant.ID, e.Tenant.Human.ID, suppliers)
	exec(`INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
		SELECT $1, md5('k' || n)::uuid, $2, 'V' || n, 'Cost center ' || n, 'J. Doe', 'active', $3, $3 FROM generate_series(1, 20) n`,
		e.Tenant.ID, legalEntity, e.Tenant.Human.ID)
	exec(`INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from, valid_to, target_value,
			status, created_by, updated_by)
		SELECT $1, md5('t' || n)::uuid, 'CTV' || n, $2, md5('s' || n)::uuid, 'EUR', '2026-01-01', '2026-12-31', 100000000, 'active', $3, $3
		FROM generate_series(1, 50) n`, e.Tenant.ID, legalEntity, e.Tenant.Human.ID)
	exec(`INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date, payment_term_code,
			contract_id, status, created_by, updated_by)
		SELECT $1, md5('p' || n)::uuid, 'POV' || n, $2, md5('s' || (n % $3::int + 1))::uuid, 'EUR', '2026-01-01', 'NT30',
			CASE WHEN n % 2 = 0 THEN md5('t' || (n % 50 + 1))::uuid END, 'released', $4, $4
		FROM generate_series(1, $5::int) n`, e.Tenant.ID, legalEntity, suppliers, e.Tenant.Human.ID, invoices/10)
	exec(`INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id,
			account_assignment, cost_center_id)
		SELECT $1, md5('p' || n)::uuid, 10, 'Item', 1, 'EA', 100, $2, 'cost_center', md5('k' || (n % 20 + 1))::uuid
		FROM generate_series(1, $3::int) n`, e.Tenant.ID, paper, invoices/10)
	exec(`INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
			posting_date, currency, gross_amount, tax_amount, purchase_order_id, status, source, journal_entry_year, journal_entry_id, created_by, updated_by)
		SELECT $1, md5('i' || n)::uuid, 'SIV' || n, $2, md5('s' || (n % $3::int + 1))::uuid, 'SIV' || n, DATE '2026-01-01' + (n % 360),
			DATE '2026-01-01' + (n % 360), 'EUR', 300, 0, md5('p' || (n % $5::int + 1))::uuid, 'posted', 'manual', 2026, md5('j' || n)::uuid, $4, $4
		FROM generate_series(1, $6::int) n`, e.Tenant.ID, legalEntity, suppliers, e.Tenant.Human.ID, invoices/10, invoices)
	exec(`INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no, description, quantity,
			uom_code, amount, account_id, cost_center_id)
		SELECT $1, md5('i' || n)::uuid, i, CASE WHEN i = 1 THEN md5('p' || (n % $4::int + 1))::uuid END, CASE WHEN i = 1 THEN 10 END, 'Item', 1, 'EA', i * 100,
			CASE WHEN i = 2 THEN $2::uuid END, CASE WHEN i = 2 THEN md5('k' || (n % 20 + 1))::uuid END
		FROM generate_series(1, $3::int) n, generate_series(1, 2) i`, e.Tenant.ID, office, invoices, invoices/10)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE payables.supplier_invoice, payables.supplier_invoice_item, procurement.purchase_order, procurement.purchase_order_item,
		procurement.contract, ledger.cost_center, partner.business_partner`)
}

// accountOf is the id of an account of the legal entity's chart.
func (e Env) accountOf(tb testing.TB, legalEntity uuid.UUID, number string) uuid.UUID {
	tb.Helper()
	var id uuid.UUID
	pgtest.Scan(tb, `SELECT a.id FROM tenancy.legal_entity le
		JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
		JOIN ledger.gl_account a ON a.chart_id = c.id AND a.number = $3
		WHERE le.tenant_id = $1 AND le.id = $2`, []any{e.Tenant.ID, legalEntity, number}, &id)
	return id
}
