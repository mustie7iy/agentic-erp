package fixture

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Sales documents written straight into the sales context's tables, for
// the tests of what reads them (the analytical store's sales lines,
// T91a): the sales context's own tests create theirs through the API.

// SalesItem is an item SalesOrder writes: its item key ("": none) and its
// net amount in minor units of the order's currency.
type SalesItem struct {
	Key string
	Net int64
}

// Customer writes an active business partner in the customer role alone
// and returns its id.
func (e Env) Customer(tb testing.TB, number, name string) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'DE', 'active', $5, $5)`, e.Tenant.ID, id, number, name, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`,
		e.Tenant.ID, id, e.Tenant.Human.ID)
	return id
}

// SalesOrder writes a sales order of the legal entity to the customer,
// dated date, in status (draft or confirmed), with the items numbered
// from 1, each one unit at its net amount on its company's revenue
// account 400000 and
// delivered in full, as createSalesOrder, confirmSalesOrder and
// confirmSalesDelivery would leave it, and returns its id.
func (e Env) SalesOrder(tb testing.TB, legalEntity, customer uuid.UUID, currency, date, status string, items ...SalesItem) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	var net int64
	for _, it := range items {
		net += it.Net
	}
	pgtest.Exec(tb, `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
			net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'NT30', $8, $9, $10, $10)`,
		e.Tenant.ID, id, "SO"+id.String()[24:], legalEntity, customer, date, currency, net, status, e.Tenant.Human.ID)
	for i, it := range items {
		pgtest.Exec(tb, `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, item_key, description, quantity, uom_code, unit_price, net_amount,
				account_id, delivered_quantity)
			SELECT $1, $2, $3, nullif($4, ''), 'Item', 1, 'EA', $5, $5, a.id, 1
			FROM tenancy.legal_entity le
			JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
			JOIN ledger.gl_account a ON a.chart_id = c.id AND a.number = '400000'
			WHERE le.tenant_id = $1 AND le.id = $6`,
			e.Tenant.ID, id, i+1, it.Key, it.Net, legalEntity)
	}
	return id
}

// asTenant scopes a fixture statement to the tenant $1, as a request's
// transaction is: the billing tables' triggers move the order and its
// items as their owner, to whom row-level security applies (000293).
const asTenant = `(SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`

