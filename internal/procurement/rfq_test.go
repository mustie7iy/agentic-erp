package procurement_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Requests for quotation (T82a, ADR-0069): the tests' clock is 15 June
// 2026 (setupIn2026), so an RFQ due on or after that day is accepted.

// rfqItem is an RFQ item body for a material key.
func rfqItem(key, qty, unit, account, extra string) string {
	return fmt.Sprintf(`{"material_key": %q, "description": "Item %s", "quantity": {"value": %q, "unit": %q}, "account_id": %q%s}`,
		key, key, qty, unit, account, extra)
}

// rfqBody is an RFQ of company 2000 in EUR for the items and suppliers.
func (e env) rfqBody(due, its string, suppliers ...uuid.UUID) string {
	ids := make([]string, len(suppliers))
	for i, s := range suppliers {
		ids[i] = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "due_date": %q, "items": %s, "supplier_ids": [%s]}`, e.DE, due, its, strings.Join(ids, ", "))
}

// chairsAndDesks are two items on the office account.
func (e env) chairsAndDesks() string {
	return items(rfqItem("CHAIR", "40", "EA", e.office, ""), rfqItem("DESK", "12.5", "EA", e.office, ""))
}

// rfq creates a draft RFQ due on the day for chairs and desks, inviting
// the fixture's two suppliers.
func (e env) rfq(t *testing.T, due string) map[string]any {
	t.Helper()
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody(due, e.chairsAndDesks(), e.Supplier, e.Vendor), "")
	return r
}

// invited are the suppliers of an RFQ as "id number name", in their order.
func invited(t *testing.T, r map[string]any) string {
	t.Helper()
	var out []string
	for _, s := range apitest.List(t, r, "suppliers") {
		m := s.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v", m["supplier_id"], m["supplier_number"], m["supplier_name"]))
	}
	return strings.Join(out, ", ")
}

// nameSuppliers gives the fixture's suppliers names that differ from their
// numbers (the fixture names a partner by its number), so a test can tell
// a name from a number and see that no event carries a name.
func (e env) nameSuppliers(t *testing.T) {
	t.Helper()
	for id, name := range map[uuid.UUID]string{e.Supplier: tyrell, e.Vendor: cabot} {
		pgtest.Exec(t, `UPDATE partner.business_partner SET name = $2, version = version + 1 WHERE id = $1`, id, name)
	}
}

const tyrell, cabot = "Tyrell Office Products", "Cabot Components"

// audits counts the tenant's audit records of the action on the object.
func audits(t *testing.T, e env, action, object string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = $2 AND object_id = $3 AND outcome = 'succeeded'`,
		[]any{e.Tenant.ID, action, object}, &n)
	return n
}

