package processmining_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// flows writes, without the tables' triggers, n purchase-to-pay flows of
// the company numbered from first: each a requisition, an order of it
// dated day plus (number mod spread) days, a goods receipt, a posted
// invoice on the order and a released run that paid it, the documents'
// ids md5 of a letter and the number, with their audit events an hour
// apart per flow from 2026-03-01 08:00 UTC, a minute apart within one,
// the payment by the system and an update, which is no activity, among
// them. The events' sequence numbers start at seq, far above the
// tenant's own.
func flows(t *testing.T, e fixture.Env, le uuid.UUID, first, n int, day string, spread, seq int) {
	t.Helper()
	admin := pgtest.Admin(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	ten, who, last := e.Tenant.ID, e.Tenant.Human.ID, first+n-1
	exec(`SET session_replication_role = replica`)
	defer exec(`RESET session_replication_role`)
	exec(`INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, status, created_by, updated_by)
		SELECT $1, md5('r' || n)::uuid, 'PR' || n, $2, $3, 'EUR', 'ordered', $3, $3 FROM generate_series($4::int, $5::int) n`, ten, le, who, first, last)
	exec(`INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date, payment_term_code,
			requisition_id, status, created_by, updated_by)
		SELECT $1, md5('o' || n)::uuid, 'PO' || n, $2, $3, 'EUR', $6::date + n % $7::int, 'NT30', md5('r' || n)::uuid, 'released', $3, $3
		FROM generate_series($4::int, $5::int) n`, ten, le, who, first, last, day, spread)
	exec(`INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date, posted_by)
		SELECT $1, md5('g' || n)::uuid, 'GR' || n, $2, '101', $6::date, $6::date, $3 FROM generate_series($4::int, $5::int) n`, ten, le, who, first, last, day)
	exec(`INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code)
		SELECT $1, md5('g' || n)::uuid, 1, md5('o' || n)::uuid, 1, 1, 'EA' FROM generate_series($2::int, $3::int) n`, ten, first, last)
	exec(`INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date, posting_date,
			currency, gross_amount, tax_amount, purchase_order_id, status, source, journal_entry_year, journal_entry_id, created_by, updated_by)
		SELECT $1, md5('i' || n)::uuid, 'SI' || n, $2, $3, 'REF' || n, $6::date, $6::date, 'EUR', 100, 0, md5('o' || n)::uuid, 'posted', 'manual',
			2026, md5('j' || n)::uuid, $3, $3 FROM generate_series($4::int, $5::int) n`, ten, le, who, first, last, day)
	exec(`INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status,
			proposed_by, decided_by, decided_at, created_by, updated_by)
		SELECT $1, md5('p' || n)::uuid, 'RUN' || n, $2, gen_random_uuid(), $6::date, $6::date, 'EUR', 100, 'released', $3, gen_random_uuid(), now(), $3, $3
		FROM generate_series($4::int, $5::int) n`, ten, le, who, first, last, day)
	exec(`INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by)
		SELECT $1::uuid, md5('y' || n)::uuid, md5('p' || n)::uuid, $2, gen_random_uuid(), 100, 'EUR', left(md5($1::uuid::text || n), 30), $2
		FROM generate_series($3::int, $4::int) n`, ten, who, first, last)
	exec(`INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		SELECT $1, md5('y' || n)::uuid, 2026, md5('j' || n)::uuid, 1, 100, $4::date FROM generate_series($2::int, $3::int) n`, ten, first, last, day)
	exec(`INSERT INTO audit.audit_event (tenant_id, occurred_at, seq, actor_id, actor_kind, action, object_type, object_id, outcome, hash)
		SELECT $1, timestamptz '2026-03-01 08:00Z' + a.k * interval '1 minute' + n * interval '1 hour', $3::bigint + n * 10 + a.k,
			CASE a.kind WHEN 'human' THEN $2 ELSE gen_random_uuid() END, a.kind, a.action, a.type, md5(a.doc || n)::uuid, 'success', sha256(n::text::bytea)
		FROM generate_series($4::int, $5::int) n, (VALUES
			(1, 'human', 'procurement.requisition.create', 'procurement.requisition', 'r'),
			(2, 'human', 'procurement.requisition.approved', 'procurement.requisition', 'r'),
			(3, 'agent', 'procurement.purchase_order.create', 'procurement.purchase_order', 'o'),
			(4, 'human', 'procurement.purchase_order.release', 'procurement.purchase_order', 'o'),
			(5, 'human', 'procurement.purchase_order.update', 'procurement.purchase_order', 'o'),
			(6, 'human', 'procurement.goods_receipt.post', 'procurement.goods_movement', 'g'),
			(7, 'agent', 'payables.supplier_invoice.create', 'payables.supplier_invoice', 'i'),
			(8, 'human', 'payables.supplier_invoice.post', 'payables.supplier_invoice', 'i'),
			(9, 'service', 'payments.payment_run.complete', 'payments.payment_run', 'p')) a(k, kind, action, type, doc)`, ten, who, seq, first, last)
	exec(`ANALYZE procurement.purchase_order, procurement.requisition, procurement.goods_movement, procurement.goods_movement_item,
		payables.supplier_invoice, payables.supplier_invoice_item, payments.payment_run, payments.payment, payments.payment_item, audit.audit_event`)
}

// read reads the log through the API as the client and returns its body
// as JSON.
func read(t *testing.T, c apitest.Client, query string) map[string]any {
	t.Helper()
	got, _ := c.Must(200, "GET", "/v1/process-event-logs?process=purchase_to_pay&"+query, "", "")
	return got
}

// activitiesOf are a case's events as "activity actor document" lines.
func activitiesOf(t *testing.T, c map[string]any) string {
	t.Helper()
	var out []string
	for _, ev := range apitest.List(t, c, "events") {
		ev := ev.(map[string]any)
		out = append(out, fmt.Sprint(ev["activity"], " ", ev["actor_kind"], " ", ev["document_number"]))
	}
	return strings.Join(out, ", ")
}

// TestEventLog: a case per order of the period with its requisition's,
// its own, its receipt's, its invoice's and its payment's activities in
// order, each with the kind of its actor and its document's number; an
// invoice on no order is a case of its own; an order of another period
// or company is not; actions that are no activity are left out (T103a
// acceptance).
func TestEventLog(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 2, "2026-03-02", 3, 1_000_000)
	flows(t, e, e.DE, 3, 1, "2026-05-02", 3, 1_000_100) // another period
	flows(t, e, e.US, 4, 1, "2026-03-02", 3, 1_000_200) // another company
	account := e.Account(t, "2000", "520000")
	alone := e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "parked", "EUR", "2026-03-05", fixture.InvoiceItem{Net: 100, Account: &account})
	got := read(t, e.Client, "legal_entity_id="+e.DE.String()+"&from=2026-03-01&to=2026-03-31")
	if s := fmt.Sprint(got["process"], " ", got["company_code"], " ", got["case_count"], " ", got["from"], " ", got["to"]); s != "purchase_to_pay 2000 3 2026-03-01 2026-03-31" {
		t.Errorf("the log: got %s", s)
	}
	cases := apitest.List(t, got, "cases")
	var kinds []string
	for _, c := range cases {
		c := c.(map[string]any)
		kinds = append(kinds, fmt.Sprint(c["case_kind"], " ", c["document_number"], " ", c["document_date"]))
	}
	if s := strings.Join(kinds, ", "); s != "purchase_order PO1 2026-03-03, purchase_order PO2 2026-03-04, supplier_invoice "+numberOf(t, alone)+" 2026-03-05" {
		t.Errorf("the cases: got %s", s)
	}
	if s := activitiesOf(t, cases[0].(map[string]any)); s != "Create requisition person PR1, Approve requisition person PR1, Create purchase order agent PO1, "+
		"Release purchase order person PO1, Receive goods person GR1, Enter invoice agent SI1, Post invoice person SI1, Pay invoice system RUN1" {
		t.Errorf("the first case's events: got %s", s)
	}
	if evs := apitest.List(t, cases[2].(map[string]any), "events"); len(evs) != 0 {
		t.Errorf("the invoice on no order: got %v, want no audited activity", evs)
	}
	if got["next_page_token"] != nil {
		t.Errorf("one page: got a next page token %v", got["next_page_token"])
	}
}

