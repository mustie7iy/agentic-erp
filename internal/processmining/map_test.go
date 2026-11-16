package processmining_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/processmining"
)

// readMap reads a map through the API as the client and returns its body
// as JSON.
func readMap(t *testing.T, c apitest.Client, query string) map[string]any {
	t.Helper()
	got, _ := c.Must(200, "GET", "/v1/process-maps?process=purchase_to_pay&"+query, "", "")
	return got
}

// rows are a map's list as lines of the fields named, in its order.
func rows(t *testing.T, m map[string]any, list string, fields ...string) string {
	t.Helper()
	var out []string
	for _, x := range apitest.List(t, m, list) {
		var parts []string
		for _, f := range fields {
			parts = append(parts, fmt.Sprint(x.(map[string]any)[f]))
		}
		out = append(out, strings.Join(parts, " "))
	}
	return strings.Join(out, "; ")
}

// TestProcessMap: three orders run the same activities, a minute apart
// but for the update between release and receipt; a fourth was not paid,
// so it is open in a variant of its own; a document on no order without
// events is open in the empty variant; an order of another company is not
// in the map (T103b acceptance: counts add up, every case in exactly one
// variant, pairs counted where the second directly follows the first).
func TestProcessMap(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 4, "2026-03-02", 3, 1_000_000)
	flows(t, e, e.US, 5, 1, "2026-03-02", 3, 1_000_100)
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica; DELETE FROM audit.audit_event WHERE seq = 1000049; END $$`)
	account := e.Account(t, "2000", "520000")
	e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "parked", "EUR", "2026-03-05", fixture.InvoiceItem{Net: 100, Account: &account})
	got := readMap(t, e.Client, "legal_entity_id="+e.DE.String()+"&from=2026-03-01&to=2026-03-31")
	for name, c := range map[string]struct{ got, want string }{
		"the map": {fmt.Sprint(got["process"], " ", got["company_code"], " ", got["from"], " ", got["to"], " ", got["case_count"], " ", got["open_case_count"], " ",
			got["variant_count"]), "purchase_to_pay 2000 2026-03-01 2026-03-31 5 2 3"},
		"the activities": {rows(t, got, "activities", "activity", "event_count", "case_count"), "Approve requisition 4 4; Create purchase order 4 4; " +
			"Create requisition 4 4; Enter invoice 4 4; Post invoice 4 4; Receive goods 4 4; Release purchase order 4 4; Pay invoice 3 3"},
		"the pairs": {rows(t, got, "pairs", "from", "to", "count", "median_seconds"), "Approve requisition Create purchase order 4 60; " +
			"Create purchase order Release purchase order 4 60; Create requisition Approve requisition 4 60; Enter invoice Post invoice 4 60; " +
			"Receive goods Enter invoice 4 60; Release purchase order Receive goods 4 120; Post invoice Pay invoice 3 60"},
		"the variants": {rows(t, got, "variants", "case_count", "open_case_count", "share_percent"), "3 0 60.00; 1 1 20.00; 1 1 20.00"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %s, want %s", c.got, c.want)
			}
		})
	}
	variants := apitest.List(t, got, "variants")
	paid := "[Create requisition Approve requisition Create purchase order Release purchase order Receive goods Enter invoice Post invoice Pay invoice]"
	if s := fmt.Sprint(variants[0].(map[string]any)["activities"]); s != paid {
		t.Errorf("the first variant: got %s, want %s", s, paid)
	}
	if s := fmt.Sprint(variants[1].(map[string]any)["activities"]); s != "[]" {
		t.Errorf("the second variant, the document's without events: got %s, want none", s)
	}
	if s := fmt.Sprint(variants[2].(map[string]any)["activities"]); s != strings.TrimSuffix(paid, " Pay invoice]")+"]" {
		t.Errorf("the third variant, the order not paid: got %s", s)
	}
}

// TestProcessMapMedian: the median of a pair is of whole seconds, the
// lower middle of an even count; a case ended by its kind's last activity
// is closed, a document on no order reversed among them.
func TestProcessMapMedian(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 4, "2026-03-02", 3, 1_000_000)
	// The payments come 60.9, 240, 120 and 180 seconds after the postings.
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event p SET occurred_at = i.occurred_at + d.after FROM audit.audit_event i,
			(VALUES (1, interval '60.9 seconds'), (2, interval '240 seconds'), (3, interval '120 seconds'), (4, interval '180 seconds')) d(n, after)
		WHERE p.seq = 1000000 + d.n * 10 + 9 AND i.seq = 1000000 + d.n * 10 + 8; END $$`)
	got := readMap(t, e.Client, "legal_entity_id="+e.DE.String()+"&from=2026-03-01&to=2026-03-31")
	if s := rows(t, got, "pairs", "from", "to", "count", "median_seconds"); !strings.Contains(s, "Post invoice Pay invoice 4 120") {
		t.Errorf("the pairs: got %s, want the payment's median 120 s, the lower middle of 60, 120, 180 and 240", s)
	}
	if got["open_case_count"] != float64(0) {
		t.Errorf("open cases: got %v, want none: each was paid", got["open_case_count"])
	}
}