func TestRFQLifecycle(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	// The suppliers are invited in the order named, not the order of their ids or numbers.
	r, tag := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-07-31", e.chairsAndDesks(), e.Vendor, e.Supplier), "")
	path := "/v1/rfqs/" + id(r)
	want := fmt.Sprintf("%s BP90000002 %s, %s BP90000001 %s", e.Vendor, cabot, e.Supplier, tyrell)
	checks := func(t *testing.T, what string, got map[string]any, status string, version float64) {
		t.Helper()
		for field, want := range map[string]any{"document_number": "RQ100000", "status": status, "version": version, "currency": "EUR", "due_date": "2026-07-31",
			"legal_entity_id": e.DE.String(), "company_code": "2000"} {
			if got[field] != want {
				t.Errorf("%s %s: got %v, want %v", what, field, got[field], want)
			}
		}
		if got := invited(t, got); got != want {
			t.Errorf("%s suppliers: got %s, want %s", what, got, want)
		}
		its := apitest.List(t, got, "items")
		if len(its) != 2 {
			t.Fatalf("%s items: got %d, want 2", what, len(its))
		}
		desk := its[1].(map[string]any)
		if desk["item_no"] != float64(2) || desk["material_key"] != "DESK" || desk["account_id"] != e.office {
			t.Errorf("%s item 2: got %v, want item 2, DESK on the office account", what, desk)
		}
		if desk["account_number"] != "520000" {
			t.Errorf("%s item 2 account_number: got %v, want 520000", what, desk["account_number"])
		}
		apitest.WantNamed(t, what+" company", got, "legal_entity_id", "company_name", `SELECT name FROM tenancy.legal_entity WHERE id = $1`)
		apitest.WantNamed(t, what+" item 2 account", desk, "account_id", "account_name", `SELECT name FROM ledger.gl_account WHERE id = $1`)
		if q := desk["quantity"].(map[string]any); q["value"] != "12.5" || q["unit"] != "EA" {
			t.Errorf("%s item 2 quantity: got %v, want 12.5 EA", what, q)
		}
	}
	checks(t, "created", r, "draft", 1)
	if tag != `"1"` {
		t.Errorf("created etag: got %s, want \"1\"", tag)
	}
	got, _ := e.Must(200, "GET", path, "", "")
	checks(t, "read", got, "draft", 1)
	e.Problem(412, "request.version_mismatch", "POST", path+":publish", "", `"7"`)
	pub, tag := e.Must(200, "POST", path+":publish", "", `"1"`)
	checks(t, "published", pub, "published", 2)
	if tag != `"2"` {
		t.Errorf("published etag: got %s, want \"2\"", tag)
	}
	got, _ = e.Must(200, "GET", path, "", "")
	checks(t, "read after publication", got, "published", 2)
	e.Problem(409, "procurement.invalid_status", "POST", path+":publish", "", `"2"`)

	changes := apitest.CompanyNamed(t, events(t, e, "rfq_changed"))
	if len(changes) != 2 {
		t.Fatalf("rfq_changed events: got %d, want 2", len(changes))
	}
	for i, want := range []map[string]any{{"change": "created", "status": "draft"}, {"change": "published", "status": "published"}} {
		for field, v := range want {
			if changes[i][field] != v {
				t.Errorf("rfq_changed %d %s: got %v, want %v", i, field, changes[i][field], v)
			}
		}
		if changes[i]["document_number"] != "RQ100000" || changes[i]["item_count"] != float64(2) || changes[i]["supplier_count"] != float64(2) {
			t.Errorf("rfq_changed %d: got %v, want RQ100000 with 2 items and 2 suppliers", i, changes[i])
		}
	}
	published := apitest.CompanyNamed(t, events(t, e, "rfq_published"))
	if len(published) != 1 {
		t.Fatalf("rfq_published events: got %d, want 1", len(published))
	}
	wantInvited := []any{map[string]any{"supplier_id": e.Vendor.String(), "supplier_number": "BP90000002"},
		map[string]any{"supplier_id": e.Supplier.String(), "supplier_number": "BP90000001"}}
	if got := published[0]["suppliers"]; !reflect.DeepEqual(got, wantInvited) {
		t.Errorf("rfq_published suppliers: got %v, want %v (ids and numbers, no names)", got, wantInvited)
	}
	// A supplier's name goes into no event (product owner 2026-11-05).
	for _, name := range []string{tyrell, cabot} {
		apitest.NeverPublished(t, e.Tenant, "procurement", name)
	}
	for _, action := range []string{"procurement.rfq.create", "procurement.rfq.publish"} {
		if got := audits(t, e, action, id(r)); got != 1 {
			t.Errorf("%s audit records: got %d, want 1", action, got)
		}
	}
}