// BillingDocument writes a billing document of the confirmed sales order,
// dated date, billing every item of the order in full, as
// createBillingDocument would leave it, and returns its id. With posted
// it is posted as a customer invoice (postBilling).
func (e Env) BillingDocument(tb testing.TB, order uuid.UUID, date string, posted bool) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date, currency,
			net_amount, status, created_by, updated_by)
		SELECT o.tenant_id, $2, $3, o.legal_entity_id, o.customer_id, o.id, $4, o.currency, o.net_amount, 'created', $5, $5
		FROM sales.sales_order o, `+asTenant+` WHERE o.tenant_id = $1 AND o.id = $6`,
		e.Tenant.ID, id, "BD"+id.String()[24:], date, e.Tenant.Human.ID, order)
	pgtest.Exec(tb, `INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount)
		SELECT i.tenant_id, $2, i.item_no, i.sales_order_id, i.item_no, i.quantity, i.net_amount
		FROM sales.sales_order_item i, `+asTenant+` WHERE i.tenant_id = $1 AND i.sales_order_id = $3 ORDER BY i.item_no`, e.Tenant.ID, id, order)
	if posted {
		e.postBilling(tb, &id)
	}
	return id
}

// postBilling posts the tenant's billing document id, or every one of
// them (nil), as a customer invoice.
//
// Posting arrives with T79b, whose migration admits the status and has a
// posted document name its customer invoice, posting date and entry. On a
// base without it the test's database is given the status alone: the
// check that admits only created is dropped, as that migration replaces
// it. The branch without those columns goes once T79b is merged.
func (e Env) postBilling(tb testing.TB, id *uuid.UUID) {
	tb.Helper()
	var names bool
	pgtest.Scan(tb, `SELECT EXISTS (SELECT FROM information_schema.columns
		WHERE table_schema = 'sales' AND table_name = 'billing_document' AND column_name = 'customer_invoice_id')`, nil, &names)
	if names {
		pgtest.Exec(tb, `UPDATE sales.billing_document SET status = 'posted', posting_date = billing_date, customer_invoice_id = gen_random_uuid(),
				customer_invoice_number = document_number, journal_entry_year = extract(year FROM billing_date), journal_entry_id = gen_random_uuid(),
				version = version + 1
			WHERE tenant_id = $1 AND ($2::uuid IS NULL OR id = $2)`, e.Tenant.ID, id)
		return
	}
	pgtest.Exec(tb, `ALTER TABLE sales.billing_document DROP CONSTRAINT IF EXISTS billing_document_status_check`)
	pgtest.Exec(tb, `UPDATE sales.billing_document SET status = 'posted', version = version + 1 WHERE tenant_id = $1 AND ($2::uuid IS NULL OR id = $2)`,
		e.Tenant.ID, id)
}

// SalesVolume writes a year of sales documents of the legal entity for a
// budget test: customers customers and orders confirmed sales orders in
// EUR, spread over the days of 2026 and the customers, each of two items
// (1.00 and 2.00, of fifty item keys) and billed in full five days later
// by a posted billing document. The rows are written in bulk without the
// tables' triggers, as the documents' own operations would leave them.
func (e Env) SalesVolume(tb testing.TB, legalEntity uuid.UUID, customers, orders int) {
	tb.Helper()
	ctx := context.Background()
	admin := pgtest.Admin(tb)
	exec := func(sql string, args ...any) {
		tb.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			tb.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`SET session_replication_role = replica`)
	exec(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		SELECT $1, md5('c' || n)::uuid, 'BP6' || lpad(n::text, 7, '0'), 'organization', 'Customer ' || n, 'DE', 'active', $2, $2
		FROM generate_series(1, $3::int) n`, e.Tenant.ID, e.Tenant.Human.ID, customers)
	exec(`INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by)
		SELECT $1, md5('c' || n)::uuid, 'customer', $2 FROM generate_series(1, $3::int) n`, e.Tenant.ID, e.Tenant.Human.ID, customers)
	exec(`INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
			net_amount, status, created_by, updated_by)
		SELECT $1, md5('o' || n)::uuid, 'SO' || n, $2, md5('c' || (n % $3::int + 1))::uuid, DATE '2026-01-01' + (n % 360), 'EUR', 'NT30', 300, 'confirmed', $4, $4
		FROM generate_series(1, $5::int) n`, e.Tenant.ID, legalEntity, customers, e.Tenant.Human.ID, orders)
	exec(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, item_key, description, quantity, uom_code, unit_price, net_amount,
			account_id, delivered_quantity, billed_quantity)
		SELECT $1, md5('o' || n)::uuid, i, 'ITEM-' || (n % 50), 'Item', 1, 'EA', i * 100, i * 100, a.id, 1, 1
		FROM generate_series(1, $3::int) n, generate_series(1, 2) i, tenancy.legal_entity le
		JOIN ledger.chart_of_accounts c ON c.tenant_id = le.tenant_id AND c.code = le.code
		JOIN ledger.gl_account a ON a.chart_id = c.id AND a.number = '400000'
		WHERE le.tenant_id = $1 AND le.id = $2`, e.Tenant.ID, legalEntity, orders)
	exec(`INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date, currency,
			net_amount, status, created_by, updated_by)
		SELECT o.tenant_id, md5('b' || o.id)::uuid, 'BD' || substr(o.document_number, 3), o.legal_entity_id, o.customer_id, o.id, o.order_date + 5, o.currency,
			o.net_amount, 'created', o.created_by, o.created_by
		FROM sales.sales_order o WHERE o.tenant_id = $1`, e.Tenant.ID)
	exec(`INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount)
		SELECT i.tenant_id, md5('b' || i.sales_order_id)::uuid, i.item_no, i.sales_order_id, i.item_no, i.quantity, i.net_amount
		FROM sales.sales_order_item i WHERE i.tenant_id = $1`, e.Tenant.ID)
	exec(`RESET session_replication_role`)
	e.postBilling(tb, nil)
	exec(`ANALYZE sales.sales_order, sales.sales_order_item, sales.billing_document, sales.billing_document_item, partner.business_partner`)
}