// numberOf is a supplier document's number.
func numberOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var n string
	pgtest.Scan(t, `SELECT document_number FROM payables.supplier_invoice WHERE id = $1`, []any{id}, &n)
	return n
}

// TestEventLogOrder: events of a case are by time and then by sequence
// number, whatever object they are about; a document on two orders and
// the run that paid it are in both cases.
func TestEventLogOrder(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 2, "2026-03-02", 3, 1_000_000)
	// The second flow's invoice is on the first order too, by an item; the
	// first order's release happened before its requisition's events, and
	// those two at the same instant.
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no, description, quantity,
		uom_code, amount) VALUES ($1, md5('i2')::uuid, 1, md5('o1')::uuid, 1, 'Item', 1, 'EA', 100)`, e.Tenant.ID)
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET occurred_at = timestamptz '2026-03-01 09:01Z' WHERE seq IN (1000011, 1000012);
		UPDATE audit.audit_event SET occurred_at = timestamptz '2026-03-01 09:00Z' WHERE seq = 1000014; END $$`)
	got := read(t, e.Client, "legal_entity_id="+e.DE.String()+"&from=2026-03-01&to=2026-03-31")
	first := apitest.List(t, got, "cases")[0].(map[string]any)
	if s := activitiesOf(t, first); s != "Release purchase order person PO1, Create requisition person PR1, Approve requisition person PR1, "+
		"Create purchase order agent PO1, Receive goods person GR1, Enter invoice agent SI1, Post invoice person SI1, Pay invoice system RUN1, "+
		"Enter invoice agent SI2, Post invoice person SI2, Pay invoice system RUN2" {
		t.Errorf("the first case: got %s", s)
	}
}