// TestRFQRefusals: what an RFQ is refused for when it is created, each by
// the request's own field.
func TestRFQRefusals(t *testing.T) {
	e := setupIn2026(t)
	ok := e.chairsAndDesks()
	blocked := e.NewSupplier(t, "BP90000009")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, blocked)
	one := func(item string) string { return e.rfqBody("2026-07-31", items(item), e.Supplier) }
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
		field  string
	}{
		"no item":           {e.rfqBody("2026-07-31", "[]", e.Supplier), 400, "request.invalid_body", ""},
		"no supplier":       {e.rfqBody("2026-07-31", ok), 400, "request.invalid_body", ""},
		"a supplier twice":  {e.rfqBody("2026-07-31", ok, e.Supplier, e.Vendor, e.Supplier), 422, "procurement.rfq_supplier_repeated", "supplier_ids[2]"},
		"no such supplier":  {e.rfqBody("2026-07-31", ok, e.Supplier, uuid.New()), 422, "procurement.supplier_not_usable", "supplier_ids[1]"},
		"a blocked one":     {e.rfqBody("2026-07-31", ok, blocked, e.Supplier), 422, "procurement.supplier_not_usable", "supplier_ids[0]"},
		"due yesterday":     {e.rfqBody("2026-06-14", ok, e.Supplier), 422, "procurement.rfq_due_date_past", "due_date"},
		"a key twice":       {e.rfqBody("2026-07-31", items(rfqItem("A", "1", "EA", e.office, ""), rfqItem("A", "2", "EA", e.office, "")), e.Supplier), 422, "procurement.duplicate_material_key", "items[1].material_key"},
		"a zero quantity":   {one(rfqItem("A", "0", "EA", e.office, "")), 422, "procurement.invalid_quantity", "items[0].quantity.value"},
		"a blank text":      {one(strings.Replace(rfqItem("A", "1", "EA", e.office, ""), "Item A", " ", 1)), 422, "request.validation_failed", "items[0].description"},
		"another company's": {one(rfqItem("A", "1", "EA", e.Account(t, "1000", "520000").String(), "")), 422, "procurement.account_not_postable", "items[0].account_id"},
		"an unknown unit":   {one(rfqItem("A", "1", "NOUNIT", e.office, "")), 422, "procurement.unknown_unit", "items"},
		"a wrong item no":   {one(rfqItem("A", "1", "EA", e.office, `, "item_no": 2`)), 422, "procurement.invalid_item_no", "items[0].item_no"},
		"a price":           {one(rfqItem("A", "1", "EA", e.office, `, "unit_price": {"amount": "1.00", "currency": "EUR"}`)), 400, "request.invalid_body", ""},
		"an unknown currency": {strings.Replace(e.rfqBody("2026-07-31", ok, e.Supplier), `"EUR"`, `"XXA"`, 1), 422,
			"procurement.unknown_currency", "currency"},
		"an unknown company": {strings.Replace(e.rfqBody("2026-07-31", ok, e.Supplier), e.DE.String(), uuid.NewString(), 1), 422,
			"request.reference_not_found", "legal_entity_id"},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(c.status, c.code, "POST", "/v1/rfqs", c.body, "")
			if c.field == "" {
				return
			}
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 || errs[0].(map[string]any)["field"] != c.field {
				t.Errorf("field errors: got %v, want one on %s", p["errors"], c.field)
			}
		})
	}
	t.Run("the refusal names the day and the zone", func(t *testing.T) {
		p := e.in(t).Problem(422, "procurement.rfq_due_date_past", "POST", "/v1/rfqs", e.rfqBody("2026-06-14", ok, e.Supplier), "")
		if detail, _ := p["detail"].(string); !strings.Contains(detail, "2026-06-15 in UTC") {
			t.Errorf("detail: got %q, want it to name 2026-06-15 in UTC", detail)
		}
	})
	t.Run("due today", func(t *testing.T) {
		e.in(t).Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-06-15", ok, e.Supplier), "")
	})
	if n := len(events(t, e, "rfq_changed")); n != 1 {
		t.Errorf("rfq_changed events after the refusals: got %d, want 1 (the RFQ due today)", n)
	}
}

// TestRFQDueDateTimeZone: the due date is judged on the company's date in
// its time zone, not UTC's. At 23:00 on 30 June in Toronto (03:00 UTC on
// 1 July) an RFQ due on 30 June is accepted; at 01:00 on 1 July in Dubai
// (21:00 UTC on 30 June) it is refused, naming 1 July in Dubai.
func TestRFQDueDateTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone   string
		at     time.Time
		status int
		names  string
	}{
		{"America/Toronto", time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC), 201, ""},
		{"Asia/Dubai", time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC), 422, "2026-07-01 in Asia/Dubai"},
	} {
		t.Run(c.zone, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone }, perms...)
			f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			e := env{Env: f, office: f.Account(t, "2000", "520000").String()}
			status, got, _ := e.Do("POST", "/v1/rfqs", e.rfqBody("2026-06-30", e.chairsAndDesks(), e.Supplier), "")
			if status != c.status {
				t.Fatalf("an RFQ due 2026-06-30: got %d %v, want %d", status, got, c.status)
			}
			if detail, _ := got["detail"].(string); c.names != "" && !strings.Contains(detail, c.names) {
				t.Errorf("detail: got %q, want it to name %s", detail, c.names)
			}
		})
	}
}