// TestProcessMapChunks: a map reads every case, chunk after chunk, the
// orders and the documents on no order alike, and a document reversed
// ends its case.
func TestProcessMapChunks(t *testing.T) {
	defer processmining.SetMapChunk(2)()
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 5, "2026-03-02", 3, 1_000_000)
	account := e.Account(t, "2000", "520000")
	alone := e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "parked", "EUR", "2026-03-03", fixture.InvoiceItem{Net: 100, Account: &account})
	pgtest.Exec(t, `INSERT INTO audit.audit_event (tenant_id, occurred_at, seq, actor_id, actor_kind, action, object_type, object_id, outcome, hash)
		VALUES ($1, '2026-03-04 08:00Z', 2000000, $2, 'human', 'payables.supplier_invoice.reverse', 'payables.supplier_invoice', $3, 'success', sha256('r'::bytea))`,
		e.Tenant.ID, e.Tenant.Human.ID, alone)
	got := readMap(t, e.Client, "legal_entity_id="+e.DE.String()+"&from=2026-03-01&to=2026-03-31")
	if s := fmt.Sprint(got["case_count"], " ", got["open_case_count"], " ", rows(t, got, "variants", "case_count", "open_case_count", "share_percent")); s != "6 0 5 0 83.33; 1 0 16.67" {
		t.Errorf("the map: got %s, want 6 cases, none open, five of one variant and the reversed document's", s)
	}
}

// TestProcessMapRefused: another process, a period that ends before it
// starts or lasts too long are refused; another company's map, or one
// the caller cannot act in, is not found; a company without cases has an
// empty map.
func TestProcessMapRefused(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	path := func(process, le, to string) string {
		return "/v1/process-maps?process=" + process + "&legal_entity_id=" + le + "&from=2026-03-01&to=" + to
	}
	for name, c := range map[string]struct{ path, field string }{
		"another process": {path("order_to_cash", e.DE.String(), "2026-03-31"), "process"},
		"ending earlier":  {path("purchase_to_pay", e.DE.String(), "2026-02-01"), "to"},
		"too long":        {path("purchase_to_pay", e.DE.String(), "2027-03-07"), "to"},
	} {
		t.Run(name, func(t *testing.T) {
			if p := e.In(t).Problem(422, "request.validation_failed", "GET", c.path, "", ""); !strings.Contains(fmt.Sprint(p["errors"]), c.field) {
				t.Errorf("got %v, want %s named", p, c.field)
			}
		})
	}
	e.Problem(404, "request.not_found", "GET", path("purchase_to_pay", uuid.Must(uuid.NewV7()).String(), "2026-03-31"), "", "")
	reader := e.As(e.Reader(t, "Rita Reader", e.US))
	reader.Problem(404, "request.not_found", "GET", path("purchase_to_pay", e.DE.String(), "2026-03-31"), "", "")
	got := readMap(t, reader, "legal_entity_id="+e.US.String()+"&from=2026-03-01&to=2027-03-06")
	if s := fmt.Sprint(got["case_count"], got["open_case_count"], got["variant_count"], got["activities"], got["pairs"], got["variants"]); s != "0 0 0 [] [] []" {
		t.Errorf("an empty map over 370 days: got %s", s)
	}
}

// TestProcessMapDatabaseFailures reads a map outside a unit of work and
// with each of its database statements failing in turn.
func TestProcessMapDatabaseFailures(t *testing.T) {
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 2, "2026-03-02", 3, 1_000_000)
	ep := app.ProcessMining(app.Config{})
	from, _ := time.Parse(time.DateOnly, "2026-03-01") // the constants parse
	to, _ := time.Parse(time.DateOnly, "2026-03-31")
	call := func(ctx context.Context) error {
		_, err := ep.GetProcessMap(ctx, gen.GetProcessMapParams{Process: "purchase_to_pay", LegalEntityID: e.DE, From: from, To: to})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "a map read", call)
}