// TestEventLogPagesAndCSV: the log is paged by case, a token naming the
// next page; as CSV a page is a row per event under a header row, the
// token in the Next-Page-Token header.
func TestEventLogPagesAndCSV(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 3, "2026-03-02", 3, 1_000_000)
	q := "legal_entity_id=" + e.DE.String() + "&from=2026-03-01&to=2026-03-31&page_size=2"
	page := read(t, e.Client, q)
	next := read(t, e.Client, q+"&page_token="+url.QueryEscape(page["next_page_token"].(string)))
	var numbers []string
	for _, p := range []map[string]any{page, next} {
		for _, c := range apitest.List(t, p, "cases") {
			numbers = append(numbers, c.(map[string]any)["document_number"].(string))
		}
	}
	if s := strings.Join(numbers, " "); s != "PO3 PO1 PO2" || next["next_page_token"] != nil || page["case_count"] != float64(3) {
		t.Errorf("pages: got %s, a last token %v and %v cases, want PO3 PO1 PO2 by date, no token after, 3 cases", s, next["next_page_token"], page["case_count"])
	}
	r := e.API.Do(apitest.Req{Method: http.MethodGet, Path: "/v1/process-event-logs?process=purchase_to_pay&format=csv&" + q, Tenant: e.Tenant.ID, As: e.Tenant.Human})
	rows, err := csv.NewReader(r.Body).ReadAll()
	if err != nil || r.Code != 200 || r.Header().Get("Next-Page-Token") == "" {
		t.Fatalf("CSV: got %d %v with token %q, want 200 and a token", r.Code, err, r.Header().Get("Next-Page-Token"))
	}
	if len(rows) != 1+2*8 || strings.Join(rows[0], ",") != "case_id,case_kind,case_document_number,activity,occurred_at,actor_kind,object_type,object_id,object_document_number,seq" ||
		rows[1][2] != "PO3" || rows[1][3] != "Create requisition" || rows[16][2] != "PO1" || rows[16][3] != "Pay invoice" {
		t.Errorf("CSV rows: got %v", rows)
	}
}

// TestEventLogRefused: another process or format, a period that ends
// before it starts and a bad token are refused; another company's log,
// or one the caller cannot act in, is not found.
func TestEventLogRefused(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	base := "/v1/process-event-logs?legal_entity_id=" + e.DE.String() + "&from=2026-03-01&to=2026-03-31"
	for name, c := range map[string]struct{ query, field string }{
		"another process": {"&process=order_to_cash", "process"},
		"another format":  {"&process=purchase_to_pay&format=xes", "format"},
		"ending earlier":  {"&process=purchase_to_pay&to=2026-02-01", "to"},
		"too long":        {"to=2027-03-07", "to"},
	} {
		t.Run(name, func(t *testing.T) {
			path := base + c.query
			if c.field == "to" {
				to := strings.TrimPrefix(c.query, "&process=purchase_to_pay&to=")
				to = strings.TrimPrefix(to, "to=")
				path = "/v1/process-event-logs?process=purchase_to_pay&legal_entity_id=" + e.DE.String() + "&from=2026-03-01&to=" + to
			}
			if p := e.In(t).Problem(422, "request.validation_failed", "GET", path, "", ""); !strings.Contains(fmt.Sprint(p["errors"]), c.field) {
				t.Errorf("got %v, want %s named", p, c.field)
			}
		})
	}
	e.Problem(400, "request.invalid_page_token", "GET", base+"&process=purchase_to_pay&page_token=nonsense", "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/process-event-logs?process=purchase_to_pay&legal_entity_id="+uuid.Must(uuid.NewV7()).String()+
		"&from=2026-03-01&to=2026-03-31", "", "")
	reader := e.As(e.Reader(t, "Rita Reader", e.US))
	reader.Problem(404, "request.not_found", "GET", base+"&process=purchase_to_pay", "", "")
	if got := read(t, reader, "legal_entity_id="+e.US.String()+"&from=2026-03-01&to=2026-03-31"); got["case_count"] != float64(0) {
		t.Errorf("an empty log: got %v", got)
	}
}