// TestRFQPublishChecksAgain: publishing checks the RFQ as creating it did.
// A supplier blocked since the draft refuses it by that supplier's place,
// and a due date that passed since refuses it; the RFQ stays a draft.
func TestRFQPublishChecksAgain(t *testing.T) {
	e := setupIn2026(t)
	r := e.rfq(t, "2026-06-20")
	path := "/v1/rfqs/" + id(r)
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Vendor)
	p := e.Problem(422, "procurement.supplier_not_usable", "POST", path+":publish", "", `"1"`)
	if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "suppliers[1].supplier_id" {
		t.Errorf("field errors: got %v, want one on suppliers[1].supplier_id", p["errors"])
	}
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = false, version = version + 1 WHERE partner_id = $1`, e.Vendor)
	e.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Now = func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }
	})
	p = e.Problem(422, "procurement.rfq_due_date_past", "POST", path+":publish", "", `"1"`)
	if detail, _ := p["detail"].(string); !strings.Contains(detail, "2026-06-21 in UTC") {
		t.Errorf("detail: got %q, want it to name 2026-06-21 in UTC", detail)
	}
	if got, _ := e.Must(200, "GET", path, "", ""); got["status"] != "draft" || got["version"] != float64(1) {
		t.Errorf("after the refusals: got status %v version %v, want draft at version 1", got["status"], got["version"])
	}
	if n := len(events(t, e, "rfq_published")); n != 0 {
		t.Errorf("rfq_published events: got %d, want 0", n)
	}
}

// TestRFQPublishedOnce: two publications at the version both read
// serialize on the RFQ's row lock; the second finds the version moved and
// is refused, and one publication is published.
func TestRFQPublishedOnce(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	r := uuidOf(e.rfq(t, "2026-07-31"))
	publish := func(ctx context.Context) error {
		_, err := h.PublishRfq(ctx, gen.PublishRfqParams{ID: r, IfMatch: `"1"`})
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, publish, publish)
	if first != nil {
		t.Fatalf("first publication: %v", first)
	}
	if got := problem.From(second).Code; got != "request.version_mismatch" {
		t.Errorf("second publication: got %s, want request.version_mismatch", got)
	}
	if n := len(events(t, e, "rfq_published")); n != 1 {
		t.Errorf("rfq_published events: got %d, want 1", n)
	}
}

// TestRFQReplayed: the same request with its Idempotency-Key again answers
// what it answered and creates nothing more. The key is kept by the wall
// clock, so the test runs on it, with a due date no run reaches.
func TestRFQReplayed(t *testing.T) {
	e := setup(t)
	req := apitest.Req{Method: http.MethodPost, Path: "/v1/rfqs", Body: e.rfqBody("2999-12-31", e.chairsAndDesks(), e.Supplier, e.Vendor),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, again := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || again.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice", first.Code, again.Code)
	}
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", again.Header().Get("Idempotent-Replayed"))
	}
	if got, want := apitest.JSON(t, again), apitest.JSON(t, first); !reflect.DeepEqual(got, want) {
		t.Errorf("replayed body: got %v, want %v", got, want)
	}
	if page, _ := e.Must(200, "GET", "/v1/rfqs", "", ""); len(apitest.List(t, page, "items")) != 1 {
		t.Errorf("RFQs after the replay: got %v, want one", page["items"])
	}
}

// TestRFQList: the list's filters and its pages in both orders, each RFQ
// of a page with its own company, accounts and suppliers named; a caller
// sees the RFQs of the companies it may act in, and another company's is
// answered as missing.
func TestRFQList(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	first := e.rfq(t, "2026-09-30") // RQ100000, the only one inviting both suppliers
	one := fmt.Sprintf("%s BP90000001 %s", e.Supplier, tyrell)
	both := one + fmt.Sprintf(", %s BP90000002 %s", e.Vendor, cabot)
	second, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-07-31", e.chairsAndDesks(), e.Supplier), "")
	third, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-08-31", e.chairsAndDesks(), e.Supplier), "")
	e.act(t, "/v1/rfqs/"+id(third), "publish", "")
	list := func(t *testing.T, c apitest.Client, q string) ([]string, string) {
		t.Helper()
		page, _ := c.In(t).Must(200, "GET", "/v1/rfqs?"+q, "", "")
		var numbers []string
		for _, r := range apitest.List(t, page, "items") {
			m := r.(map[string]any)
			want := one
			if m["document_number"] == "RQ100000" {
				want = both
			}
			if got := invited(t, m); got != want {
				t.Errorf("RFQ %v suppliers: got %s, want %s", m["document_number"], got, want)
			}
			if m["company_code"] != "2000" {
				t.Errorf("RFQ %v company_code: got %v, want 2000", m["document_number"], m["company_code"])
			}
			apitest.WantNamed(t, "listed RFQ's company", m, "legal_entity_id", "company_name", `SELECT name FROM tenancy.legal_entity WHERE id = $1`)
			for _, it := range apitest.List(t, m, "items") {
				item := it.(map[string]any)
				if item["account_number"] != "520000" {
					t.Errorf("RFQ %v item %v account_number: got %v, want 520000", m["document_number"], item["item_no"], item["account_number"])
				}
				apitest.WantNamed(t, "listed RFQ's item's account", item, "account_id", "account_name", `SELECT name FROM ledger.gl_account WHERE id = $1`)
			}
			numbers = append(numbers, m["document_number"].(string))
		}
		tok, _ := page["next_page_token"].(string)
		return numbers, tok
	}
	for name, c := range map[string]struct{ query, want string }{
		"by due date":            {"", "RQ100001 RQ100002 RQ100000"},
		"by due date descending": {"order_by=due_date%20desc", "RQ100000 RQ100002 RQ100001"},
		"by number":              {"order_by=document_number", "RQ100000 RQ100001 RQ100002"},
		"of the company":         {"legal_entity_id=" + e.DE.String(), "RQ100001 RQ100002 RQ100000"},
		"of another company":     {"legal_entity_id=" + e.US.String(), ""},
		"a supplier's":           {"supplier_id=" + e.Vendor.String(), "RQ100000"},
		"drafts":                 {"status=draft", "RQ100001 RQ100000"},
		"published":              {"status=published", "RQ100002"},
		"one number":             {"document_number=RQ100001", "RQ100001"},
		"filters together": {fmt.Sprintf("legal_entity_id=%s&supplier_id=%s&status=draft&document_number=RQ100001", e.DE, e.Supplier),
			"RQ100001"},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := list(t, e.Client, c.query)
			if got := strings.Join(got, " "); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	for _, order := range []string{"due_date", "document_number"} {
		t.Run("pages by "+order, func(t *testing.T) {
			page1, tok := list(t, e.Client, "page_size=2&order_by="+order)
			if tok == "" {
				t.Fatal("page 1: got no next page token, want one")
			}
			page2, tok := list(t, e.Client, "page_size=2&order_by="+order+"&page_token="+tok)
			if tok != "" {
				t.Errorf("page 2: got next page token %q, want none", tok)
			}
			if len(page1) != 2 || len(page2) != 1 {
				t.Errorf("pages: got %v and %v, want two RFQs and one", page1, page2)
			}
		})
	}
	t.Run("a caller of another company", func(t *testing.T) {
		other := e.As(e.Reader(t, "Reader of 1000", e.US))
		if got, _ := list(t, other, ""); len(got) != 0 {
			t.Errorf("list: got %v, want none", got)
		}
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/rfqs/"+id(first), "", "")
		other.In(t).Problem(404, "request.not_found", "POST", "/v1/rfqs/"+id(second)+":publish", "", `"1"`)
		other.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/rfqs", e.rfqBody("2026-07-31", e.chairsAndDesks(), e.Supplier), "")
	})
	t.Run("an order the list does not have", func(t *testing.T) {
		e.in(t).Problem(400, "request.invalid_order_by", "GET", "/v1/rfqs?order_by=status", "", "")
	})
	t.Run("no RFQ with the id", func(t *testing.T) {
		e.in(t).Problem(404, "request.not_found", "GET", "/v1/rfqs/"+uuid.NewString(), "", "")
	})
	t.Run("without the permissions", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.rfq.manage'`, e.Tenant.ID)
		e.in(t).Problem(403, "identity.permission_missing", "POST", "/v1/rfqs", e.rfqBody("2026-07-31", e.chairsAndDesks(), e.Supplier), "")
		e.in(t).Problem(403, "identity.permission_missing", "POST", "/v1/rfqs/"+id(second)+":publish", "", `"1"`)
		e.in(t).Must(200, "GET", "/v1/rfqs/"+id(second), "", "")
	})
}

// TestRFQDatabaseFailures calls every RFQ handler outside a unit of work
// and with each of its database statements failing in turn.
func TestRFQDatabaseFailures(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	draft := uuidOf(e.rfq(t, "2026-07-31"))
	for name, call := range map[string]func(ctx context.Context) error{
		"create RFQ": func(ctx context.Context) error {
			_, err := h.CreateRfq(ctx, &gen.RfqCreate{LegalEntityID: e.DE, Currency: "EUR", DueDate: dateOf("2026-07-31"), SupplierIds: []uuid.UUID{e.Supplier, e.Vendor},
				Items: []gen.RfqItemInput{{MaterialKey: "A", Description: "P", Quantity: gen.Quantity{Value: "1", Unit: "EA"}, AccountID: uuid.MustParse(e.office)}}},
				gen.CreateRfqParams{})
			return err
		},
		"get RFQ": func(ctx context.Context) error {
			_, err := h.GetRfq(ctx, gen.GetRfqParams{ID: draft})
			return err
		},
		"list RFQs": func(ctx context.Context) error {
			_, err := h.ListRfqs(ctx, gen.ListRfqsParams{})
			return err
		},
		"publish RFQ": func(ctx context.Context) error {
			_, err := h.PublishRfq(ctx, gen.PublishRfqParams{ID: draft, IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}
